package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// The prompter: secrets from standard input only, without echo on a
// terminal (issue #715, design §5.4).

// fakeTerminalInput makes standard input a terminal whose no-echo reads give
// secrets and whose echoed reads give lines, in order, and records which
// kind each read was.
func fakeTerminalInput(t *testing.T, secrets, lines []string) (*os.File, *[]string) {
	t.Helper()
	var reads []string
	f := fakeTerminal(t, func(context.Context, int) ([]byte, error) {
		reads = append(reads, "secret")
		if len(secrets) == 0 {
			return nil, io.EOF
		}
		s := secrets[0]
		secrets = secrets[1:]
		return []byte(s), nil
	})
	old := readVisible
	readVisible = func(context.Context, *os.File) (string, error) {
		reads = append(reads, "line")
		if len(lines) == 0 {
			return "", io.EOF
		}
		s := lines[0]
		lines = lines[1:]
		return s, nil
	}
	t.Cleanup(func() { readVisible = old })
	return f, &reads
}

func TestPrompterTerminal(t *testing.T) {
	const pw = "  a password with spaces  "
	tests := []struct {
		name       string
		secrets    []string
		lines      []string
		ask        func(p *prompter) (string, error)
		want       string
		wantErr    string
		wantReads  string
		wantStderr string
	}{
		{name: "a secret is read without echo", secrets: []string{pw}, ask: func(p *prompter) (string, error) { return p.secret("Password: ", "password") },
			want: pw, wantReads: "secret", wantStderr: "Password: \n"},
		{name: "a name is read with echo", lines: []string{" alice "}, ask: func(p *prompter) (string, error) { return p.line("Name: ", "name") },
			want: "alice", wantReads: "line", wantStderr: "Name: "},
		{name: "a new password is asked twice", secrets: []string{memberPassword, memberPassword}, ask: func(p *prompter) (string, error) { return p.newPassword("New password: ") },
			want: memberPassword, wantReads: "secret secret", wantStderr: "New password: \nRepeat it: \n"},
		{name: "two different passwords", secrets: []string{memberPassword, memberPassword + "!"}, ask: func(p *prompter) (string, error) { return p.newPassword("New password: ") },
			wantErr: "the two passwords differ", wantReads: "secret secret", wantStderr: "New password: \nRepeat it: \n"},
		{name: "a short new password is refused before it is repeated", secrets: []string{"short"}, ask: func(p *prompter) (string, error) { return p.newPassword("New password: ") },
			wantErr: "password must be 15 to 128 characters", wantReads: "secret", wantStderr: "New password: \n"},
		{name: "a code is trimmed and checked", secrets: []string{" 123456 "}, ask: func(p *prompter) (string, error) { return p.totpCode("Code: ") },
			want: "123456", wantReads: "secret", wantStderr: "Code: \n"},
		{name: "a code that is not one", secrets: []string{"12345x"}, ask: func(p *prompter) (string, error) { return p.totpCode("Code: ") },
			wantErr: "a TOTP code is 6 digits", wantReads: "secret", wantStderr: "Code: \n"},
		{name: "ctrl-d is no answer", ask: func(p *prompter) (string, error) { return p.secret("Password: ", "password") },
			wantErr: "no password given", wantReads: "secret", wantStderr: "Password: \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin, reads := fakeTerminalInput(t, tt.secrets, tt.lines)
			var stderr bytes.Buffer
			p := newPrompter(context.Background(), stdin, &stderr)
			if !p.interactive() {
				t.Fatal("a terminal is not interactive")
			}
			got, err := tt.ask(p)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("answer = %q, %v; want %q", got, err, tt.want)
			}
			if strings.Join(*reads, " ") != tt.wantReads {
				t.Errorf("reads = %v, want %s", *reads, tt.wantReads)
			}
			if stderr.String() != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
			for _, s := range tt.secrets {
				if strings.TrimSpace(s) != "" && (strings.Contains(stderr.String(), strings.TrimSpace(s)) || (err != nil && strings.Contains(err.Error(), strings.TrimSpace(s)))) {
					t.Errorf("a secret was shown: stderr %q err %v", stderr.String(), err)
				}
			}
		})
	}
}

// From a pipe each answer is one line, in order, with no prompt; only the
// line ending is taken off a password.
func TestPrompterPipe(t *testing.T) {
	var stderr bytes.Buffer
	p := newPrompter(context.Background(), strings.NewReader(" bob \r\n  spaced password, kept  \r\n"+memberPassword+"\n"), &stderr)
	if p.interactive() {
		t.Fatal("a pipe is interactive")
	}
	name, err := p.line("Name: ", "name")
	if err != nil || name != "bob" {
		t.Fatalf("name = %q, %v", name, err)
	}
	pw, err := p.secret("Password: ", "password")
	if err != nil || pw != "  spaced password, kept  " {
		t.Fatalf("password = %q, %v", pw, err)
	}
	// From a pipe a new password is given once.
	if pw, err := p.newPassword("New password: "); err != nil || pw != memberPassword {
		t.Fatalf("new password = %q, %v", pw, err)
	}
	if _, err := p.secret("Code: ", "authenticator code"); err == nil || err.Error() != "cli: no authenticator code given" {
		t.Errorf("past the end = %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a pipe was prompted: %q", stderr.String())
	}
}

// A terminal that cannot turn echo off is never read with echo instead.
func TestPrompterNoEchoUnavailable(t *testing.T) {
	stdin := fakeTerminal(t, func(context.Context, int) ([]byte, error) { return nil, errors.New("inappropriate ioctl for device") })
	old := readVisible
	readVisible = func(context.Context, *os.File) (string, error) {
		t.Error("a secret was read with echo")
		return "", nil
	}
	t.Cleanup(func() { readVisible = old })
	_, err := newPrompter(context.Background(), stdin, io.Discard).secret("Password: ", "password")
	if err == nil || !strings.Contains(err.Error(), "cannot read the password without echo") {
		t.Errorf("err = %v", err)
	}
}

func TestCodeFromInput(t *testing.T) {
	code := schema.InviteCodePrefix + strings.Repeat("A", 43)
	tests := []struct {
		name, input, want string
	}{
		{"the code", code, code},
		{"the link", "https://conch.example/app/#/invite/" + code, code},
		{"the link with a slash after", "https://conch.example/app/#/invite/" + code + "/", code},
		{"another kind of code", schema.SignInLinkCodePrefix + strings.Repeat("A", 43), ""},
		{"a short code", code[:20], ""},
		{"nothing like one", "hello", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := codeFromInput(tt.input, schema.InviteCodePrefix, "an invite link or code", schema.ValidateInviteCode)
			if tt.want == "" {
				if err == nil || err.Error() != "cli: that is not an invite link or code" {
					t.Errorf("err = %v, want the refusal that names nothing", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("code = %q, %v", got, err)
			}
		})
	}
}

func TestRefuseSecretArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		switches []string
		refused  bool
	}{
		{"a switch alone", []string{"--password"}, []string{"password"}, false},
		{"a switch set true", []string{"--password=true"}, []string{"password"}, false},
		{"a switch given a value", []string{"--password=x"}, []string{"password"}, true},
		{"a secret flag that is no switch", []string{"--password", "x"}, nil, true},
		{"one dash", []string{"-code=123456"}, nil, true},
		{"a token flag", []string{"--token"}, nil, true},
		{"a secret-shaped argument", []string{"conch_sil_" + strings.Repeat("a", 43)}, nil, true},
		{"one inside a link", []string{"https://x/app/#/signin/conch_sil_" + strings.Repeat("a", 43)}, nil, true},
		{"ordinary flags and names", []string{"--server", "https://x", "--label", "Pat", "conch_fan"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseSecretArgs("cmd", tt.args, tt.switches...)
			if (err != nil) != tt.refused {
				t.Fatalf("err = %v, refused %v", err, tt.refused)
			}
			if err != nil && strings.Contains(err.Error(), "aaaa") {
				t.Errorf("the refusal repeats the argument: %v", err)
			}
		})
	}
}

func TestRequireSecureServer(t *testing.T) {
	tests := []struct {
		server string
		ok     bool
	}{
		{"https://conch.example", true},
		{"HTTPS://conch.example:8443/prefix", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.9.9.9", true},
		{"http://[::1]:8080", true},
		{"http://localhost:8080", true},
		{"http://conch.example", false},
		{"http://192.168.1.10:8080", false},
		{"http://localhost.example", false},
	}
	for _, tt := range tests {
		t.Run(tt.server, func(t *testing.T) {
			if err := requireSecureServer("cmd", tt.server); (err == nil) != tt.ok {
				t.Errorf("requireSecureServer(%q) = %v, want ok %v", tt.server, err, tt.ok)
			}
		})
	}
}

// A refused flag value is named by its flag, never repeated.
func TestParseQuietNeverRepeatsAValue(t *testing.T) {
	fs := quietFlags("cmd")
	fs.Bool("yes", false, "")
	_, err := parseQuiet(fs, "cmd", []string{"--yes=hunter2hunter2"})
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "-yes") {
		t.Errorf("err = %v, want the flag named and its value left out", err)
	}
	// Any other parse error is reported generically, never in the flag
	// package's words.
	_, err = parseQuiet(quietFlags("cmd"), "cmd", []string{"--hunter2hunter2"})
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "cannot parse the flags") {
		t.Errorf("err = %v, want the generic refusal", err)
	}
}
