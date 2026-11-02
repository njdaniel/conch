package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

// Hostile characters are always written by number in this file. An escape
// typed into source can be turned into the character itself by an editor or a
// tool, which would both defeat a test and put a raw control character in the
// repository.
var (
	esc   = string(rune(0x1b))
	bel   = string(rune(0x07))
	bs    = string(rune(0x08))
	nul   = string(rune(0x00))
	del   = string(rune(0x7f))
	c1    = string(rune(0x85))
	rlo   = string(rune(0x202e)) // right-to-left override
	lri   = string(rune(0x2066)) // left-to-right isolate
	lrm   = string(rune(0x200e))
	lsep  = string(rune(0x2028))
	psep  = string(rune(0x2029))
	zwj   = string(rune(0x200d))
	bsl   = string(rune(0x5c))
	crlf  = string(rune(0x0d)) + string(rune(0x0a))
	tabch = string(rune(0x09))
)

// hostile is a string that carries one of every kind of character a terminal
// would act on, and marks where it was with printable text.
var hostile = "H" + esc + "[2J" + esc + "]0;pwned" + bel + bs + nul + del + c1 + rlo + lri + lrm + lsep + psep + crlf + tabch + "T"

func TestTailBody(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary", "hello world", "hello world"},
		{"accents and CJK", "café 日本語", "café 日本語"},
		{"emoji with zero-width joiner is unchanged", "family " + string(rune(0x1f469)) + zwj + string(rune(0x1f469)) + zwj + string(rune(0x1f467)) + " ok", "family " + string(rune(0x1f469)) + zwj + string(rune(0x1f469)) + zwj + string(rune(0x1f467)) + " ok"},
		{"CSI", "a" + esc + "[2Jb", `a\x1b[2Jb`},
		{"OSC with BEL terminator", esc + "]0;title" + bel, `\x1b]0;title\a`},
		{"OSC with ST terminator", esc + "]0;t" + esc + bsl, `\x1b]0;t\x1b\\`},
		{"BEL", "a" + bel + "b", `a\ab`},
		{"backspace", "a" + bs + "b", `a\bb`},
		{"tab", "a" + tabch + "b", `a\tb`},
		{"NUL", "a" + nul + "b", `a\x00b`},
		{"DEL", "a" + del + "b", `a\x7fb`},
		{"C1 control", "a" + c1 + "b", `a\u0085b`},
		{"right-to-left override", "a" + rlo + "b", `a\u202eb`},
		{"left-to-right isolate", "a" + lri + "b", `a\u2066b`},
		{"line separator", "a" + lsep + "b", `a\u2028b`},
		{"paragraph separator", "a" + psep + "b", `a\u2029b`},
		{"CR LF", "a" + crlf + "b", `a\r\nb`},
		{"mixed with ordinary text", "ok " + esc + "[31mred" + esc + "[0m café", `ok \x1b[31mred\x1b[0m caf` + "é"},
		{"only a backslash", bsl, `\\`},
		{"starts with a bracket", "[whisper:3] x", `\[whisper:3] x`},
		{"starts with a backslash then a bracket", bsl + "[x", `\\[x`},
		{"bracket after an escaped control", esc + "[x", `\x1b[x`},
		{"later brackets untouched", "a [b] c", "a [b] c"},
		{"invalid UTF-8 byte is kept visibly", "a" + string([]byte{0xff}) + "b", `a\xffb`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tailBody(tt.in)
			if got != tt.want {
				t.Errorf("tailBody(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if bad := firstUnsafe(got); bad != "" {
				t.Errorf("tailBody(%q) = %q: %s", tt.in, got, bad)
			}
		})
	}
}

// Two different bodies must never print the same line, whatever the bodies
// are made of: the alphabet has the escape character, the characters an
// escape is spelt with, the bracket rule and the hostile runes.
func TestTailBodyIsInjective(t *testing.T) {
	alphabet := []string{bsl, "[", "n", "x", "u", "r", "0", "1", "a", "2", " ", esc, bel, nul, del, c1, rlo, lri, lsep, psep, crlf, tabch, zwj, "é"}
	rng := rand.New(rand.NewSource(234)) // #nosec G404 -- a test's reproducible input
	seen := make(map[string]string)
	for range 5000 {
		var b strings.Builder
		for range rng.Intn(6) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		body := b.String()
		line := tailBody(body)
		if prev, ok := seen[line]; ok && prev != body {
			t.Fatalf("bodies %q and %q both print as %q", prev, body, line)
		}
		seen[line] = body
	}
	if len(seen) < 1000 {
		t.Fatalf("only %d distinct bodies were drawn; the property was barely tested", len(seen))
	}
}

// firstUnsafe describes the first thing in s that a terminal could act on, or
// is "" when there is none. A newline is allowed: it ends a line.
func firstUnsafe(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 && s[i] != '\n' {
			return "raw byte " + string(rune('0'+s[i]/16)) + " below 0x20"
		}
		if s[i] == 0x7f {
			return "raw DEL"
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsControl(r) && r != '\n':
			return "raw control character"
		case unicode.Is(unicode.Bidi_Control, r):
			return "raw bidi control"
		case unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return "raw line or paragraph separator"
		}
	}
	return ""
}

// walkCase is one plain-CLI command run by the walk. key is the command as
// the usage text names it ("nets list").
type walkCase struct {
	key  string
	args func(url, dir string) []string
	// stdin is what the command reads, for login.
	stdin string
	// whoamiOK keeps whoami answering normally in the error run, so a command
	// that asks who you are first gets past it to its own routes.
	whoamiOK bool
	// quietWhenOK is a command whose success prints none of the server's
	// strings (an id, or only what the user typed).
	quietWhenOK bool
}

func walkCases(t *testing.T) []walkCase {
	t.Helper()
	return []walkCase{
		{key: "send", quietWhenOK: true, whoamiOK: true, args: func(u, _ string) []string { return []string{"send", "--server", u, "general", "hello"} }},
		{key: "tail", args: func(u, _ string) []string { return []string{"tail", "--server", u, "general"} }},
		{key: "search", args: func(u, _ string) []string { return []string{"search", "--server", u, "general", "hello"} }},
		{key: "thread", args: func(u, _ string) []string { return []string{"thread", "--server", u, "general", "3"} }},
		{key: "upload", quietWhenOK: true, whoamiOK: true, args: func(u, dir string) []string {
			return []string{"upload", "--server", u, "general", filepath.Join(dir, "note.txt")}
		}},
		{key: "download", quietWhenOK: true, args: func(u, dir string) []string {
			return []string{"download", "--server", u, "-o", filepath.Join(dir, "out.bin"), "1"}
		}},
		{key: "nets list", args: func(u, _ string) []string { return []string{"nets", "list", "--server", u, "general"} }},
		{key: "nets create", args: func(u, _ string) []string { return []string{"nets", "create", "--server", u, "general", "charlie"} }},
		{key: "nets archive", quietWhenOK: true, args: func(u, _ string) []string { return []string{"nets", "archive", "--server", u, "general", "alpha"} }},
		{key: "nets add", quietWhenOK: true, args: func(u, _ string) []string { return []string{"nets", "add", "--server", u, "general", "alpha", "9"} }},
		{key: "nets remove", quietWhenOK: true, args: func(u, _ string) []string { return []string{"nets", "remove", "--server", u, "general", "alpha", "9"} }},
		{key: "admin channel create", args: func(u, _ string) []string { return []string{"admin", "channel", "create", "--server", u, "general"} }},
		{key: "admin principal create", args: func(u, _ string) []string {
			return []string{"admin", "principal", "create", "--server", u, "--kind", "human", "bob"}
		}},
		{key: "admin principal disable|enable", quietWhenOK: true, args: func(u, _ string) []string { return []string{"admin", "principal", "disable", "--server", u, "9"} }},
		{key: "admin principal rename", args: func(u, _ string) []string { return []string{"admin", "principal", "rename", "--server", u, "9", "bob"} }},
		{key: "admin principal role", args: func(u, _ string) []string {
			return []string{"admin", "principal", "role", "--server", u, "9", "operator"}
		}},
		{key: "admin principal list", args: func(u, _ string) []string { return []string{"admin", "principal", "list", "--server", u} }},
		{key: "admin principal hide", args: func(u, _ string) []string { return []string{"admin", "principal", "hide", "--server", u, "9"} }},
		{key: "admin principal hide", args: func(u, _ string) []string {
			return []string{"admin", "principal", "hide", "--server", u, "--prefix", "test-"}
		}},
		{key: "admin credential issue", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "credential", "issue", "--server", u, "--label", "laptop", "9"}
		}},
		{key: "admin credential list", args: func(u, _ string) []string { return []string{"admin", "credential", "list", "--server", u, "9"} }},
		{key: "admin credential revoke", quietWhenOK: true, args: func(u, _ string) []string { return []string{"admin", "credential", "revoke", "--server", u, "9"} }},
		{key: "admin member add", quietWhenOK: true, args: func(u, _ string) []string { return []string{"admin", "member", "add", "--server", u, "general", "9"} }},
		{key: "admin member add", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "member", "add", "--server", u, "general", "--name", "bob"}
		}},
		{key: "admin member manage|decide", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "member", "manage", "--server", u, "general", "9"}
		}},
		{key: "admin member remove", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "member", "remove", "--server", u, "general", "9"}
		}},
		{key: "admin manifest put", quietWhenOK: true, stdin: testManifest, args: func(u, _ string) []string { return []string{"admin", "manifest", "put", "--server", u, "9"} }},
		{key: "admin manifest get", args: func(u, _ string) []string { return []string{"admin", "manifest", "get", "--server", u, "9"} }},
		{key: "admin invite create", args: func(u, _ string) []string {
			return []string{"admin", "invite", "create", "--server", u, "--open", "--channel", "general", "--expires", "1h"}
		}},
		{key: "admin invite list", args: func(u, _ string) []string { return []string{"admin", "invite", "list", "--server", u} }},
		{key: "admin invite close|joiners", args: func(u, _ string) []string { return []string{"admin", "invite", "joiners", "--server", u, "9"} }},
		{key: "admin invite disable-joiners", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "invite", "disable-joiners", "--server", u, "9"}
		}},
		{key: "admin invite pause|resume|status", quietWhenOK: true, args: func(u, _ string) []string { return []string{"admin", "invite", "status", "--server", u} }},
		{key: "admin principal invite-permission", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "principal", "invite-permission", "--server", u, "9", "on"}
		}},
		{key: "admin principal disable-invitees", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "principal", "disable-invitees", "--server", u, "9"}
		}},
		{key: "admin principal sign-in-link", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "principal", "sign-in-link", "--server", u, "9"}
		}},
		{key: "admin sign-in-link close", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"admin", "sign-in-link", "close", "--server", u, "9"}
		}},
		{key: "voice status", args: func(u, _ string) []string { return []string{"voice", "status", "--server", u, "general"} }},
		{key: "voice where", quietWhenOK: true, args: func(u, _ string) []string { return []string{"voice", "where", "--server", u} }},
		{key: "approvals list", args: func(u, _ string) []string { return []string{"approvals", "list", "--server", u} }},
		{key: "approve", whoamiOK: true, args: func(u, _ string) []string { return []string{"approve", "--server", u, "--reason", "fine", "1"} }},
		{key: "reject", whoamiOK: true, args: func(u, _ string) []string { return []string{"reject", "--server", u, "--reason", "no", "1"} }},
		{key: "login", stdin: "tok\n", args: func(u, _ string) []string { return []string{"login", "--server", u} }},
		{key: "login", stdin: "bob\n" + walkPassword + "\n", args: func(u, _ string) []string { return []string{"login", "--server", u, "--password"} }},
		{key: "login", stdin: walkSignInCode + "\n", args: func(u, _ string) []string { return []string{"login", "--server", u, "--link", "--yes"} }},
		// Joining, guest visits, passwords and device links (issue #715).
		{key: "join info", args: func(u, _ string) []string { return []string{"join", "info", "--server", u} }},
		{key: "join account", stdin: walkPassword + "\n", args: func(u, _ string) []string { return []string{"join", "account", "--server", u, "bob"} }},
		{key: "account create", stdin: walkPassword + "\n", args: func(u, _ string) []string { return []string{"account", "create", "--server", u, "bob"} }},
		{key: "end-visit", args: func(u, _ string) []string { return []string{"end-visit", "--server", u} }},
		{key: "password set", quietWhenOK: true, stdin: walkPassword + "\n", args: func(u, _ string) []string { return []string{"password", "set", "--server", u} }},
		{key: "password change", quietWhenOK: true, whoamiOK: true, stdin: walkPassword + "x\n" + walkPassword + "\n", args: func(u, _ string) []string {
			return []string{"password", "change", "--server", u}
		}},
		{key: "password reset", stdin: walkResetCode + "\n" + walkPassword + "\n", args: func(u, _ string) []string { return []string{"password", "reset", "--server", u} }},
		{key: "reauth", quietWhenOK: true, whoamiOK: true, stdin: walkPassword + "\n", args: func(u, _ string) []string { return []string{"reauth", "--server", u} }},
		{key: "setup", stdin: walkSetupToken + "\n" + walkPassword + "\n123456\n", args: func(u, _ string) []string { return []string{"setup", "--server", u} }},
		{key: "device-link create", quietWhenOK: true, args: func(u, _ string) []string { return []string{"device-link", "create", "--server", u} }},
		{key: "device-link list", args: func(u, _ string) []string { return []string{"device-link", "list", "--server", u} }},
		{key: "device-link close", quietWhenOK: true, args: func(u, _ string) []string { return []string{"device-link", "close", "--server", u, "9"} }},
		{key: "admin join status", args: func(u, _ string) []string { return []string{"admin", "join", "status", "--server", u} }},
		{key: "admin join set", args: func(u, _ string) []string { return []string{"admin", "join", "set", "--server", u, "--guests", "off"} }},
		{key: "admin guest list", args: func(u, _ string) []string { return []string{"admin", "guest", "list", "--server", u} }},
		{key: "admin guest end", args: func(u, _ string) []string { return []string{"admin", "guest", "end", "--server", u, "9"} }},
		{key: "admin guest end-all", args: func(u, _ string) []string { return []string{"admin", "guest", "end-all", "--server", u} }},
		{key: "admin guest permit", quietWhenOK: true, args: func(u, _ string) []string { return []string{"admin", "guest", "permit", "--server", u, "9"} }},
		{key: "admin permissions show", args: func(u, _ string) []string { return []string{"admin", "permissions", "show", "--server", u} }},
		{key: "admin permissions set", args: func(u, _ string) []string {
			return []string{"admin", "permissions", "set", "--server", u, "--members", "off", "use_voice"}
		}},
		{key: "admin permissions preset", args: func(u, _ string) []string {
			return []string{"admin", "permissions", "preset", "--server", u, "--yes", "personal"}
		}},
		{key: "whoami", args: func(u, _ string) []string { return []string{"whoami", "--server", u} }},
		{key: "rename", whoamiOK: true, args: func(u, _ string) []string { return []string{"rename", "--server", u, "bob"} }},
		{key: "report", quietWhenOK: true, args: func(u, _ string) []string {
			return []string{"report", "--server", u, "--did", "a", "--expected", "b", "--happened", "c", "--often", "once", "--yes"}
		}},
		{key: "admin reports list", args: func(u, _ string) []string { return []string{"admin", "reports", "list", "--server", u} }},
		{key: "admin reports show", args: func(u, _ string) []string { return []string{"admin", "reports", "show", "--server", u, "9"} }},
		{key: "admin reports file", args: func(u, _ string) []string {
			return []string{"admin", "reports", "file", "--server", u, "9", "https://example.com/issues/1"}
		}},
	}
}

// The secrets the walk types and the server-made ones its server answers
// with: each in the exact form a client checks before it prints or keeps one.
var (
	walkPassword   = "correct horse battery staple"
	walkSignInCode = schema.SignInLinkCodePrefix + strings.Repeat("A", schema.SignInLinkCodeLength-len(schema.SignInLinkCodePrefix))
	walkResetCode  = schema.SignInLinkCodePrefix + strings.Repeat("B", schema.SignInLinkCodeLength-len(schema.SignInLinkCodePrefix))
	walkSetupToken = schema.SetupTokenPrefix + strings.Repeat("A", schema.SetupTokenLength-len(schema.SetupTokenPrefix))
	walkToken      = schema.CredentialTokenPrefix + strings.Repeat("A", schema.CredentialTokenLength-len(schema.CredentialTokenPrefix))
	walkHandle     = schema.GuestNamePrefix + "a7k4m2q9a7k4m2q9"
)

// walkVisit is a well-formed live visit: a client checks a visit before it
// keeps one, so its hostile text is in the fields it only prints.
func walkVisit(ts schema.Timestamp) schema.GuestV1 {
	return schema.GuestV1{State: schema.GuestStateActive, Origin: schema.GuestOriginAddress, Handle: walkHandle, Label: "Pat", Tag: schema.GuestTag(walkHandle),
		StartedAt: ts, ExpiresAt: schema.NewTimestamp(ts.Time().Add(24 * time.Hour)), IdleExpiresAt: schema.NewTimestamp(ts.Time().Add(2 * time.Hour))}
}

// Commands the walk does not run, and why. Every other command in the usage
// text must be in walkCases.
var walkExempt = map[string]string{
	"audit export": "copies the server's JSON Lines byte for byte by design (#21); its error line is covered by the audit tests",
	"logout":       "prints nothing the server sent",
	"version":      "prints the client's own version",
}

// usageCommands are the commands the usage text lists, as "send", "nets
// list", "voice status".
func usageCommands(t *testing.T) []string {
	t.Helper()
	var buf bytes.Buffer
	Usage(&buf)
	var keys []string
	for _, line := range strings.Split(buf.String(), "\n") {
		rest, ok := strings.CutPrefix(line, "  conch ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		key := fields[0]
		switch key {
		case "nets", "voice", "audit", "approvals", "join", "account", "password", "device-link":
			key += " " + fields[1]
		case "admin":
			key += " " + fields[1] + " " + fields[2]
		}
		keys = append(keys, key)
	}
	return keys
}

// hostileServer answers every route the plain CLI uses. In ok mode the
// answers are well formed and every string in them is hostile; otherwise
// every route (whoami aside, when whoamiOK) refuses with a hostile code and
// message.
func hostileServer(t *testing.T, ok, whoamiOK bool) *httptest.Server {
	t.Helper()
	ts := schema.NewTimestamp(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	empty := sha256.Sum256(nil)
	write := func(w http.ResponseWriter, status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if !ok && (!whoamiOK || key != "GET /v1/whoami") {
			write(w, http.StatusForbidden, schema.Error{Code: "bad" + esc + "[31m" + rlo, Message: hostile, Permission: schema.Permission(hostile)})
			return
		}
		switch key {
		case "GET /v1/whoami":
			write(w, http.StatusOK, schema.WhoAmIResponseV1{ID: 42, Kind: schema.PrincipalKind(hostile), Name: hostile, Role: schema.PrincipalRole(hostile), Permissions: []schema.Permission{schema.Permission(hostile), schema.PermUseVoice}, PermissionsRevision: 1})
		case "POST /v2/channels/general/messages":
			write(w, http.StatusCreated, schema.PostMessageResponseV2{Message: schema.MessageV2{ID: 1, Body: hostile, CreatedAt: ts}})
		case "GET /v2/channels/general/messages":
			// The thread listing (?thread=3): a root and a reply, each hostile.
			write(w, http.StatusOK, schema.ListMessagesResponseV2{Messages: []schema.MessageV2{
				{Schema: schema.MessageSchemaV2, ID: 3, AuthorID: 42, Body: hostile, CreatedAt: ts, Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}},
				{Schema: schema.MessageSchemaV2, ID: 4, AuthorID: 42, Body: hostile, CreatedAt: ts, ThreadRootID: 3, Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}},
			}})
		case "GET /v2/channels/general/messages/search":
			write(w, http.StatusOK, schema.SearchMessagesResponseV2{Messages: []schema.MessageV2{
				{Schema: schema.MessageSchemaV2, ID: 3, AuthorID: 42, Body: hostile, CreatedAt: ts},
				{Schema: schema.MessageSchemaV2, ID: 4, AuthorID: 42, Body: hostile, CreatedAt: ts, Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}},
				{Schema: schema.MessageSchemaV2, ID: 5, AuthorID: 42, Body: "x", CreatedAt: ts, Audience: &schema.Audience{Kind: schema.AudienceKind(hostile)}},
			}})
		case "GET /v2/ws":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			for _, m := range []schema.MessageV2{
				{Schema: schema.MessageSchemaV2, ID: 3, AuthorID: 42, Body: hostile, CreatedAt: ts},
				{Schema: schema.MessageSchemaV2, ID: 4, AuthorID: 42, Body: hostile, CreatedAt: ts, Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}},
			} {
				_ = wsjson.Write(r.Context(), conn, m)
			}
			_ = conn.Close(websocket.StatusGoingAway, "bye")
		case "GET /v1/channels/general/nets":
			write(w, http.StatusOK, schema.ListNetsResponseV1{Nets: []schema.NetV1{
				{ID: 4, Name: hostile, Members: []schema.NetMember{{PrincipalID: 9, Role: schema.NetRole(hostile)}}},
				{ID: 5, Name: hostile, Members: []schema.NetMember{}},
			}})
		case "POST /v1/channels/general/nets":
			write(w, http.StatusCreated, schema.CreateNetResponseV1{Net: schema.NetV1{ID: 6, Name: hostile}})
		case "DELETE /v1/channels/general/nets/alpha", "DELETE /v1/channels/general/nets/alpha/members/9", "PUT /v1/channels/general/nets/alpha/members/9":
			w.WriteHeader(http.StatusNoContent)
		case "POST /v0/channels":
			write(w, http.StatusCreated, schema.CreateChannelResponse{Channel: schema.ChannelV0{ID: 1, Name: hostile, CreatedAt: ts.Time()}})
		case "POST /v0/principals":
			write(w, http.StatusCreated, schema.CreatePrincipalResponse{Principal: schema.PrincipalV0{ID: 9, Kind: schema.PrincipalKind(hostile), Name: hostile, CreatedAt: ts.Time()}})
		case "PUT /v1/principals/9/name", "PUT /v1/principals/42/name":
			write(w, http.StatusOK, schema.RenamePrincipalResponseV1{Principal: schema.PrincipalV0{ID: 9, Kind: schema.PrincipalKind(hostile), Name: hostile, CreatedAt: ts.Time()}})
		case "GET /v1/principals":
			invite := int64(3)
			write(w, http.StatusOK, schema.ListPrincipalsResponseV1{Principals: []schema.PrincipalAdminV1{{
				ID: 9, Kind: schema.PrincipalKind(hostile), Name: hostile, Role: schema.PrincipalRole(hostile), CreatedAt: ts, InviteID: &invite,
			}}})
		case "PUT /v1/principals/9/role":
			write(w, http.StatusOK, schema.RoleChangeResponseV1{Principal: schema.PrincipalAdminV1{
				ID: 9, Kind: schema.PrincipalKind(hostile), Name: hostile, Role: schema.PrincipalRole(hostile), Owner: true, CreatedAt: ts,
			}, Manages: []int64{1}, InvitesClosed: 1, CredentialsRevoked: 2, HooksRevoked: 3, LiveCredentials: 4, HooksBound: 5})
		case "POST /v1/principals/9/hide":
			write(w, http.StatusOK, schema.HidePrincipalResponseV1{Principal: schema.PrincipalAdminV1{
				ID: 9, Kind: schema.PrincipalKind(hostile), Name: hostile, Role: schema.PrincipalRole(hostile), CreatedAt: ts, HiddenAt: &ts, DisabledAt: &ts,
			}})
		case "POST /v1/principals/hide":
			write(w, http.StatusOK, schema.HidePrincipalsResponseV1{DryRun: true, Count: 1, Principals: []schema.HiddenPrincipalV1{{ID: 9, Name: hostile}}, Remaining: 2})
		case "POST /v1/principals/9/disable", "DELETE /v1/credentials/9", "PUT /v1/channels/general/members/9", "DELETE /v1/channels/general/members/9":
			w.WriteHeader(http.StatusNoContent)
		case "POST /v1/channels/general/members":
			write(w, http.StatusCreated, schema.ChannelMemberV1{PrincipalID: 9, Name: hostile, Kind: schema.PrincipalKind(hostile), CreatedAt: ts})
		case "POST /v1/principals/9/credentials":
			write(w, http.StatusCreated, schema.CreateCredentialResponseV1{
				Credential: schema.CredentialV1{ID: 5, PrincipalID: 9, Label: hostile, CreatedAt: ts},
				Token:      schema.CredentialTokenPrefix + strings.Repeat("A", schema.CredentialTokenLength-len(schema.CredentialTokenPrefix)),
			})
		case "GET /v1/principals/9/credentials":
			write(w, http.StatusOK, schema.ListCredentialsResponseV1{Credentials: []schema.CredentialV1{{ID: 5, PrincipalID: 9, Label: hostile, CreatedAt: ts}}})
		case "PUT /v1/principals/9/manifest":
			write(w, http.StatusOK, schema.PutAgentManifestResponseV1{Manifest: schema.AgentManifestV1{PrincipalID: 9, DisplayName: hostile, Tier: schema.AgentTier(hostile)}})
		case "GET /v1/principals/9/manifest":
			write(w, http.StatusOK, schema.GetAgentManifestResponseV1{Manifest: schema.AgentManifestV1{
				PrincipalID: 9, DisplayName: hostile, Tier: schema.AgentTier(hostile),
				Capabilities: []schema.Capability{schema.Capability(hostile)},
				Channels:     []schema.ChannelGrant{{ChannelID: 1, Permissions: []schema.ChannelPermission{schema.ChannelPermission(hostile)}}},
			}})
		case "POST /v1/invites":
			write(w, http.StatusCreated, schema.CreateInviteResponseV1{
				Invite: schema.InviteV1{ID: 3, Kind: schema.InviteKind(hostile), CreatedBy: 42, CreatedAt: ts, ExpiresAt: ts,
					Channels: []schema.InviteChannelV1{{ID: 1, Name: hostile}}},
				Code: schema.InviteCodePrefix + strings.Repeat("A", schema.InviteCodeLength-len(schema.InviteCodePrefix)),
			})
		case "GET /v1/invites":
			write(w, http.StatusOK, schema.ListInvitesResponseV1{Invites: []schema.InviteV1{{
				ID: 3, Kind: schema.InviteKind(hostile), CreatedBy: 42, CreatedAt: ts, ExpiresAt: ts,
				Channels: []schema.InviteChannelV1{{ID: 1, Name: hostile}},
			}}})
		case "GET /v1/invites/9/principals":
			write(w, http.StatusOK, schema.InviteJoinersResponseV1{Joiners: []schema.InviteJoinerV1{{ID: 5, Name: hostile, CreatedAt: ts}}})
		case "POST /v1/invites/9/disable-joiners", "POST /v1/principals/9/disable-invitees":
			write(w, http.StatusOK, schema.DisableJoinersResponseV1{DryRun: true, Count: 1, PrincipalIDs: []int64{5}})
		case "GET /v1/invites/settings":
			write(w, http.StatusOK, schema.InviteSettingsV1{Paused: true})
		case "PUT /v1/principals/9/invite-permission", "DELETE /v1/sign-in-links/9":
			w.WriteHeader(http.StatusNoContent)
		case "POST /v1/principals/9/sign-in-links":
			write(w, http.StatusCreated, schema.CreateSignInLinkResponseV1{
				SignInLink: schema.SignInLinkV1{ID: 3, PrincipalID: 9, CreatedBy: 42, CreatedAt: ts, ExpiresAt: ts},
				Code:       schema.SignInLinkCodePrefix + strings.Repeat("A", schema.SignInLinkCodeLength-len(schema.SignInLinkCodePrefix)),
			})
		case "GET /v1/channels/general/voice":
			write(w, http.StatusOK, schema.VoicePresenceV1{Schema: schema.VoicePresenceSchemaV1, ChannelID: 1, Configured: true, Available: true, Rooms: []schema.VoicePresenceRoom{{
				Participants: []schema.VoiceParticipant{{PrincipalID: 42, JoinedAt: ts}},
			}}})
		case "GET /v1/voice/occupancy":
			write(w, http.StatusOK, schema.VoiceOccupancyV1{Schema: schema.VoiceOccupancySchemaV1, Configured: true, Available: true,
				Channels: []schema.VoiceChannelOccupancy{{ChannelID: 1, InVoice: 2}}})
		case "GET /v1/approvals":
			write(w, http.StatusOK, schema.ListApprovalsResponseV1{Approvals: []schema.ApprovalV1{
				{ID: 1, State: schema.ApprovalState(hostile), Title: hostile, Body: hostile, RequesterID: 42, Deadline: ts},
			}})
		case "POST /v1/approvals/1/decisions":
			write(w, http.StatusCreated, schema.CastDecisionResponseV1{State: schema.ApprovalState(hostile), Decision: schema.Decision{PrincipalID: 42, OptionID: "approve", Reason: hostile}})
		case "POST /v2/channels/general/files":
			write(w, http.StatusCreated, schema.FileV1{Schema: schema.FileSchemaV1, ID: 7, Filename: "note.txt", MediaType: "text/plain", SHA256: hex.EncodeToString(empty[:]), CreatedAt: ts})
		case "GET /v2/files/1/meta":
			write(w, http.StatusOK, schema.FileV1{Schema: schema.FileSchemaV1, ID: 1, Filename: hostile, MediaType: hostile, Size: 0, SHA256: hex.EncodeToString(empty[:]), CreatedAt: ts})
		case "GET /v2/files/1":
			w.WriteHeader(http.StatusOK)
		case "POST /v1/reports":
			write(w, http.StatusCreated, schema.CreateProblemReportResponseV1{ID: 9, CreatedAt: ts})
		case "GET /v1/reports":
			write(w, http.StatusOK, schema.ListProblemReportsResponseV1{Reports: []schema.ProblemReportSummaryV1{{
				ID: 9, State: schema.ProblemReportState(hostile), CreatedAt: ts, PrincipalID: 42, ClientName: hostile, ClientVersion: hostile,
				ServerVersion: hostile, Often: schema.ProblemReportOften(hostile), Issue: hostile,
			}}})
		case "GET /v1/reports/9", "PUT /v1/reports/9":
			write(w, http.StatusOK, hostileReport(ts))
		case "GET /v1/channels":
			write(w, http.StatusOK, schema.ListChannelsResponse{Channels: []schema.ChannelV0{{ID: 1, Name: "general", CreatedAt: ts.Time()}}})
		case "GET /v1/channels/general/members":
			write(w, http.StatusOK, schema.ListChannelMembersResponseV1{Members: []schema.ChannelMemberV1{{PrincipalID: 42, Name: hostile, CreatedAt: ts}}})
		case "POST /v1/login":
			cred := schema.CredentialV1{ID: 5, PrincipalID: 42, Label: hostile, CreatedAt: ts}
			write(w, http.StatusOK, schema.LoginResponseV1{Next: schema.LoginNextSignedIn, Principal: &schema.PrincipalV0{ID: 42, Name: hostile}, Credential: &cred, Token: walkToken})
		case "POST /v1/sign-in-links/preview":
			// The walk's reset link and its sign-in link are told apart by
			// their codes.
			var req schema.SignInLinkPreviewRequestV1
			_ = json.NewDecoder(r.Body).Decode(&req)
			purpose := schema.SignInLinkPurposeSignIn
			if req.Code == walkResetCode {
				purpose = schema.SignInLinkPurposeReset
			}
			write(w, http.StatusOK, schema.SignInLinkPreviewV1{Name: hostile, Purpose: purpose})
		case "POST /v1/sign-in-links/redeem":
			write(w, http.StatusCreated, schema.RedeemSignInLinkResponseV1{Principal: schema.PrincipalV0{ID: 42, Name: hostile}, Credential: schema.CredentialV1{ID: 5, PrincipalID: 42, CreatedAt: ts}, Token: walkToken})
		case "GET /v1/join":
			write(w, http.StatusOK, schema.JoinInfoV1{ServerName: hostile, Join: schema.JoinOptionsV1{Guest: true, Paused: true}, GuestIdleSeconds: 7200, GuestAbsoluteSeconds: 86400})
		case "POST /v1/join/guest":
			write(w, http.StatusCreated, schema.JoinGuestResponseV1{Principal: schema.PrincipalV0{ID: 9, Name: walkHandle}, Guest: walkVisit(ts),
				Credential: schema.CredentialV1{ID: 5, PrincipalID: 9, CreatedAt: ts}, Token: walkToken, Channels: []schema.InviteChannelV1{{ID: 1, Name: hostile}}})
		case "POST /v1/join/account":
			write(w, http.StatusCreated, schema.JoinAccountResponseV1{Principal: schema.PrincipalV0{ID: 9, Name: hostile}, Credential: schema.CredentialV1{ID: 5, PrincipalID: 9, CreatedAt: ts},
				Token: walkToken, Channels: []schema.InviteChannelV1{{ID: 1, Name: hostile}}})
		case "POST /v1/guest/convert":
			write(w, http.StatusOK, schema.ConvertGuestResponseV1{Principal: schema.PrincipalV0{ID: 9, Name: hostile}, Credential: schema.CredentialV1{ID: 6, PrincipalID: 9, CreatedAt: ts}, Token: walkToken})
		case "POST /v1/guest/end":
			g := walkVisit(ts)
			g.State, g.EndedAt, g.EndReason, g.Label = schema.GuestStateEnded, &ts, schema.GuestEndVisit, hostile
			write(w, http.StatusOK, schema.EndVisitResponseV1{Guest: g})
		case "POST /v1/password/mine", "PUT /v1/password/mine", "POST /v1/password/reset":
			write(w, http.StatusOK, schema.PasswordUpdatedResponseV1{Revoked: 1})
		case "POST /v1/reauth":
			write(w, http.StatusOK, schema.ReauthResponseV1{Until: ts})
		case "POST /v1/setup/redeem":
			write(w, http.StatusCreated, schema.RedeemSetupTokenResponseV1{Principal: schema.PrincipalV0{ID: 1, Name: hostile}, Credential: schema.CredentialV1{ID: 5, PrincipalID: 1, CreatedAt: ts, Scope: schema.CredentialScopeSetup},
				Token: walkToken, Auth: schema.AuthStateV1{Scope: schema.CredentialScopeSetup}})
		case "POST /v1/totp/enroll":
			write(w, http.StatusOK, schema.TOTPEnrollmentV1{Secret: hostile, Issuer: hostile, Account: hostile, Algorithm: hostile, Digits: 6, PeriodSeconds: 30, URI: hostile})
		case "POST /v1/totp/confirm", "DELETE /v1/sign-in-links/mine/9":
			w.WriteHeader(http.StatusNoContent)
		case "POST /v1/sign-in-links/mine":
			write(w, http.StatusCreated, schema.CreateSignInLinkResponseV1{SignInLink: schema.SignInLinkV1{ID: 3, PrincipalID: 42, CreatedBy: 42, CreatedAt: ts, ExpiresAt: ts, Label: hostile}, Code: walkSignInCode})
		case "GET /v1/sign-in-links/mine":
			write(w, http.StatusOK, schema.ListOwnSignInLinksResponseV1{SignInLinks: []schema.SignInLinkV1{{ID: 3, PrincipalID: 42, CreatedBy: 42, CreatedAt: ts, ExpiresAt: ts, Label: hostile}}})
		case "GET /v1/join/settings", "PUT /v1/join/settings":
			write(w, http.StatusOK, schema.JoinSettingsResponseV1{ServerName: hostile, LiveGuests: 2, Warnings: []schema.JoinWarning{schema.JoinWarning(hostile), schema.JoinWarningLiveGuestsRemain},
				Settings: schema.JoinSettingsV1{OpenJoining: schema.OpenJoiningMode(hostile), OpenChannels: []string{hostile}, MaxGuests: 5, MaxGuestsPerAddress: 1, GuestIdleSeconds: 60, GuestAbsoluteSeconds: 60}})
		case "GET /v1/guests":
			invite := int64(3)
			write(w, http.StatusOK, schema.ListGuestsResponseV1{Guests: []schema.GuestAdminV1{{ID: 9, Handle: hostile, Label: hostile, Tag: hostile, State: schema.GuestState(hostile),
				Origin: schema.GuestOrigin(hostile), InviteID: &invite, StartedAt: ts, LastActiveAt: ts, ExpiresAt: ts, IdleExpiresAt: ts}}})
		case "POST /v1/guests/9/end":
			write(w, http.StatusOK, schema.EndGuestResponseV1{Guest: schema.GuestAdminV1{ID: 9, Handle: hostile, State: schema.GuestStateEnded}})
		case "POST /v1/guests/end":
			write(w, http.StatusOK, schema.EndGuestsResponseV1{DryRun: true, Count: 1, Guests: []schema.GuestRefV1{{ID: 9, Handle: hostile}}, Remaining: 2})
		case "GET /v1/permissions", "PUT /v1/permissions":
			write(w, http.StatusOK, hostilePermissions(ts))
		case "POST /v1/guests/9/conversion-permit":
			write(w, http.StatusCreated, schema.CreateSignInLinkResponseV1{SignInLink: schema.SignInLinkV1{ID: 3, PrincipalID: 9, CreatedBy: 42, CreatedAt: ts, ExpiresAt: ts}, Code: walkSignInCode})
		default:
			write(w, http.StatusNotFound, schema.Error{Code: "no_route", Message: key + hostile})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// hostilePermissions is a permissions document whose every name is hostile:
// an unknown permission, preset, warning and role, and issued items with
// bogus ids and kinds.
func hostilePermissions(ts schema.Timestamp) schema.PermissionsResponseV1 {
	return schema.PermissionsResponseV1{
		Revision: 3,
		Rows: []schema.PermissionRowV1{
			{Permission: schema.Permission(hostile), Member: true, Operator: true, MemberFixed: true, OperatorFixed: true},
			{Permission: schema.PermUseVoice, Member: true, Operator: true},
			{Permission: schema.PermActAsMembers, Operator: true, MemberFixed: true},
		},
		Preset:    schema.PermissionPreset(hostile),
		Presets:   []schema.PermissionPresetV1{{Preset: schema.PresetPersonal, Member: []schema.Permission{schema.Permission(hostile)}}, {Preset: schema.PresetGroup, Member: []schema.Permission{schema.PermUseVoice}}},
		Editable:  []schema.PermissionRole{schema.PermissionRole(hostile)},
		Warnings:  []schema.PermissionsWarning{schema.PermissionsWarning(hostile), schema.PermissionsWarningOpenSignupOn},
		UpdatedAt: ts,
		IssuedToOthers: []schema.IssuedByOperatorV1{
			{Kind: schema.IssuedKind(hostile), ID: 7, PrincipalID: 9, IssuedBy: 42, IssuedAt: ts},
			{Kind: schema.IssuedCredential, ID: -1, PrincipalID: 9, IssuedBy: 42, IssuedAt: ts},
			{Kind: schema.IssuedHook, ID: 0, PrincipalID: 9, IssuedBy: 42, IssuedAt: ts},
			{Kind: schema.IssuedCredential, ID: 8, PrincipalID: 9, IssuedBy: 42, IssuedAt: ts},
		},
	}
}

// hostileReport is a problem report whose every string is hostile.
func hostileReport(ts schema.Timestamp) schema.ProblemReportV1 {
	return schema.ProblemReportV1{
		ID: 9, State: schema.ProblemReportState(hostile), CreatedAt: ts, PrincipalID: 42, ServerVersion: hostile,
		Voice: schema.HealthVoice(hostile), Did: hostile, Expected: hostile, Happened: hostile, Often: schema.ProblemReportOften(hostile),
		Client: schema.ProblemReportClientV1{Name: hostile, Version: hostile, UserAgent: hostile, Platform: hostile, Screen: hostile, Voice: hostile, Share: hostile},
		Diagnostics: []schema.DiagnosticEventV1{{At: ts, Kind: schema.DiagnosticEventKind(hostile), Socket: schema.DiagnosticSocket(hostile),
			Error: hostile, RequestID: hostile, State: schema.DiagnosticVoiceStateName(hostile), Name: hostile}},
		Server: schema.ProblemReportServerV1{
			Requests:     []schema.ServerRequestTraceV1{{At: ts, RequestID: hostile, Route: hostile, Status: 200}},
			SocketCloses: []schema.ServerSocketCloseV1{{At: ts, RequestID: hostile, Route: hostile, Reason: schema.SocketCloseReason(hostile)}},
		},
		Issue: "https://example.com/issues/1" + hostile,
	}
}

// Every plain command is run against a server whose every string is hostile,
// once with well-formed answers and once with errors, and nothing it prints,
// the final error line included, may carry a byte or character a terminal
// would act on (issue #234).
func TestPlainCommandsNeverPrintRawServerText(t *testing.T) {
	cases := walkCases(t)

	// A command added to the usage text without a case here is noticed.
	known := make(map[string]bool)
	for _, c := range cases {
		known[c.key] = true
	}
	for _, key := range usageCommands(t) {
		if _, exempt := walkExempt[key]; !known[key] && !exempt {
			t.Errorf("usage lists %q but the walk neither runs nor exempts it: add it to walkCases", key)
		}
	}
	for key := range walkExempt {
		if known[key] {
			t.Errorf("%q is both run and exempt", key)
		}
	}

	for _, mode := range []string{"ok", "error"} {
		for _, c := range cases {
			t.Run(mode+"/"+c.key, func(t *testing.T) {
				isolateConfig(t)
				t.Setenv("CONCH_TOKEN", "tok")
				if c.key == "login" {
					t.Setenv("CONCH_TOKEN", "")
				}
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				srv := hostileServer(t, mode == "ok", c.whoamiOK)
				var stdout, stderr bytes.Buffer
				err := RunWithStdin(context.Background(), c.args(srv.URL, dir), strings.NewReader(c.stdin), &stdout, &stderr, "vtest")
				all := stdout.String() + stderr.String()
				if err != nil {
					// What cmd/conch prints for the returned error.
					all += "conch: " + ErrorLine(err) + "\n"
				}
				if mode == "ok" && err != nil {
					t.Fatalf("well-formed answers failed: %v", err)
				}
				if mode == "error" && err == nil {
					t.Fatalf("every route refuses, yet the command succeeded; stdout=%q", stdout.String())
				}
				if !utf8.ValidString(all) {
					t.Errorf("output is not valid UTF-8: %q", all)
				}
				if bad := firstUnsafe(all); bad != "" {
					t.Errorf("%s in output %q", bad, all)
				}
				// The hostile text did reach the output, as an escape: the
				// check above is not passing because nothing was printed.
				if !strings.Contains(all, `\u202e`) && (mode == "error" || !c.quietWhenOK) {
					t.Errorf("no escaped hostile text in the output %q", all)
				}
			})
		}
	}
}

func TestWalkSeesHostileTextWhereItIsMeant(t *testing.T) {
	// A control on the walk itself: the same server through a raw copy of the
	// body reaches the scan, so the scan can fail.
	if firstUnsafe("a"+esc+"b") == "" || firstUnsafe("a"+rlo) == "" || firstUnsafe("a"+c1) == "" || firstUnsafe("a"+del) == "" || firstUnsafe("a\tb") == "" {
		t.Fatal("the scan misses a hostile character")
	}
	if firstUnsafe("a\nb é"+zwj) != "" {
		t.Fatal("the scan rejects ordinary text")
	}
}

func TestServerErrorTextIsEscaped(t *testing.T) {
	err := &ServerError{Status: 403, Code: "c" + esc, Message: "m" + rlo + crlf}
	if bad := firstUnsafe(err.Error()); bad != "" {
		t.Errorf("ServerError.Error() = %q: %s", err.Error(), bad)
	}
	if !strings.Contains(err.Error(), `\x1b`) || !strings.Contains(err.Error(), `\u202e`) {
		t.Errorf("ServerError.Error() = %q, want visible escapes", err.Error())
	}
}

var _ = regexp.MustCompile // keep imports honest if the scan grows
