package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

var notificationsT0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// credentialOf makes a live credential for p and returns its id.
func credentialOf(t *testing.T, s *Store, p Principal, label string) int64 {
	t.Helper()
	c, _, err := s.CreateCredential(context.Background(), "system", p.ID, label, nil)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	return c.ID
}

func putPrefs(t *testing.T, s *Store, credentialID int64, prefs schema.NotificationPreferencesV1) {
	t.Helper()
	if _, err := s.PutNotificationPreferences(context.Background(), credentialID, prefs, notificationsT0); err != nil {
		t.Fatalf("PutNotificationPreferences: %v", err)
	}
}

func prefsRows(t *testing.T, s *Store, credentialID int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM notification_preferences WHERE credential_id = ?", credentialID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNotificationPreferences(t *testing.T) {
	s, _, human := credentialFixture(t)
	ctx := context.Background()
	a := credentialOf(t, s, human, "phone")
	b := credentialOf(t, s, human, "desk")

	t.Run("a fresh credential has the defaults", func(t *testing.T) {
		got, err := s.GetNotificationPreferences(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if want := schema.DefaultNotificationPreferences(); !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
		raw, _ := json.Marshal(got)
		if want := `{"policy":"off","muted":[],"preview":false,"quiet_when_unreachable":false}`; string(raw) != want {
			t.Errorf("wire = %s, want %s", raw, want)
		}
	})

	tests := []struct {
		name    string
		in      schema.NotificationPreferencesV1
		want    schema.NotificationPreferencesV1
		refused bool
	}{
		{"replace with everything on", schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{3, 1}, Preview: true, QuietWhenUnreachable: true},
			schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{1, 3}, Preview: true, QuietWhenUnreachable: true}, false},
		{"mutes are deduplicated and ascending", schema.NotificationPreferencesV1{Policy: "mentions", Muted: []int64{9, 2, 9, 2, 5}},
			schema.NotificationPreferencesV1{Policy: "mentions", Muted: []int64{2, 5, 9}}, false},
		{"a mute is not checked against membership", schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{987654}},
			schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{987654}}, false},
		{"an empty list reads back as [] not null", schema.NotificationPreferencesV1{Policy: "off", Muted: []int64{}},
			schema.NotificationPreferencesV1{Policy: "off", Muted: []int64{}}, false},
		{"the replace keeps nothing of before", schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{}},
			schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{}}, false},
		{"256 mutes", schema.NotificationPreferencesV1{Policy: "all", Muted: idRange(1, 256)},
			schema.NotificationPreferencesV1{Policy: "all", Muted: idRange(1, 256)}, false},
		{"257 mutes", schema.NotificationPreferencesV1{Policy: "all", Muted: idRange(1, 257)}, schema.NotificationPreferencesV1{}, true},
		{"a non-positive id", schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{4, 0}}, schema.NotificationPreferencesV1{}, true},
		{"a negative id", schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{-1}}, schema.NotificationPreferencesV1{}, true},
		{"no policy", schema.NotificationPreferencesV1{Muted: []int64{}}, schema.NotificationPreferencesV1{}, true},
		{"an unknown policy", schema.NotificationPreferencesV1{Policy: "everything", Muted: []int64{}}, schema.NotificationPreferencesV1{}, true},
		{"a null muted list", schema.NotificationPreferencesV1{Policy: "all"}, schema.NotificationPreferencesV1{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, err := s.GetNotificationPreferences(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.PutNotificationPreferences(ctx, a, tt.in, notificationsT0)
			if tt.refused {
				if err == nil {
					t.Fatal("the write was accepted")
				}
				if strings.Contains(err.Error(), "everything") {
					t.Errorf("the refusal echoes the value: %v", err)
				}
				if after, _ := s.GetNotificationPreferences(ctx, a); !reflect.DeepEqual(after, before) {
					t.Errorf("a refused write stored %+v over %+v", after, before)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("answer = %+v, want %+v", got, tt.want)
			}
			if stored, err := s.GetNotificationPreferences(ctx, a); err != nil || !reflect.DeepEqual(stored, tt.want) {
				t.Errorf("stored = %+v, %v; want %+v", stored, err, tt.want)
			}
		})
	}

	t.Run("a second credential of the same principal has its own", func(t *testing.T) {
		putPrefs(t, s, a, schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{7}, Preview: true})
		got, err := s.GetNotificationPreferences(ctx, b)
		if err != nil || !reflect.DeepEqual(got, schema.DefaultNotificationPreferences()) {
			t.Errorf("other credential = %+v, %v; want the defaults", got, err)
		}
		putPrefs(t, s, b, schema.NotificationPreferencesV1{Policy: "mentions", Muted: []int64{}})
		if got, _ := s.GetNotificationPreferences(ctx, a); got.Policy != "all" || !got.Preview {
			t.Errorf("writing b changed a: %+v", got)
		}
	})

	t.Run("a revoked credential is not written", func(t *testing.T) {
		c := credentialOf(t, s, human, "gone")
		if err := s.RevokeCredentialOf(ctx, "principal:test", human.ID, c); err != nil {
			t.Fatal(err)
		}
		_, err := s.PutNotificationPreferences(ctx, c, schema.DefaultNotificationPreferences(), notificationsT0)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("write to a revoked credential = %v, want ErrNotFound", err)
		}
		if n := prefsRows(t, s, c); n != 0 {
			t.Errorf("%d rows for a dead credential", n)
		}
	})
}

func idRange(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// Every way a credential is revoked removes its preferences in the same
// transaction, and a revocation that is refused leaves them.
func TestRevocationEntryPointsEndNotifications(t *testing.T) {
	ctx := context.Background()
	prefs := schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{1}, Preview: true}
	type world struct {
		s     *Store
		human Principal
		cred  int64
		other int64 // another credential of the same principal, never revoked by the entry
	}
	newWorld := func(t *testing.T) world {
		s, _, human := credentialFixture(t)
		w := world{s: s, human: human, cred: credentialOf(t, s, human, "phone"), other: credentialOf(t, s, human, "desk")}
		putPrefs(t, s, w.cred, prefs)
		putPrefs(t, s, w.other, prefs)
		return w
	}
	tests := []struct {
		name string
		do   func(t *testing.T, w world) error
		// othersToo is true for an entry that ends every credential.
		othersToo bool
	}{
		{"sign out (own credential)", func(t *testing.T, w world) error {
			return w.s.RevokeOwnCredential(ctx, "principal:test", w.cred)
		}, false},
		{"end a sign-in by id", func(t *testing.T, w world) error {
			return w.s.RevokeCredentialOf(ctx, "principal:test", w.human.ID, w.cred)
		}, false},
		{"operator revoke", func(t *testing.T, w world) error {
			return w.s.RevokeCredential(ctx, "principal:test", w.cred)
		}, false},
		{"rotate", func(t *testing.T, w world) error {
			_, _, err := w.s.RotateCredential(ctx, "principal:test", w.cred)
			return err
		}, false},
		{"revoke all", func(t *testing.T, w world) error {
			_, err := w.s.RevokeAllCredentials(ctx, "principal:test", w.human.ID)
			return err
		}, true},
		{"disable", func(t *testing.T, w world) error {
			_, err := w.s.DisablePrincipal(ctx, "principal:test", w.human.ID)
			return err
		}, true},
		{"restore invalidates every credential without revoking one", func(t *testing.T, w world) error {
			_, err := w.s.InvalidateRestored(ctx, time.Now())
			return err
		}, true},
		{"bootstrap revokes every live credential", func(t *testing.T, w world) error {
			_, err := w.s.BootstrapOperatorWith(ctx, "root", BootstrapOptions{})
			return err
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			if err := tt.do(t, w); err != nil {
				t.Fatal(err)
			}
			if n := prefsRows(t, w.s, w.cred); n != 0 {
				t.Errorf("the revoked credential kept %d preference rows", n)
			}
			if n := prefsRows(t, w.s, w.other); (n == 0) != tt.othersToo {
				t.Errorf("the principal's other credential has %d rows; the entry ends them = %v", n, tt.othersToo)
			}
		})
	}

	t.Run("a refused revocation keeps them", func(t *testing.T) {
		s := openTestStore(t)
		op, cred, _, err := s.BootstrapOperator(ctx, "root")
		if err != nil {
			t.Fatal(err)
		}
		putPrefs(t, s, cred.ID, prefs)
		for name, do := range map[string]func() error{
			"end by id":    func() error { return s.RevokeCredentialOf(ctx, "principal:test", op.ID, cred.ID) },
			"own sign-out": func() error { return s.RevokeOwnCredential(ctx, "principal:test", cred.ID) },
		} {
			if err := do(); !errors.Is(err, ErrLastOperator) {
				t.Fatalf("%s = %v, want ErrLastOperator", name, err)
			}
			if n := prefsRows(t, s, cred.ID); n != 1 {
				t.Errorf("%s: a refused revocation left %d rows, want 1", name, n)
			}
		}
	})
}

// A credential that expired, or went idle past its bound, is not revoked; the
// session sweep removes its preferences.
func TestSweepRemovesPreferencesOfDeadCredentials(t *testing.T) {
	s, _, human := credentialFixture(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	soon := now.Add(time.Hour)
	expired, _, err := s.CreateCredential(ctx, "system", human.ID, "expiring", &soon)
	if err != nil {
		t.Fatal(err)
	}
	idle := credentialOf(t, s, human, "idle")
	live := credentialOf(t, s, human, "live")
	idleLive := credentialOf(t, s, human, "idle but used")
	if _, err := s.db.Exec("UPDATE credentials SET idle_ms = 60000, last_used_at = ? WHERE id = ?", now.Add(-time.Hour).UnixMilli(), idle); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE credentials SET idle_ms = 60000, last_used_at = ? WHERE id = ?", now.UnixMilli(), idleLive); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{expired.ID, idle, live, idleLive} {
		putPrefs(t, s, id, schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{}})
	}
	// Before the expiry: only the idle one goes.
	if _, err := s.SweepSessions(ctx, now, time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		id   int64
		rows int
	}{{"expiring, not yet", expired.ID, 1}, {"idle past its bound", idle, 0}, {"no bound", live, 1}, {"idle bound, used now", idleLive, 1}} {
		if n := prefsRows(t, s, c.id); n != c.rows {
			t.Errorf("%s: %d rows, want %d", c.name, n, c.rows)
		}
	}
	// A generation-dead credential (not revoked: its principal's generation
	// moved on) loses its row at the next sweep.
	if _, err := s.db.Exec("UPDATE principals SET auth_generation = auth_generation + 1 WHERE id = ?", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepSessions(ctx, now, time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{live, idleLive} {
		if n := prefsRows(t, s, id); n != 0 {
			t.Errorf("generation-dead credential %d kept %d rows", id, n)
		}
	}
	// After it: the expired one goes too, and the rest stay.
	if _, err := s.SweepSessions(ctx, now.Add(2*time.Hour), time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := prefsRows(t, s, expired.ID); n != 0 {
		t.Errorf("expired credential kept %d rows", n)
	}
	if n := prefsRows(t, s, live); n != 0 {
		t.Errorf("a generation-dead credential has %d rows, want 0", n)
	}
}

// revokesCredentialSQL matches a statement that sets credentials.revoked_at,
// whatever its spelling: case, line breaks, spacing, other columns set first.
var revokesCredentialSQL = regexp.MustCompile(`(?is)update\s+credentials\s+set\b.*?\brevoked_at\s*=`)

// TestEveryCredentialRevocationEndsItsNotifications is the structural half of
// "preferences die with the credential": every function in this package that
// sets credentials.revoked_at must also call endCredentialNotificationsTx, so
// a new revocation site cannot skip the helper. The check is on the function,
// because the helper is called in the same transaction as the update.
func TestEveryCredentialRevocationEndsItsNotifications(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) // #nosec G304 -- this package's own source files
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			revokes, ends := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind == token.STRING && revokesCredentialSQL.MatchString(x.Value) {
						revokes = true
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "endCredentialNotificationsTx" {
						ends = true
					}
				}
				return true
			})
			if !revokes {
				continue
			}
			sites++
			if !ends {
				t.Errorf("%s: %s sets credentials.revoked_at and does not call endCredentialNotificationsTx", path, fn.Name.Name)
			}
		}
	}
	// The sites known when this was written: if the walk finds far fewer,
	// it is not looking at the right files.
	if sites < 8 {
		t.Errorf("found %d revocation sites, want at least 8: the check is not finding them", sites)
	}
}

// --- the feed --------------------------------------------------------------

// feedFixture is the audience fixture with dave as the reader, a second
// channel he is in, and a phone credential of his.
type feedFixture struct {
	*audienceFixture
	reader Reader
	cred   int64
}

func newFeedFixture(t *testing.T) *feedFixture {
	t.Helper()
	f := &feedFixture{audienceFixture: newAudienceFixture(t)}
	f.reader = Reader{PrincipalID: f.dave.ID, Scoped: true}
	f.cred = credentialOf(t, f.s, f.dave, "phone")
	return f
}

func (f *feedFixture) policy(t *testing.T, policy schema.NotificationPolicy, preview bool, muted ...int64) {
	t.Helper()
	if muted == nil {
		muted = []int64{}
	}
	putPrefs(t, f.s, f.cred, schema.NotificationPreferencesV1{Policy: policy, Muted: muted, Preview: preview})
}

func (f *feedFixture) feed(t *testing.T, after int64) schema.ListPendingNotificationsResponseV1 {
	t.Helper()
	page, err := f.s.ListPendingNotifications(context.Background(), f.reader, f.cred, after)
	if err != nil {
		t.Fatalf("ListPendingNotifications: %v", err)
	}
	if err := page.Validate(); err != nil {
		t.Fatalf("the page is not valid: %v", err)
	}
	return page
}

// post writes a channel-wide message to channelID.
func (f *feedFixture) post(t *testing.T, channelID int64, author Principal, body string) int64 {
	t.Helper()
	m, err := f.s.InsertMessageV1(context.Background(), channelID, author.ID, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func idKinds(page schema.ListPendingNotificationsResponseV1) []string {
	out := []string{}
	for _, s := range page.Summaries {
		out = append(out, fmt.Sprintf("%d:%s", s.MessageID, s.Kind))
	}
	return out
}

func TestPendingNotificationsPolicies(t *testing.T) {
	f := newFeedFixture(t)
	ctx := context.Background()
	if _, err := f.s.AddChannelMember(ctx, "system", f.other.ID, f.dave.ID, 0); err != nil {
		t.Fatal(err)
	}
	m := map[string]int64{}
	m["hello"] = f.post(t, f.ch.ID, f.frank, "hello there")
	m["mention"] = f.post(t, f.ch.ID, f.frank, "ping @dave please")
	m["net"], _ = f.toNet(t, f.alice, f.n1, "net one")
	m["hiddenNet"], _ = f.toNet(t, f.erin, f.n2, "net two @dave")
	m["whisper"] = f.whisper(t, f.erin, "psst", f.dave)
	m["hiddenWhisper"] = f.whisper(t, f.erin, "psst @dave", f.frank)
	m["whisperMention"] = f.whisper(t, f.alice, "hi @dave", f.dave)
	m["own"] = f.post(t, f.ch.ID, f.dave, "@dave note to self")
	m["removed"] = f.post(t, f.ch.ID, f.frank, "@dave removed")
	m["netMention"], _ = f.toNet(t, f.alice, f.n1, "net @dave")
	m["elsewhere"] = f.post(t, f.other.ID, f.frank, "in the other channel")
	if _, err := f.s.db.Exec("INSERT INTO message_removals (message_id, channel_id, removed_at) VALUES (?, ?, 1)", m["removed"], f.ch.ID); err != nil {
		t.Fatal(err)
	}
	k := func(name string, kind schema.NotificationKind) string { return fmt.Sprintf("%d:%s", m[name], kind) }

	tests := []struct {
		name   string
		policy schema.NotificationPolicy
		muted  []int64
		want   []string
	}{
		{"off is empty", schema.NotificationPolicyOff, nil, []string{}},
		{"mentions", schema.NotificationPolicyMentions, nil, []string{
			k("mention", "mention"), k("whisper", "whisper"), k("whisperMention", "whisper"), k("netMention", "mention")}},
		{"all", schema.NotificationPolicyAll, nil, []string{
			k("hello", "message"), k("mention", "mention"), k("net", "message"), k("whisper", "whisper"),
			k("whisperMention", "whisper"), k("netMention", "mention"), k("elsewhere", "message")}},
		{"a muted channel is absent", schema.NotificationPolicyAll, []int64{f.other.ID}, []string{
			k("hello", "message"), k("mention", "mention"), k("net", "message"), k("whisper", "whisper"),
			k("whisperMention", "whisper"), k("netMention", "mention")}},
		{"muting a channel the reader is not in changes nothing", schema.NotificationPolicyMentions, []int64{987654}, []string{
			k("mention", "mention"), k("whisper", "whisper"), k("whisperMention", "whisper"), k("netMention", "mention")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.policy(t, tt.policy, false, tt.muted...)
			page := f.feed(t, 0)
			if got := idKinds(page); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("feed = %v\nwant   %v", got, tt.want)
			}
			// Everything examined, so nothing beyond.
			if page.More {
				t.Error("more is true with nothing left")
			}
			if tt.policy == schema.NotificationPolicyOff && page.Cursor != 0 {
				t.Errorf("an empty feed's cursor = %d, want after (0)", page.Cursor)
			}
			for _, s := range page.Summaries {
				if s.Text != "" {
					t.Errorf("text %q without preview", s.Text)
				}
				if s.AuthorID <= 0 || s.ChannelName == "" || s.Schema != schema.NotificationSummarySchemaV1 {
					t.Errorf("summary %+v is missing its identity", s)
				}
			}
		})
	}

	t.Run("a message by the reader's own name is absent for the author too", func(t *testing.T) {
		// frank sees dave's @dave note as a message to someone else, not a mention of him.
		frank := Reader{PrincipalID: f.frank.ID, Scoped: true}
		fc := credentialOf(t, f.s, f.frank, "phone")
		putPrefs(t, f.s, fc, schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{}})
		page, err := f.s.ListPendingNotifications(ctx, frank, fc, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page.Summaries {
			if s.AuthorID == f.frank.ID {
				t.Errorf("the reader's own message %d is in their feed", s.MessageID)
			}
			if s.MessageID == m["hiddenNet"] {
				t.Errorf("a net message frank is not on is in his feed")
			}
		}
	})

	t.Run("the feed needs a scoped reader with a principal", func(t *testing.T) {
		for _, r := range []Reader{{}, {PrincipalID: f.dave.ID}, {Scoped: true}} {
			if _, err := f.s.ListPendingNotifications(ctx, r, f.cred, 0); err == nil {
				t.Errorf("reader %+v was accepted", r)
			}
		}
	})
}

// A message that the read state counts as unread is in the feed under "all",
// and one it does not is not; after and the read mark both bound the start.
func TestPendingNotificationsStartWhereUnreadStarts(t *testing.T) {
	f := newFeedFixture(t)
	ctx := context.Background()
	f.policy(t, schema.NotificationPolicyAll, false)
	var posted []int64
	for i := range 6 {
		posted = append(posted, f.post(t, f.ch.ID, f.frank, fmt.Sprintf("m%d", i)))
	}
	posted = append(posted, f.post(t, f.ch.ID, f.dave, "mine"))
	tests := []struct {
		name  string
		mark  int64
		after int64
		want  []int64
	}{
		{"never marked, after 0", 0, 0, posted[:6]},
		{"after above the mark", posted[1], posted[3], posted[4:6]},
		{"mark above after", posted[3], posted[1], posted[4:6]},
		{"after past everything", 0, posted[6] + 100, nil},
		{"mark at the newest", posted[6], 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.s.db.Exec("DELETE FROM read_marks"); err != nil {
				t.Fatal(err)
			}
			if tt.mark > 0 {
				if _, err := f.s.MarkRead(ctx, f.ch.ID, f.reader, tt.mark, notificationsT0); err != nil {
					t.Fatal(err)
				}
			}
			page := f.feed(t, tt.after)
			got := []int64{}
			for _, s := range page.Summaries {
				got = append(got, s.MessageID)
			}
			if want := append([]int64{}, tt.want...); !reflect.DeepEqual(got, want) {
				t.Errorf("feed = %v, want %v", got, want)
			}
			// The read state's own count, over the same bound, agrees.
			start := max(tt.mark, tt.after)
			unread, err := f.s.CountVisibleUnreadMessagesAfter(ctx, f.ch.ID, f.reader, start, 100)
			if err != nil {
				t.Fatal(err)
			}
			if unread != len(got) {
				t.Errorf("the feed holds %d, the unread count after %d is %d", len(got), start, unread)
			}
		})
	}
}

// Access lost between the post and the fetch empties the feed.
func TestPendingNotificationsFollowAccess(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		lose func(t *testing.T, f *feedFixture)
	}{
		{"removed from the channel", func(t *testing.T, f *feedFixture) {
			if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.dave.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"banned from the channel", func(t *testing.T, f *feedFixture) {
			if _, err := f.s.BanChannelMember(ctx, "system", f.ch.ID, f.dave.ID, 0); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFeedFixture(t)
			f.policy(t, schema.NotificationPolicyAll, false)
			f.post(t, f.ch.ID, f.frank, "wide")
			f.toNet(t, f.alice, f.n1, "net")
			f.whisper(t, f.erin, "psst", f.dave)
			if got := len(f.feed(t, 0).Summaries); got != 3 {
				t.Fatalf("before: %d summaries, want 3", got)
			}
			tt.lose(t, f)
			page := f.feed(t, 0)
			if len(page.Summaries) != 0 || page.More || page.Cursor != 0 {
				t.Errorf("after losing access: %+v, want an empty feed", page)
			}
		})
	}

	t.Run("disabled", func(t *testing.T) {
		f := newFeedFixture(t)
		f.policy(t, schema.NotificationPolicyAll, false)
		f.whisper(t, f.erin, "psst", f.dave)
		wide := f.post(t, f.ch.ID, f.frank, "wide")
		if _, err := f.s.DisablePrincipal(ctx, "system", f.dave.ID); err != nil {
			t.Fatal(err)
		}
		// The credential is gone with the principal's access, so the preferences
		// are too: the feed is the defaults, which is off.
		if page := f.feed(t, 0); len(page.Summaries) != 0 {
			t.Errorf("a disabled principal's feed = %v (wide %d)", idKinds(page), wide)
		}
	})
}

// The feed is never a function of messages the reader cannot see. The same
// visible messages, with and without a private channel the reader is not in,
// a whisper not to them and a net they are not on between them, give the same
// bytes: summaries, cursor and more, under every policy and for every after.
func TestPendingNotificationsHiddenMessagesChangeNothing(t *testing.T) {
	ctx := context.Background()
	type visible struct {
		id      int64
		channel int64
		author  int64
		body    string
	}
	// World A: visible and hidden messages interleaved, through the real
	// writers. World B holds the same visible messages at the same ids and
	// none of the hidden ones; it is built from A's record of them.
	build := func(t *testing.T, world string) (*feedFixture, []visible) {
		f := newFeedFixture(t)
		secret, err := f.s.CreateChannel(ctx, "secret")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.AddChannelMember(ctx, "system", secret.ID, f.erin.ID, 0); err != nil {
			t.Fatal(err)
		}
		var vis []visible
		next := int64(1)
		put := func(channel int64, author Principal, body string) {
			vis = append(vis, visible{next, channel, author.ID, body})
			next++
			if world == "A" {
				f.post(t, channel, author, body)
			}
		}
		hidden := func(do func()) {
			if world == "A" {
				do()
			}
			next++ // the id is taken in A; B leaves the gap
		}
		// By id: 1 visible, 2-4 hidden, 5 visible mention, 6 hidden, 7-8 visible.
		put(f.ch.ID, f.frank, "first")
		hidden(func() { f.post(t, secret.ID, f.erin, "secret @dave one") })
		hidden(func() { f.whisper(t, f.erin, "whisper @dave", f.frank) })
		hidden(func() { f.toNet(t, f.erin, f.n2, "net two @dave") })
		put(f.ch.ID, f.frank, "look @dave")
		hidden(func() { f.post(t, secret.ID, f.erin, "secret two") })
		put(f.ch.ID, f.frank, "seventh")
		put(f.ch.ID, f.alice, "eighth @dave")
		return f, vis
	}
	a, vis := build(t, "A")
	b, _ := build(t, "B")
	for _, v := range vis {
		// The same time as A's, so that the bytes can be equal.
		var at int64
		if err := a.s.db.QueryRow("SELECT created_at FROM messages WHERE id = ?", v.id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if _, err := b.s.db.Exec("INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (?, ?, ?, ?, ?)",
			v.id, v.channel, v.author, v.body, at); err != nil {
			t.Fatal(err)
		}
	}
	// The ids line up: A's visible rows are exactly the ones B holds.
	for _, v := range vis {
		var body string
		if err := a.s.db.QueryRow("SELECT body FROM messages WHERE id = ? AND channel_id = ?", v.id, v.channel).Scan(&body); err != nil || body != v.body {
			t.Fatalf("world A id %d = %q, %v; want %q: the worlds do not line up", v.id, body, err, v.body)
		}
	}

	for _, policy := range []schema.NotificationPolicy{schema.NotificationPolicyAll, schema.NotificationPolicyMentions, schema.NotificationPolicyOff} {
		for _, preview := range []bool{false, true} {
			for _, after := range []int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 500} {
				name := fmt.Sprintf("%s/preview=%v/after=%d", policy, preview, after)
				t.Run(name, func(t *testing.T) {
					a.policy(t, policy, preview)
					b.policy(t, policy, preview)
					ra, rb := a.feed(t, after), b.feed(t, after)
					ja, _ := json.Marshal(ra)
					jb, _ := json.Marshal(rb)
					if string(ja) != string(jb) {
						t.Errorf("with hidden messages:    %s\nwithout hidden messages: %s", ja, jb)
					}
				})
			}
		}
	}
}

// What the hidden messages cost is nothing: not a position, not the bound.
// 2,500 messages in a channel the reader is not in sit before one that
// mentions them, and the answer is that one, with nothing more.
func TestPendingNotificationsHiddenMessagesAreNotCounted(t *testing.T) {
	f := newFeedFixture(t)
	ctx := context.Background()
	f.policy(t, schema.NotificationPolicyMentions, false)
	secret, err := f.s.CreateChannel(ctx, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AddChannelMember(ctx, "system", secret.ID, f.erin.ID, 0); err != nil {
		t.Fatal(err)
	}
	bulkInsert(t, f.s, secret.ID, f.erin.ID, 2500, "hidden @dave")
	// And whispers not to the reader.
	for range 3 {
		f.whisper(t, f.erin, "whisper @dave", f.frank)
	}
	want := f.post(t, f.ch.ID, f.frank, "hey @dave")
	page := f.feed(t, 0)
	if len(page.Summaries) != 1 || page.Summaries[0].MessageID != want || page.Cursor != want || page.More {
		t.Errorf("page = %+v, want one summary %d, cursor %d, no more", page, want, want)
	}
	// And nothing visible at all: the cursor stays where it was asked from.
	empty := f.feed(t, want)
	if len(empty.Summaries) != 0 || empty.Cursor != want || empty.More {
		t.Errorf("past everything: %+v, want an empty page at cursor %d", empty, want)
	}
}

// bulkInsert writes n channel-wide messages in one transaction.
func bulkInsert(t *testing.T, s *Store, channelID, authorID int64, n int, body string) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := tx.Exec("INSERT INTO messages (channel_id, author_id, body, created_at) VALUES (?, ?, ?, ?)",
			channelID, authorID, fmt.Sprintf("%s %d", body, i), notificationsT0.UnixMilli()+int64(i)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// 45 qualifying messages across three channels, with more posted between page
// fetches: following the cursor while more is set delivers every one exactly
// once, ascending, with no gap, and more is right on every page.
func TestPendingNotificationsPaging(t *testing.T) {
	f := newFeedFixture(t)
	ctx := context.Background()
	f.policy(t, schema.NotificationPolicyAll, false)
	chans := []int64{f.ch.ID, f.other.ID}
	third, err := f.s.CreateChannel(ctx, "third")
	if err != nil {
		t.Fatal(err)
	}
	chans = append(chans, third.ID)
	for _, c := range chans[1:] {
		if _, err := f.s.AddChannelMember(ctx, "system", c, f.dave.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	var all []int64
	postN := func(n int) {
		for i := range n {
			all = append(all, f.post(t, chans[i%3], f.frank, fmt.Sprintf("m%d", len(all))))
			// Something that never qualifies, between every one.
			f.post(t, chans[i%3], f.dave, "own")
		}
	}
	postN(45)
	var got []int64
	after := int64(0)
	for page := 1; ; page++ {
		resp := f.feed(t, after)
		for _, s := range resp.Summaries {
			got = append(got, s.MessageID)
		}
		remaining := 0
		for _, id := range all {
			if id > resp.Cursor {
				remaining++
			}
		}
		if resp.More != (remaining > 0) {
			t.Fatalf("page %d: more = %v with %d qualifying messages above the cursor", page, resp.More, remaining)
		}
		if resp.More && len(resp.Summaries) != schema.MaxPendingNotificationsPage {
			t.Fatalf("page %d: more is set on a page of %d", page, len(resp.Summaries))
		}
		if resp.Cursor <= after && resp.More {
			t.Fatalf("page %d: no progress: cursor %d after %d", page, resp.Cursor, after)
		}
		after = resp.Cursor
		if page == 1 {
			postN(5) // arrives between fetches
		}
		if !resp.More {
			break
		}
		if page > 10 {
			t.Fatal("the feed does not end")
		}
	}
	if !reflect.DeepEqual(got, all) {
		t.Errorf("delivered %v\nposted    %v", got, all)
	}
	if len(all) != 50 {
		t.Fatalf("posted %d, want 50", len(all))
	}
}

// A long run of candidates that do not match is examined, bounded, and still
// makes progress: the cursor moves to the highest examined, and more is set.
func TestPendingNotificationsScanBound(t *testing.T) {
	f := newFeedFixture(t)
	f.policy(t, schema.NotificationPolicyMentions, false)
	bulkInsert(t, f.s, f.ch.ID, f.frank.ID, 2500, "chatter")
	last := f.post(t, f.ch.ID, f.frank, "finally, @dave")

	first := f.feed(t, 0)
	if len(first.Summaries) != 0 || !first.More {
		t.Fatalf("first page = %+v, want empty with more", first)
	}
	// The cursor is the id of the 2,000th candidate, which is a visible
	// message: the 2,000th row of the channel.
	var wantCursor int64
	if err := f.s.db.QueryRow("SELECT id FROM messages ORDER BY id LIMIT 1 OFFSET ?", PendingNotificationScanBound-1).Scan(&wantCursor); err != nil {
		t.Fatal(err)
	}
	if first.Cursor != wantCursor {
		t.Errorf("cursor = %d, want the 2000th candidate %d", first.Cursor, wantCursor)
	}
	second := f.feed(t, first.Cursor)
	if len(second.Summaries) != 1 || second.Summaries[0].MessageID != last || second.More || second.Cursor != last {
		t.Errorf("second page = %+v, want the mention %d and the end", second, last)
	}
}

func TestPendingNotificationsNamesAndText(t *testing.T) {
	ctx := context.Background()
	f := newFeedFixture(t)
	long := strings.Repeat("é", 200)
	exactly160 := strings.Repeat("x", 160)
	guestHandle := schema.GuestNamePrefix + strings.Repeat("a", schema.GuestHandleLength-len(schema.GuestNamePrefix))
	m := map[string]int64{
		"plain":     f.post(t, f.ch.ID, f.frank, "  hello \t\n  there  world  "),
		"long":      f.post(t, f.ch.ID, f.frank, long),
		"exact":     f.post(t, f.ch.ID, f.frank, exactly160),
		"spoiler":   f.post(t, f.ch.ID, f.frank, "the butler ||did it||"),
		"spoilfile": f.post(t, f.ch.ID, f.frank, "see attached"),
		"blank":     f.post(t, f.ch.ID, f.frank, " \n\t "),
		"guest":     f.post(t, f.ch.ID, f.frank, "i am a guest"),
		"whisper":   f.whisper(t, f.erin, "private words", f.dave),
	}
	if _, err := f.s.db.Exec("INSERT INTO files (channel_id, uploader_id, filename, media_type, size, sha256, created_at, message_id) VALUES (?, ?, ?, 'image/png', 1, ?, 1, ?)",
		f.ch.ID, f.frank.ID, schema.SpoilerFilePrefix+"cat.png", strings.Repeat("0", 64), m["spoilfile"]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("INSERT INTO message_guest_attribution (message_id, handle, label) VALUES (?, ?, 'Visitor')", m["guest"], guestHandle); err != nil {
		t.Fatal(err)
	}

	byID := func(page schema.ListPendingNotificationsResponseV1) map[int64]schema.NotificationSummaryV1 {
		out := map[int64]schema.NotificationSummaryV1{}
		for _, s := range page.Summaries {
			out[s.MessageID] = s
		}
		return out
	}

	t.Run("preview off carries no text", func(t *testing.T) {
		f.policy(t, schema.NotificationPolicyAll, false)
		for id, s := range byID(f.feed(t, 0)) {
			if s.Text != "" {
				t.Errorf("message %d: text %q with preview off", id, s.Text)
			}
		}
	})

	f.policy(t, schema.NotificationPolicyAll, true)
	page := f.feed(t, 0)
	got := byID(page)
	cut := strings.Repeat("é", 159) + "…"
	tests := []struct {
		name string
		id   int64
		text string
	}{
		{"whitespace collapsed and trimmed", m["plain"], "hello there world"},
		{"cut to 159 and an ellipsis", m["long"], cut},
		{"exactly 160 is not cut", m["exact"], exactly160},
		{"a body with || has no text", m["spoiler"], ""},
		{"a SPOILER_ attachment has no text", m["spoilfile"], ""},
		{"no text, no text", m["blank"], ""},
		{"a whisper to the reader carries its text", m["whisper"], "private words"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok := got[tt.id]
			if !ok {
				t.Fatalf("message %d is not in the feed", tt.id)
			}
			if s.Text != tt.text {
				t.Errorf("text = %q, want %q", s.Text, tt.text)
			}
			if n := len([]rune(s.Text)); n > schema.MaxNotificationTextChars {
				t.Errorf("text has %d characters", n)
			}
		})
	}

	t.Run("author names", func(t *testing.T) {
		if s := got[m["plain"]]; s.AuthorID != f.frank.ID || s.AuthorName != "frank" {
			t.Errorf("a member's post: author %d %q, want frank", s.AuthorID, s.AuthorName)
		}
		// A guest's post is the attribution it carries, and author_id is sent.
		s := got[m["guest"]]
		if want := schema.GuestDisplayName("Visitor", guestHandle); s.AuthorName != want || s.AuthorID != f.frank.ID {
			t.Errorf("a guest's post: author %d %q, want %q", s.AuthorID, s.AuthorName, want)
		}
		if s := got[m["whisper"]]; s.AuthorName != "erin" || s.Kind != schema.NotificationKindWhisper {
			t.Errorf("a whisper: %+v", s)
		}
		// The name comes by the authors rule, and says what that route says.
		for _, s := range page.Summaries {
			if s.AuthorName == "" {
				t.Errorf("message %d has no author name", s.MessageID)
				continue
			}
			authors, err := f.s.ListChannelAuthors(ctx, s.ChannelID, f.reader, []int64{s.AuthorID})
			if err != nil {
				t.Fatal(err)
			}
			if s.MessageID != m["guest"] && (len(authors) != 1 || authors[0].Name != s.AuthorName) {
				t.Errorf("message %d: name %q, the authors route says %+v", s.MessageID, s.AuthorName, authors)
			}
		}
	})

	t.Run("a removed message is never summarised", func(t *testing.T) {
		if _, err := f.s.db.Exec("INSERT INTO message_removals (message_id, channel_id, removed_at) VALUES (?, ?, 1)", m["plain"], f.ch.ID); err != nil {
			t.Fatal(err)
		}
		if _, ok := byID(f.feed(t, 0))[m["plain"]]; ok {
			t.Error("a removed message is in the feed")
		}
	})
}

// Where the page is full, or the scan bound is reached, with only hidden
// messages after: more is false and the cursor is on the last visible
// candidate, byte for byte as without the hidden ones. A hidden message never
// makes more true and never moves the cursor.
func TestPendingNotificationsHiddenAfterAFullPageOrTheBound(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		policy  schema.NotificationPolicy
		visible int
		hidden  int
	}{
		{"a full page, then hidden messages", schema.NotificationPolicyAll, schema.MaxPendingNotificationsPage, 50},
		{"the scan bound, then hidden messages", schema.NotificationPolicyMentions, PendingNotificationScanBound, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := func(withHidden bool) (*feedFixture, string) {
				f := newFeedFixture(t)
				f.policy(t, tt.policy, true)
				bulkInsert(t, f.s, f.ch.ID, f.frank.ID, tt.visible, "chatter")
				if withHidden {
					secret, err := f.s.CreateChannel(ctx, "secret")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.s.AddChannelMember(ctx, "system", secret.ID, f.erin.ID, 0); err != nil {
						t.Fatal(err)
					}
					bulkInsert(t, f.s, secret.ID, f.erin.ID, tt.hidden, "hidden @dave")
					for range 3 {
						f.whisper(t, f.erin, "whisper @dave", f.frank)
					}
				}
				raw, err := json.Marshal(f.feed(t, 0))
				if err != nil {
					t.Fatal(err)
				}
				return f, string(raw)
			}
			_, with := world(true)
			f, without := world(false)
			if with != without {
				t.Errorf("with hidden messages:    %.300s\nwithout hidden messages: %.300s", with, without)
			}
			page := f.feed(t, 0)
			var last int64
			if err := f.s.db.QueryRow("SELECT MAX(id) FROM messages").Scan(&last); err != nil {
				t.Fatal(err)
			}
			if page.More || page.Cursor != last {
				t.Errorf("more = %v, cursor = %d; want false and the last visible candidate %d", page.More, page.Cursor, last)
			}
		})
	}
}

// The per-channel candidate statement is read through messages_by_channel in
// id order, with no sort, on a database that has never been analyzed and on
// one that has (issue #1725): its cost is a seek and the rows it returns, not
// the backlog or other channels' messages.
func TestPendingNotificationsPlan(t *testing.T) {
	for _, analyze := range []bool{false, true} {
		t.Run(fmt.Sprintf("analyze=%v", analyze), func(t *testing.T) {
			f := newFeedFixture(t)
			bulkInsert(t, f.s, f.ch.ID, f.frank.ID, 300, "chatter")
			bulkInsert(t, f.s, f.other.ID, f.frank.ID, 300, "elsewhere")
			if analyze {
				if _, err := f.s.db.Exec("ANALYZE"); err != nil {
					t.Fatal(err)
				}
			}
			_, visible := visibilityArgs(Reader{PrincipalID: f.dave.ID, Scoped: true})
			args := []any{len(schema.SpoilerFilePrefix), schema.SpoilerFilePrefix, f.ch.ID, int64(0), f.dave.ID, f.dave.ID}
			args = append(append(args, visible...), 2001)
			rows, err := f.s.db.Query("EXPLAIN QUERY PLAN "+pendingNotificationsSQL(), args...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			text := strings.Join(plan, "\n")
			t.Log("\n" + text)
			if strings.Contains(text, "TEMP B-TREE") {
				t.Errorf("the plan sorts in a temp B-tree:\n%s", text)
			}
			// The channel row is one rowid lookup the planner may put first; the
			// messages themselves must be a range search of the channel index.
			if !strings.Contains(text, "SEARCH m USING INDEX messages_by_channel (channel_id=? AND id>?)") || strings.Contains(text, "SCAN m") {
				t.Errorf("the messages are not read through messages_by_channel:\n%s", text)
			}
			// The correlated subqueries run once per row the seek passes, so each
			// is an index or rowid search, never a scan (issue #1730).
			for _, tc := range []struct{ what, want string }{
				{"removals", "SEARCH mrm USING INTEGER PRIMARY KEY (rowid=?)"},
				{"recipients", "SEARCH r USING COVERING INDEX sqlite_autoindex_message_recipients_1 (message_id=? AND principal_id=?)"},
				{"bans", "SEARCH lb USING COVERING INDEX sqlite_autoindex_channel_bans_1 (channel_id=? AND principal_id=?)"},
				{"memberships", "SEARCH lm USING"},
				{"files", "SEARCH sf USING INDEX files_by_message (message_id=?)"},
				{"guest attribution", "SEARCH g USING INTEGER PRIMARY KEY (rowid=?)"},
			} {
				if !strings.Contains(text, tc.want) {
					t.Errorf("the %s subquery is not %q:\n%s", tc.what, tc.want, text)
				}
			}
			for _, line := range plan {
				if strings.HasPrefix(line, "SCAN") {
					t.Errorf("the plan scans: %q\n%s", line, text)
				}
			}
		})
	}
}
