package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Signing in with a password, an operator's second factor and setup, password
// changes and resets, re-authentication and device links (ADR-009 §5–6,
// design §7.5): `conch login --password|--link`, `conch setup`, `conch
// password set|change|reset`, `conch reauth` and `conch device-link`. The
// server does every check; a refusal is its own words, with a hint of what
// to do. Offline bootstrap and recovery are not here: they are host commands,
// `conchd operator bootstrap|setup|recover`, which print the setup token
// `conch setup` asks for.

// errLoginFailed is every sign-in the server refuses with auth_failed: one
// answer for each cause, as the server gives one.
var errLoginFailed = errors.New("login failed: the name, password or code is not right, or the account cannot sign in with a password yet (an account from before passwords needs one set; an operator's setup must be finished with 'conch setup')")

// isOperatorWho reports whether whoami describes an operator, by the
// identity class, or by the role from a server that predates classes.
func isOperatorWho(who schema.WhoAmIResponseV1) bool {
	if who.IdentityClass != "" {
		return who.IdentityClass == schema.IdentityOperator
	}
	return who.Role == schema.RoleOperator
}

// signedIn prints the line every sign-in ends with, and how long the sign-in
// lasts when it has bounds. The role comes from whoami with the new
// credential; without it the line names the principal alone.
func signedIn(ctx context.Context, server string, token string, cred *schema.CredentialV1, fallback string, stdout, stderr io.Writer) error {
	client, err := NewClient(server, nil)
	if err != nil {
		return err
	}
	if who, err := client.WithToken(token).WhoAmI(ctx); err == nil {
		_, err = fmt.Fprintf(stdout, "logged in to %s as %s (%s)\n", safeLine(client.Server()), safeLine(who.Name), safeLine(string(who.Role)))
		if err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(stdout, "logged in to %s as %s\n", safeLine(client.Server()), safeLine(fallback)); err != nil {
		return err
	}
	if cred == nil || cred.ExpiresAt == nil {
		return nil
	}
	line := "this sign-in lasts until " + clock(*cred.ExpiresAt)
	if cred.IdleExpiresAt != nil {
		line += ", and ends sooner if unused for " + span(int64(cred.IdleExpiresAt.Time().Sub(cred.CreatedAt.Time()).Round(time.Minute)/time.Second))
	}
	_, err = fmt.Fprintln(stderr, line)
	return err
}

// loginWithPassword is `conch login --password`: the name (from --name, or
// asked for), the password, and for an operator the authenticator code the
// first step's challenge asks for (POST /v1/login, then /v1/login/totp).
func loginWithPassword(ctx context.Context, server, name string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := requireSecureServer("login", server); err != nil {
		return err
	}
	client, err := NewClient(server, nil)
	if err != nil {
		return err
	}
	if err := refuseMemberOverGuest(client.Server()); err != nil {
		return err
	}
	p := newPrompter(ctx, stdin, stderr)
	if name == "" {
		if name, err = p.line("Name: ", "name"); err != nil {
			return err
		}
	}
	password, err := p.secret("Password: ", "password")
	if err != nil {
		return err
	}
	req := schema.LoginRequestV1{Name: name, Password: password}
	if req.Validate() != nil {
		return errors.New("cli: login: the name or password is malformed")
	}
	resp, err := client.Login(ctx, req)
	if err != nil {
		return loginRefused(err)
	}
	if resp.Validate() != nil {
		return errors.New("cli: login: the server's answer is not one this conch can read; update conch")
	}
	if resp.Next == schema.LoginNextTOTP {
		code, err := p.totpCode("Authenticator code: ")
		if err != nil {
			return err
		}
		if resp, err = client.LoginTOTP(ctx, schema.TOTPLoginRequestV1{Challenge: resp.Challenge, Code: code}); err != nil {
			return loginRefused(err)
		}
		if resp.Validate() != nil || resp.Next != schema.LoginNextSignedIn {
			return errors.New("cli: login: the server's answer is not one this conch can read; update conch")
		}
	}
	if !wellFormedToken(resp.Token) {
		return errors.New("cli: login: the server returned a credential in an unexpected form; it was not stored")
	}
	if err := SaveToken(client.Server(), resp.Token); err != nil {
		return err
	}
	return signedIn(ctx, client.Server(), resp.Token, resp.Credential, resp.Principal.Name, stdout, stderr)
}

// signInHint is how to sign in as name, for a person to copy: the command,
// with the name quoted for a shell, when the name is one a principal may
// have; otherwise, since a name the server sent could be anything, no
// command at all.
func signInHint(name string) string {
	if (schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}).Validate() != nil {
		return "sign in with your new name: conch login --password"
	}
	return "sign in with: conch login --password --name " + shellQuote(name)
}

// shellQuote quotes s for a POSIX shell: single quotes, which take
// everything literally, with each single quote in s closed, escaped and
// reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// loginRefused is a sign-in refusal in a person's words.
func loginRefused(err error) error {
	if IsServerCode(err, schema.ErrorCodeAuthFailed) {
		return errLoginFailed
	}
	return olderServer(err, "password sign-in")
}

// readSignInLinkCode asks for a link's code, or its whole link.
func readSignInLinkCode(p *prompter, prompt string) (string, error) {
	input, err := p.code(prompt, "link")
	if err != nil {
		return "", err
	}
	return codeFromInput(input, schema.SignInLinkCodePrefix, "a sign-in link or its code", schema.ValidateSignInLinkCode)
}

// linkRefused is a link the server does not take, in a person's words.
func linkRefused(err error) error {
	if IsServerCode(err, schema.ErrorCodeSignInLinkInvalid) {
		return errors.New("cli: that link does not work: it is wrong, used, closed or expired; ask for another")
	}
	return err
}

// loginWithLink is `conch login --link`: a sign-in or device link, its code
// asked for without echo (design §6.2). The preview says whose account it
// opens, and nothing is used until the person says yes (or gave --yes):
// links never sign in merely because they were opened.
func loginWithLink(ctx context.Context, server string, yes bool, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := requireSecureServer("login", server); err != nil {
		return err
	}
	client, err := NewClient(server, nil)
	if err != nil {
		return err
	}
	if err := refuseMemberOverGuest(client.Server()); err != nil {
		return err
	}
	p := newPrompter(ctx, stdin, stderr)
	code, err := readSignInLinkCode(p, "Sign-in link or code: ")
	if err != nil {
		return err
	}
	preview, err := client.PreviewSignInLink(ctx, schema.SignInLinkPreviewRequestV1{Code: code})
	if err != nil {
		return linkRefused(err)
	}
	switch preview.Purpose {
	case "", schema.SignInLinkPurposeSignIn, schema.SignInLinkPurposeDevice:
	case schema.SignInLinkPurposeReset:
		return errors.New("cli: login: this link resets a password; use it with 'conch password reset'")
	default:
		return errors.New("cli: login: this link is not for signing in")
	}
	question := fmt.Sprintf("Sign in to %s as %s?", safeLine(client.Server()), safeLine(preview.Name))
	if current, err := readCredentials(); err == nil && current[client.Server()].Token != "" {
		question += " This replaces the login stored for that server."
	}
	if !yes {
		if !p.interactive() {
			return fmt.Errorf("cli: login: the link opens the account %s; give --yes to use it", safeLine(preview.Name))
		}
		ok, err := p.confirm(question)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("cli: login: not signed in; the link was not used")
		}
	}
	resp, err := client.RedeemSignInLink(ctx, schema.RedeemSignInLinkRequestV1{Code: code})
	if err != nil {
		return linkRefused(err)
	}
	if !wellFormedToken(resp.Token) {
		return errors.New("cli: login: the server returned a credential in an unexpected form; it was not stored")
	}
	if err := SaveToken(client.Server(), resp.Token); err != nil {
		return err
	}
	return signedIn(ctx, client.Server(), resp.Token, &resp.Credential, resp.Principal.Name, stdout, stderr)
}

// runSetup is an operator's setup after a host command (`conchd operator
// bootstrap|setup|recover`) printed a setup token: the token is spent for a
// short-lived credential that reaches only setup (POST /v1/setup/redeem), a
// new password is set with it, and an authenticator app is offered or required
// as whoami's setup state says. The credential is used here and never stored:
// when this ends, whatever was left unfinished needs a new token from the host.
func runSetup(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	const command = "setup"
	if err := refuseSecretArgs(command, args); err != nil {
		return err
	}
	fs := quietFlags(command)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	pos, err := parseQuiet(fs, command, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("cli: setup takes no arguments; the setup token is asked for")
	}
	if err := requireSecureServer(command, *server); err != nil {
		return err
	}
	client, err := NewClient(*server, nil)
	if err != nil {
		return err
	}
	p := newPrompter(ctx, stdin, stderr)
	token, err := p.code("Setup token: ", "setup token")
	if err != nil {
		return err
	}
	if schema.ValidateSetupToken(token) != nil {
		return errors.New("cli: setup: that is not a setup token; the host prints one with 'conchd operator setup <id>'")
	}
	resp, err := client.RedeemSetupToken(ctx, schema.RedeemSetupTokenRequestV1{Token: token})
	switch {
	case IsServerCode(err, schema.ErrorCodeSetupTokenInvalid):
		return errors.New("cli: setup: the setup token does not work: it is wrong, used or expired (it lasts ten minutes); make another on the host with 'conchd operator setup <id>'")
	case err != nil:
		return olderServer(err, "operator setup")
	}
	if !wellFormedToken(resp.Token) || resp.Auth.Validate() != nil {
		return errors.New("cli: setup: the server's answer is not one this conch can read; update conch")
	}
	setup, err := NewClient(*server, nil)
	if err != nil {
		return err
	}
	setup.WithToken(resp.Token)
	name := safeLine(resp.Principal.Name)
	_, _ = fmt.Fprintf(stderr, "Setting up %s: a new password, then an authenticator app.\n", name)
	password, err := p.newPassword("New password: ")
	if err != nil {
		return err
	}
	if _, err := setup.SetPassword(ctx, schema.SetPasswordRequestV1{Password: password}); err != nil {
		return err
	}
	who, err := setup.WhoAmI(ctx)
	if err != nil {
		return err
	}
	if a := who.Auth; a != nil && a.SetupComplete && !a.TOTPEnrolled && a.Scope == schema.CredentialScopeSetup {
		enroll := false
		if p.interactive() {
			enroll, err = p.confirm("Set up an authenticator now?")
			if err != nil {
				return err
			}
		}
		if !enroll {
			if err := setup.RevokeCurrentCredential(ctx); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "setup complete for %s; %s --server %s\n", name, signInHint(resp.Principal.Name), safeLine(client.Server()))
			return err
		}
	}
	enrol, err := setup.EnrollTOTP(ctx)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Add this account to your authenticator app (manual entry):\n  account:   %s\n  issuer:    %s\n  secret:    %s\n  type:      time-based, %s, %d digits, every %d seconds\n  uri:       %s\n",
		safeLine(enrol.Account), safeLine(enrol.Issuer), safeLine(enrol.Secret), safeLine(enrol.Algorithm), enrol.Digits, enrol.PeriodSeconds, safeLine(enrol.URI)); err != nil {
		return err
	}
	// On a terminal a mistyped code may be tried again, as the app shows the
	// next one; from a script the one answer it gave is the answer.
	tries := 1
	if p.interactive() {
		tries = 3
	}
	for try := 1; ; try++ {
		code, err := p.totpCode("Code from the app: ")
		if err != nil {
			return err
		}
		err = setup.ConfirmTOTP(ctx, schema.ConfirmTOTPRequestV1{Code: code})
		if err == nil {
			break
		}
		if !IsServerCode(err, schema.ErrorCodeAuthFailed) {
			return err
		}
		if try >= tries {
			return errors.New("cli: setup: the code was not accepted, so the authenticator is not enrolled; check the app's clock and the secret, and start again with a new setup token")
		}
		_, _ = fmt.Fprintln(stderr, "That code was not accepted; wait for the next one and try again.")
	}
	_, err = fmt.Fprintf(stdout, "setup complete for %s; %s --server %s\n", name, signInHint(resp.Principal.Name), safeLine(client.Server()))
	return err
}

func runPassword(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: password requires a subcommand (set, change, reset)")
	}
	switch args[0] {
	case "set":
		return runPasswordSet(ctx, args[1:], stdin, stdout, stderr)
	case "change":
		return runPasswordChange(ctx, args[1:], stdin, stdout, stderr)
	case "reset":
		return runPasswordReset(ctx, args[1:], stdin, stdout, stderr)
	default:
		// Not repeated: what was typed there may be the password itself.
		return errors.New("cli: password: unknown subcommand (want set, change or reset)")
	}
}

// passwordClient parses a password command's flags and builds a client that
// carries the person's own credential.
func passwordClient(command string, args []string, switches ...string) (*Client, credentialSource, map[string]*bool, error) {
	if err := refuseSecretArgs(command, args, switches...); err != nil {
		return nil, sourceNone, nil, err
	}
	fs := quietFlags(command)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	// A switch asks for what it names; the value itself is never a flag's.
	flags := make(map[string]*bool, len(switches))
	for _, s := range switches {
		flags[s] = fs.Bool(s, false, "ask for the "+s)
	}
	pos, err := parseQuiet(fs, command, args)
	if err != nil {
		return nil, sourceNone, nil, err
	}
	if len(pos) != 0 {
		return nil, sourceNone, nil, fmt.Errorf("cli: %s takes no arguments; what it needs is asked for", command)
	}
	if err := requireSecureServer(command, *server); err != nil {
		return nil, sourceNone, nil, err
	}
	client, source, err := newSourcedClient(*server)
	if err != nil {
		return nil, sourceNone, nil, err
	}
	if source == sourceNone {
		return nil, sourceNone, nil, &UnauthenticatedError{Server: client.Server()}
	}
	return client, source, flags, nil
}

// passwordUpdated finishes a password set or change: the credential the
// request came with survives rotated, and the new one replaces it where it
// came from. A stored login is rewritten; CONCH_TOKEN cannot be, so the new
// token is printed alone on standard output for the caller to take.
func passwordUpdated(client *Client, source credentialSource, resp schema.PasswordUpdatedResponseV1, done string, stdout, stderr io.Writer) error {
	others := fmt.Sprintf("%d other sign-in(s) signed out", resp.Revoked)
	if resp.Token == "" {
		_, err := fmt.Fprintf(stdout, "%s; %s\n", done, others)
		return err
	}
	if !wellFormedToken(resp.Token) {
		return fmt.Errorf("cli: %s, but the server returned a credential in an unexpected form; sign in again with 'conch login --password'", done)
	}
	if source == sourceStored {
		if err := saveMemberToken(client.Server(), resp.Token); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "%s; %s; this sign-in was renewed and stored\n", done, others)
		return err
	}
	if _, err := fmt.Fprintln(stdout, resp.Token); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stderr, "%s; %s; CONCH_TOKEN no longer works: use the credential printed on standard output\n", done, others)
	return err
}

// runPasswordSet sets the caller's first password (POST /v1/password/mine).
// A member from before passwords gives --proof: an enrolment code an
// operator made for them, asked for without echo.
func runPasswordSet(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	const command = "password set"
	client, source, flags, err := passwordClient(command, args, "proof")
	if err != nil {
		return err
	}
	p := newPrompter(ctx, stdin, stderr)
	var proof string
	if *flags["proof"] {
		if proof, err = readSignInLinkCode(p, "Enrollment code: "); err != nil {
			return err
		}
	}
	password, err := p.newPassword("New password: ")
	if err != nil {
		return err
	}
	resp, err := client.SetPassword(ctx, schema.SetPasswordRequestV1{Password: password, Proof: proof})
	switch {
	case IsServerCode(err, schema.ErrorCodeSetupOnHost):
		return errors.New("cli: password set: an operator's password is set up from the host: run 'conchd operator setup <id>' there, then 'conch setup'")
	case IsServerCode(err, schema.ErrorCodeReauthRequired):
		return errors.New("cli: password set: setting a first password needs an enrollment code from an operator ('conch admin principal sign-in-link --purpose enrollment <id>'); run this again with --proof")
	case IsServerCode(err, schema.ErrorCodePasswordAlreadySet):
		return errors.New("cli: password set: you have a password already; change it with 'conch password change'")
	case err != nil:
		return linkRefused(olderServer(err, "passwords"))
	}
	return passwordUpdated(client, source, resp, "password set", stdout, stderr)
}

// runPasswordChange replaces the caller's password with the current one
// (PUT /v1/password/mine), and an operator's authenticator code when enrolled.
func runPasswordChange(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	const command = "password change"
	client, source, _, err := passwordClient(command, args)
	if err != nil {
		return err
	}
	who, err := client.WhoAmI(ctx)
	if err != nil {
		return err
	}
	p := newPrompter(ctx, stdin, stderr)
	current, err := p.secret("Current password: ", "current password")
	if err != nil {
		return err
	}
	password, err := p.newPassword("New password: ")
	if err != nil {
		return err
	}
	req := schema.ChangePasswordRequestV1{Current: current, Password: password}
	if isOperatorWho(who) && who.Auth != nil && who.Auth.TOTPEnrolled {
		if req.Code, err = p.totpCode("Authenticator code: "); err != nil {
			return err
		}
	}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("cli: %s: %w", command, err)
	}
	resp, err := client.ChangePassword(ctx, req)
	switch {
	case IsServerCode(err, schema.ErrorCodeAuthFailed):
		return errors.New("cli: password change: the current password or the code is not right; nothing was changed")
	case IsServerCode(err, schema.ErrorCodePasswordNotSet):
		return errors.New("cli: password change: you have no password yet: set one with 'conch password set --proof'")
	case err != nil:
		return olderServer(err, "passwords")
	}
	return passwordUpdated(client, source, resp, "password changed", stdout, stderr)
}

// runPasswordReset replaces a forgotten password with the reset link an
// operator made (POST /v1/password/reset). It signs nobody in: the person
// signs in afterwards with the new password.
func runPasswordReset(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	const command = "password reset"
	if err := refuseSecretArgs(command, args); err != nil {
		return err
	}
	fs := quietFlags(command)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	pos, err := parseQuiet(fs, command, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("cli: password reset takes no arguments; the reset link is asked for")
	}
	if err := requireSecureServer(command, *server); err != nil {
		return err
	}
	client, err := NewClient(*server, nil)
	if err != nil {
		return err
	}
	p := newPrompter(ctx, stdin, stderr)
	code, err := readSignInLinkCode(p, "Reset link or code: ")
	if err != nil {
		return err
	}
	preview, err := client.PreviewSignInLink(ctx, schema.SignInLinkPreviewRequestV1{Code: code})
	if err != nil {
		return linkRefused(err)
	}
	if preview.Purpose != schema.SignInLinkPurposeReset {
		return errors.New("cli: password reset: this link does not reset a password; a sign-in link is used with 'conch login --link'")
	}
	name := safeLine(preview.Name)
	_, _ = fmt.Fprintf(stderr, "Resetting the password of %s.\n", name)
	password, err := p.newPassword("New password: ")
	if err != nil {
		return err
	}
	if _, err := client.ResetPassword(ctx, schema.ResetPasswordRequestV1{Code: code, Password: password}); err != nil {
		return linkRefused(olderServer(err, "password resets"))
	}
	_, err = fmt.Fprintf(stdout, "password reset for %s; every sign-in of the account was signed out: %s\n", name, signInHint(preview.Name))
	return err
}

// reauthenticate proves the person's password again (POST /v1/reauth), and an
// operator's authenticator code when enrolled, for actions needing a recent proof.
//
// Nothing is asked before the transport is checked: anyone answering on a
// plain-http address could otherwise forge reauth_required and be sent the
// lasting password in clear.
func reauthenticate(ctx context.Context, client *Client, p *prompter, stderr io.Writer) (schema.ReauthResponseV1, error) {
	if err := requireSecureServer("reauth", client.Server()); err != nil {
		return schema.ReauthResponseV1{}, err
	}
	who, err := client.WhoAmI(ctx)
	if err != nil {
		return schema.ReauthResponseV1{}, err
	}
	needsCode := isOperatorWho(who) && who.Auth != nil && who.Auth.TOTPEnrolled
	what := "your password"
	if needsCode {
		what += " and an authenticator code"
	}
	_, _ = fmt.Fprintf(stderr, "This needs a recent proof that it is you: %s.\n", what)
	var req schema.ReauthRequestV1
	if req.Password, err = p.secret("Password: ", "password"); err != nil {
		return schema.ReauthResponseV1{}, err
	}
	if needsCode {
		if req.Code, err = p.totpCode("Authenticator code: "); err != nil {
			return schema.ReauthResponseV1{}, err
		}
	}
	if req.Validate() != nil {
		return schema.ReauthResponseV1{}, errors.New("cli: reauth: the password or code is malformed")
	}
	resp, err := client.Reauth(ctx, req)
	switch {
	case IsServerCode(err, schema.ErrorCodeAuthFailed):
		return schema.ReauthResponseV1{}, errors.New("cli: reauth: the password or the code is not right")
	case err != nil:
		return schema.ReauthResponseV1{}, olderServer(err, "re-authentication")
	}
	return resp, nil
}

// askToReauth makes client prove the password again, asking p, whenever a
// request answers reauth_required, and then repeat it. The proof itself goes
// through a copy of the client without the hook, so a server that answers
// the proof with reauth_required gets one question, not one after another.
func askToReauth(client *Client, p *prompter, stderr io.Writer) {
	client.reauth = func(ctx context.Context) error {
		plain := *client
		plain.reauth = nil
		_, err := reauthenticate(ctx, &plain, p, stderr)
		return err
	}
}

func runReauth(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	client, _, _, err := passwordClient("reauth", args)
	if err != nil {
		return err
	}
	resp, err := reauthenticate(ctx, client, newPrompter(ctx, stdin, stderr), stderr)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "re-authenticated until %s\n", clock(resp.Until))
	return err
}

func runDeviceLink(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: device-link requires a subcommand (create, list, close)")
	}
	switch args[0] {
	case "create":
		return runDeviceLinkCreate(ctx, args[1:], stdin, stdout, stderr)
	case "list", "close":
		return runDeviceLinkRead(ctx, args[0], args[1:], stdout)
	default:
		// Not repeated: what was typed there may be a link's code.
		return errors.New("cli: device-link: unknown subcommand (want create, list or close)")
	}
}

// runDeviceLinkCreate makes a one-use link that signs the caller's own
// account in on another device (POST /v1/sign-in-links/mine), after a recent
// proof of the password, asked for when the server wants one. The link is
// printed alone on standard output; its code nowhere else.
func runDeviceLinkCreate(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	const command = "device-link create"
	if err := refuseSecretArgs(command, args); err != nil {
		return err
	}
	fs := quietFlags(command)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	label := fs.String("label", "", "a name for the device, which its sign-in carries")
	expires := fs.String("expires", "", "when it stops working: a duration such as 10m, or an RFC 3339 time (default 10m, at most 1h)")
	pos, err := parseQuiet(fs, command, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("cli: device-link create takes no arguments")
	}
	now := time.Now()
	req := schema.CreateDeviceLinkRequestV1{Label: *label}
	if *expires != "" {
		exp, err := parseInviteExpiry(*expires, now)
		if err != nil {
			return fmt.Errorf("cli: %s: %w", command, err)
		}
		ts := schema.NewTimestamp(exp)
		req.ExpiresAt = &ts
	}
	if err := req.Validate(now); err != nil {
		return fmt.Errorf("cli: %s: %w", command, err)
	}
	if err := requireSecureServer(command, *server); err != nil {
		return err
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	askToReauth(client, newPrompter(ctx, stdin, stderr), stderr)
	resp, err := client.CreateDeviceLink(ctx, req)
	if err != nil {
		return olderServer(err, "device links")
	}
	// The code is the one server string printed raw, so it is checked first.
	if schema.ValidateSignInLinkCode(resp.Code) != nil {
		return fmt.Errorf("cli: %s: the server returned a code in an unexpected form; it was not printed", command)
	}
	if _, err := fmt.Fprintln(stdout, client.resolve("app").String()+"/#/signin/"+resp.Code); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stderr, "device link %d, one use, expires %s: it signs in as you, so open it only on your own device (or use it there with 'conch login --link')\n",
		resp.SignInLink.ID, formatTimestamp(resp.SignInLink.ExpiresAt))
	return err
}

// runDeviceLinkRead lists the caller's open device links, or closes one.
func runDeviceLinkRead(ctx context.Context, verb string, args []string, stdout io.Writer) error {
	command := "device-link " + verb
	fs := quietFlags(command)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	asJSON := fs.Bool("json", false, "print the server's document")
	pos, err := parseQuiet(fs, command, args)
	if err != nil {
		return err
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	if verb == "close" {
		if len(pos) != 1 {
			return errors.New("cli: device-link close: expected <id>")
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil || id <= 0 {
			return errors.New("cli: device-link close: id must be a positive integer")
		}
		if err := client.CloseOwnDeviceLink(ctx, id); err != nil {
			return olderServer(err, "device links")
		}
		if *asJSON {
			return nil
		}
		_, err = fmt.Fprintf(stdout, "closed device link %d\n", id)
		return err
	}
	if len(pos) != 0 {
		return errors.New("cli: device-link list takes no arguments")
	}
	resp, err := client.ListOwnDeviceLinks(ctx)
	if err != nil {
		return olderServer(err, "device links")
	}
	if *asJSON {
		if resp.SignInLinks == nil {
			resp.SignInLinks = []schema.SignInLinkV1{}
		}
		return json.NewEncoder(stdout).Encode(resp)
	}
	for _, l := range resp.SignInLinks {
		if _, err := fmt.Fprintf(stdout, "%d created %s expires %s %s\n", l.ID, formatTimestamp(l.CreatedAt), formatTimestamp(l.ExpiresAt), dashIfEmpty(safeLine(l.Label))); err != nil {
			return err
		}
	}
	return nil
}
