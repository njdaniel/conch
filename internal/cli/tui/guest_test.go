package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Guests in the TUI (issue #715, ADR-009 §4.1 and §4.4): who you are and
// when your visit ends, and authors shown by the attribution the server
// keeps, never by a name's text.

const testHandle = schema.GuestNamePrefix + "a7k4m2q9a7k4m2q9"

// guestNow is the clock of these tests, and the visit ends at 18:00.
var guestNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func testVisit() *schema.GuestV1 {
	return &schema.GuestV1{
		State: schema.GuestStateActive, Origin: schema.GuestOriginInvite, Handle: testHandle, Label: "Pat", Tag: schema.GuestTag(testHandle),
		StartedAt: schema.NewTimestamp(guestNow), ExpiresAt: schema.NewTimestamp(guestNow.Add(6 * time.Hour)), IdleExpiresAt: schema.NewTimestamp(guestNow.Add(2 * time.Hour)),
		CanConvert: true,
	}
}

// guestModel is a model told by whoami that it is the guest of testVisit.
func guestModel(t *testing.T) Model {
	t.Helper()
	api := &identityAPI{who: schema.WhoAmIResponseV1{ID: 9, Kind: schema.PrincipalHuman, Name: testHandle, Role: schema.RoleMember,
		IdentityClass: schema.IdentityGuest, Guest: testVisit()}}
	m := NewModel(context.Background(), api, 0, []string{"general"}).WithCredential()
	m.loc = time.UTC
	m.now = func() time.Time { return guestNow }
	updated, _ := m.Update(m.loadWhoAmI()())
	return updated.(Model)
}

func TestGuestIdentity(t *testing.T) {
	m := guestModel(t)
	if m.userName != "Guest: Pat · a7k4" || m.visit == nil {
		t.Fatalf("name = %q, visit %v; want the guest's display name and its visit", m.userName, m.visit)
	}
	if got, want := m.identity(), "Guest: Pat · a7k4 · until 18:00"; got != want {
		t.Errorf("identity = %q, want %q", got, want)
	}
	if want := "signed in as Guest: Pat · a7k4, a temporary guest until 18:00"; !strings.HasPrefix(m.status(), want) {
		t.Errorf("status = %q, want it to start %q", m.status(), want)
	}
	notes := strings.Join(m.helpNotes(), "\n")
	if !strings.Contains(notes, "temporary guest until 18:00") || !strings.Contains(notes, "conch end-visit") || !strings.Contains(notes, "conch account create") {
		t.Errorf("help notes = %q, want the visit and the ways out of it", notes)
	}

	// Past its deadline the visit is not shown as a working sign-in.
	m.now = func() time.Time { return guestNow.Add(6 * time.Hour) }
	if got := m.identity(); got != "guest visit ended" {
		t.Errorf("identity after the deadline = %q", got)
	}

	// A member is shown as before, with no visit.
	api := &identityAPI{who: schema.WhoAmIResponseV1{ID: 42, Kind: schema.PrincipalHuman, Name: "nick", Role: schema.RoleMember, IdentityClass: schema.IdentityMember}}
	member := NewModel(context.Background(), api, 0, []string{"general"}).WithCredential()
	updated, _ := member.Update(member.loadWhoAmI()())
	member = updated.(Model)
	if member.visit != nil || member.identity() != "nick" {
		t.Errorf("member identity = %q, visit %v", member.identity(), member.visit)
	}
	if strings.Contains(strings.Join(member.helpNotes(), "\n"), "Guest visit:") {
		t.Error("a member's help talks about a guest visit")
	}
}

func TestGuestAttribution(t *testing.T) {
	attribution := &schema.GuestAttributionV1{Handle: testHandle, Label: "Pat", Tag: schema.GuestTag(testHandle)}
	m := NewModel(context.Background(), &identityAPI{}, 0, []string{"general"})
	m.members = map[string]map[int64]schema.ChannelMemberV1{"general": {
		9:  {PrincipalID: 9, Name: "alice", Kind: schema.PrincipalHuman},
		10: {PrincipalID: 10, Name: testHandle, Kind: schema.PrincipalHuman, Guest: attribution},
	}}
	tests := []struct {
		name    string
		message schema.MessageV2
		want    string
	}{
		// Posted during the visit, by a guest who has since made the
		// account alice: still the guest's.
		{"a post from a visit, after conversion", schema.MessageV2{AuthorID: 9, GuestAuthor: attribution}, "Guest: Pat · a7k4"},
		{"a post from an ended visit", schema.MessageV2{AuthorID: 99, GuestAuthor: attribution}, "Guest: Pat · a7k4"},
		{"a guest without a label", schema.MessageV2{AuthorID: 99, GuestAuthor: &schema.GuestAttributionV1{Handle: testHandle, Tag: schema.GuestTag(testHandle)}}, testHandle},
		{"a malformed attribution is not trusted", schema.MessageV2{AuthorID: 99, GuestAuthor: &schema.GuestAttributionV1{Handle: "guest-x", Tag: "x"}}, "#99"},
		{"a member's post", schema.MessageV2{AuthorID: 9}, "alice"},
		{"a live guest member", schema.MessageV2{AuthorID: 10}, "Guest: Pat · a7k4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := m.messageAuthor("general", tt.message); got != tt.want {
				t.Errorf("author = %q, want %q", got, tt.want)
			}
		})
	}
	// Where no message is at hand (voice, runs), the member list's guest
	// flag is what shows a guest.
	if got, _ := m.authorName("general", 10); got != "Guest: Pat · a7k4" {
		t.Errorf("live guest member = %q", got)
	}
}
