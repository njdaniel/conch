package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Joining a server, guest visits and creating an account (ADR-009, design
// §3 and §7.5): `conch join info|account`, `conch account create` and
// `conch end-visit`. Each is the public route the browser uses; the server
// decides every policy, and nothing here grants or refuses anything itself.
//
// Guests are removed for now (issue #1005): no route starts a visit, so this
// client offers none (`conch join guest` is gone). A visit already stored
// here can still be ended (`conch end-visit`) or made an account (`conch
// account create`).

// olderServer turns a refusal of a route the server does not have into what
// it means: the server is older than this client. It fails closed: nothing
// is assumed about what an older server would have done.
func olderServer(err error, feature string) error {
	if routeMissing(err) {
		return fmt.Errorf("cli: this server does not offer %s: it is older than this conch", feature)
	}
	return err
}

func runJoin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: join requires a subcommand (info, account)")
	}
	switch args[0] {
	case "info":
		return runJoinInfo(ctx, args[1:], stdout)
	case "account":
		return runJoinAccount(ctx, args[1:], stdin, stdout, stderr)
	default:
		// Not repeated: what was typed there may be an invite's code.
		return errors.New("cli: join: unknown subcommand (want info or account)")
	}
}

// yesNo is a boolean as a person reads it.
func yesNo(b bool) string { return either(b, "yes", "no") }

// runJoinInfo prints what the server's front door offers now, from GET
// /v1/join: one "key: value" line each.
func runJoinInfo(ctx context.Context, args []string, stdout io.Writer) error {
	fs := quietFlags("join info")
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	asJSON := fs.Bool("json", false, "print the server's document")
	pos, err := parseQuiet(fs, "join info", args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("cli: join info takes no arguments")
	}
	client, err := NewClient(*server, nil)
	if err != nil {
		return err
	}
	info, err := client.JoinInfo(ctx)
	if err != nil {
		return olderServer(err, "joining")
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(info)
	}
	account := yesNo(info.Join.Account)
	if info.Join.Paused {
		account += " (joining is paused)"
	}
	_, err = fmt.Fprintf(stdout, "server: %s\ncreate an account: %s\npassword sign-in: %s\nvoice for joiners: %s\n",
		safeLine(info.ServerName), account, yesNo(info.PasswordLogin), yesNo(info.Voice))
	return err
}

// readInviteCode asks for an invite's code, or its whole link.
func readInviteCode(p *prompter) (string, error) {
	input, err := p.code("Invite link or code: ", "invite code")
	if err != nil {
		return "", err
	}
	return codeFromInput(input, schema.InviteCodePrefix, "an invite link or code", schema.ValidateInviteCode)
}

// channelNames lists the channels a join answer admitted to, for one line.
func channelNames(channels []schema.InviteChannelV1) string {
	names := make([]string, len(channels))
	for i, c := range channels {
		names[i] = safeLine(c.Name)
	}
	return dashIfEmpty(strings.Join(names, ", "))
}

// checkPermanentName is the check the server makes first on a name for a
// lasting account, so a name it would refuse costs no password typed.
func checkPermanentName(command, name string) error {
	if err := (schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}).Validate(); err != nil {
		return fmt.Errorf("cli: %s: %w", command, err)
	}
	if schema.IsReservedGuestName(name) {
		return fmt.Errorf("cli: %s: a name beginning with %q is kept for guests; choose another", command, schema.ReservedGuestNameKeyPrefix)
	}
	return nil
}

// runJoinAccount creates an account with a name and a password: from the
// bare address (POST /v1/join/account), or with --invite through an invite
// (POST /v2/invites/redeem; the v1 route makes no account now). It signs in
// as the new account.
func runJoinAccount(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	const command = "join account"
	if err := refuseSecretArgs(command, args, "invite"); err != nil {
		return err
	}
	fs := quietFlags(command)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	viaInvite := fs.Bool("invite", false, "join through an invite; its link or code is asked for")
	pos, err := parseQuiet(fs, command, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("cli: join account: usage: conch join account [--server <url>] [--invite] <name> (the password is asked for)")
	}
	name := pos[0]
	if err := checkPermanentName(command, name); err != nil {
		return err
	}
	if err := requireSecureServer(command, *server); err != nil {
		return err
	}
	client, err := NewClient(*server, nil)
	if err != nil {
		return err
	}
	if err := refuseMemberOverGuest(client.Server()); err != nil {
		return err
	}
	p := newPrompter(ctx, stdin, stderr)
	var code string
	if *viaInvite {
		if code, err = readInviteCode(p); err != nil {
			return err
		}
	}
	password, err := p.newPassword("Password for the new account: ")
	if err != nil {
		return err
	}
	var (
		principal schema.PrincipalV0
		token     string
		channels  []schema.InviteChannelV1
	)
	if *viaInvite {
		req := schema.RedeemInviteRequestV2{Code: code, Name: name, Password: password}
		resp, err := client.RedeemInviteV2(ctx, req)
		if err != nil {
			return olderServer(err, "accounts with passwords")
		}
		principal, token = resp.Principal, resp.Token
	} else {
		resp, err := client.JoinAccount(ctx, schema.JoinAccountRequestV1{Name: name, Password: password})
		if err != nil {
			return olderServer(err, "accounts with passwords")
		}
		principal, token, channels = resp.Principal, resp.Token, resp.Channels
	}
	if !wellFormedToken(token) {
		return errors.New("cli: join account: the server returned a credential in an unexpected form; the account exists: sign in with 'conch login --password'")
	}
	if err := SaveToken(client.Server(), token); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "created account %s (id %d) on %s and signed in\n", safeLine(principal.Name), principal.ID, safeLine(client.Server())); err != nil {
		return err
	}
	if channels != nil {
		_, err = fmt.Fprintf(stdout, "channels: %s\n", channelNames(channels))
	}
	return err
}

// runAccount dispatches `conch account <verb>`.
func runAccount(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("cli: account: usage: conch account create [--server <url>] [--permit] <name>")
	}
	return runAccountCreate(ctx, args[1:], stdin, stdout, stderr)
}

// runAccountCreate makes the live guest visit an account (POST
// /v1/guest/convert): the same principal, its current access and history
// bounds, a permanent name and a password, and nothing more. --permit asks
// for an operator's conversion permit, for a visit whose entry point's
// policy is closed. On success the member credential replaces the visit's;
// the earlier messages keep showing as the guest's.
func runAccountCreate(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	const command = "account create"
	if err := refuseSecretArgs(command, args, "permit"); err != nil {
		return err
	}
	fs := quietFlags(command)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	withPermit := fs.Bool("permit", false, "use a conversion permit from an operator; it is asked for")
	pos, err := parseQuiet(fs, command, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("cli: account create: usage: conch account create [--server <url>] [--permit] <name> (the password is asked for)")
	}
	name := pos[0]
	if err := checkPermanentName(command, name); err != nil {
		return err
	}
	if err := requireSecureServer(command, *server); err != nil {
		return err
	}
	client, source, err := newSourcedClient(*server)
	if err != nil {
		return err
	}
	if source == sourceNone {
		return fmt.Errorf("cli: account create: no guest visit is stored for %s; join first with 'conch join guest'", safeLine(client.Server()))
	}
	p := newPrompter(ctx, stdin, stderr)
	var permit string
	if *withPermit {
		input, err := p.code("Conversion permit: ", "conversion permit")
		if err != nil {
			return err
		}
		if permit, err = codeFromInput(input, schema.SignInLinkCodePrefix, "a conversion permit", schema.ValidateSignInLinkCode); err != nil {
			return err
		}
	}
	password, err := p.newPassword("Password for the new account: ")
	if err != nil {
		return err
	}
	resp, err := client.ConvertGuest(ctx, schema.ConvertGuestRequestV1{Name: name, Password: password, Permit: permit})
	switch {
	case IsServerCode(err, schema.ErrorCodeConversionNotAllowed):
		return errors.New("cli: account create: this visit cannot become an account under the server's policy now; an operator can allow it with a conversion permit ('conch admin guest permit <id>'), then run this again with --permit")
	case err != nil && isNoAnswer(err):
		// The request may have been carried out with its answer lost: the
		// visit's credential would then be revoked, and only the password
		// gets the account back (design §3.3).
		return fmt.Errorf("cli: account create: no answer from the server (%w); if the account was made, %s; otherwise the visit is unchanged", err, signInHint(name))
	case err != nil:
		return olderServer(err, "accounts made from guest visits")
	}
	if !wellFormedToken(resp.Token) {
		return fmt.Errorf("cli: account create: the server returned a credential in an unexpected form; the account exists: %s", signInHint(name))
	}
	if source == sourceGuest {
		if err := replaceGuestWithMember(client.Server(), resp.Token); err != nil {
			return err
		}
	} else {
		// CONCH_TOKEN carried the visit; the member credential can only be
		// handed back, alone on standard output, as `credential issue` does.
		if _, err := fmt.Fprintln(stdout, resp.Token); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stderr, "created account %s (id %d); CONCH_TOKEN no longer works: use the credential printed on standard output\n", safeLine(resp.Principal.Name), resp.Principal.ID)
		return err
	}
	_, err = fmt.Fprintf(stdout, "created account %s (id %d) on %s and signed in; your current channels are kept, and messages from the visit still show as the guest's\n",
		safeLine(resp.Principal.Name), resp.Principal.ID, safeLine(client.Server()))
	return err
}

// isNoAnswer reports whether err is a request that got no HTTP answer: the
// server may or may not have acted on it.
func isNoAnswer(err error) bool {
	var se *ServerError
	var st *statusError
	return !errors.As(err, &se) && !errors.As(err, &st) && !errors.Is(err, ErrUnauthenticated)
}

// refusedOutright reports whether err is a refusal that trying again will
// not change: a 4xx with the server's reasons.
func refusedOutright(err error) bool {
	var se *ServerError
	return errors.As(err, &se) && se.Status >= 400 && se.Status < 500
}

// endVisitAttempts and endVisitPause bound how hard End visit tries when no
// answer comes (design §3.4: retry while possible, and say whether the
// server confirmed it). A variable so tests can shorten it.
var (
	endVisitAttempts = 3
	endVisitPause    = 500 * time.Millisecond
)

func runEndVisit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := quietFlags("end-visit")
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	pos, err := parseQuiet(fs, "end-visit", args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("cli: end-visit takes no arguments")
	}
	return endVisit(ctx, *server, stdout, stderr)
}

// endVisit ends the guest visit stored for server (POST /v1/guest/end) and,
// once the server has confirmed it, removes it from this machine. A visit
// the server no longer knows (401, or already ended) is gone already, and is
// removed too. With no answer it tries again a few times; if the end is
// never confirmed the visit stays stored, so it can be tried again, and the
// person is told it may still be live until its deadline.
func endVisit(ctx context.Context, server string, stdout, stderr io.Writer) error {
	client, source, err := newSourcedClient(server)
	if err != nil {
		return err
	}
	if source == sourceNone {
		return fmt.Errorf("cli: end-visit: no guest visit is stored for %s", safeLine(client.Server()))
	}
	var lastErr error
	for attempt := 0; attempt < endVisitAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(endVisitPause):
			}
		}
		resp, err := client.EndVisit(ctx)
		switch {
		case err == nil:
			if source == sourceGuest {
				if err := deleteGuestVisit(client.Server()); err != nil {
					return err
				}
			}
			_, err := fmt.Fprintf(stdout, "ended the visit of %s; your messages remain\n", safeLine(schema.GuestDisplayName(resp.Guest.Label, resp.Guest.Handle)))
			return err
		case errors.Is(err, ErrUnauthenticated), IsServerCode(err, schema.ErrorCodeGuestEnded):
			// The client removed a stored visit on the 401 already.
			if source != sourceGuest {
				_, err := fmt.Fprintln(stdout, "the visit had already ended")
				return err
			}
			if err := deleteGuestVisit(client.Server()); err != nil {
				return err
			}
			_, err := fmt.Fprintln(stdout, "the visit had already ended; removed it from this machine")
			return err
		case IsServerCode(err, schema.ErrorCodeNotAGuest):
			return errors.New("cli: end-visit: the login in use is not a guest visit; 'conch logout' signs a member out")
		case routeMissing(err):
			return olderServer(err, "guest visits")
		case refusedOutright(err):
			return err
		}
		lastErr = err
	}
	_, _ = fmt.Fprintln(stderr, "conch: the server did not confirm the end of the visit; it is still stored here, so 'conch end-visit' can try again")
	return fmt.Errorf("cli: end-visit: not confirmed (%w); the visit may stay live until it expires", lastErr)
}
