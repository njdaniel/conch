package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A guest visit's credential lives only in the temporary guest file, with its
// deadline, and never in credentials.json (issue #715, design §3.4).

const guestTestServer = "http://127.0.0.1:9"

// moveGuestClock sets the clock stored visits are held to.
func moveGuestClock(t *testing.T, now time.Time) {
	t.Helper()
	old := guestNow
	guestNow = func() time.Time { return now }
	t.Cleanup(func() { guestNow = old })
}

func TestGuestVisitStorage(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	visit := storedGuestVisit{Token: "guest-token", Handle: "guest-a7k4m2q9a7k4m2q9", ExpiresAt: start.Add(24 * time.Hour)}
	tests := []struct {
		name string
		// before stores what is there already: a member login, a visit or none.
		member, guest bool
		env           string
		// at is when the visit is read.
		at          time.Duration
		wantToken   string
		wantHanded  bool
		wantGuestOK bool
	}{
		{name: "a live visit is used and handed on", guest: true, at: time.Hour, wantToken: "guest-token", wantHanded: true, wantGuestOK: true},
		{name: "past its deadline it is gone", guest: true, at: 24 * time.Hour, wantToken: ""},
		{name: "CONCH_TOKEN first", guest: true, env: "env-token", at: time.Hour, wantToken: "env-token", wantHanded: true, wantGuestOK: true},
		{name: "a member login before a visit", member: true, guest: true, at: time.Hour, wantToken: "member-token", wantGuestOK: true},
		{name: "nothing", at: time.Hour, wantToken: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			moveGuestClock(t, start)
			// Both kinds are written straight to their files: the commands
			// never let them stand together, and the reader must still
			// prefer the lasting login.
			if tt.guest {
				if err := writeGuestVisits(map[string]storedGuestVisit{guestTestServer: visit}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.member {
				if err := saveMemberToken(guestTestServer, "member-token"); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CONCH_TOKEN", tt.env)
			moveGuestClock(t, start.Add(tt.at))
			token, handed, err := LoadCredential(guestTestServer)
			if err != nil || token != tt.wantToken || handed != tt.wantHanded {
				t.Fatalf("LoadCredential = %q, %v, %v; want %q, %v", token, handed, err, tt.wantToken, tt.wantHanded)
			}
			_, statErr := os.Stat(guestFile())
			if tt.guest && tt.wantGuestOK == os.IsNotExist(statErr) {
				t.Errorf("guest file: %v, want it there %v", statErr, tt.wantGuestOK)
			}
			if !tt.member {
				if _, err := os.Stat(memberFile()); !os.IsNotExist(err) {
					t.Errorf("credentials.json exists (%v)", err)
				}
			}
		})
	}
}

// A visit and a lasting login never share a server: each refuses the other,
// and a conversion replaces the visit with the login.
func TestGuestVisitAndMemberLoginExclude(t *testing.T) {
	isolateConfig(t)
	moveGuestClock(t, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	visit := storedGuestVisit{Token: "guest-token", Handle: "guest-a7k4m2q9a7k4m2q9", ExpiresAt: guestNow().Add(time.Hour)}
	if err := saveGuestVisit(guestTestServer, visit); err != nil {
		t.Fatal(err)
	}
	if err := saveGuestVisit(guestTestServer, visit); err == nil || !strings.Contains(err.Error(), "already stored") {
		t.Errorf("a second visit = %v", err)
	}
	if err := SaveToken(guestTestServer, "member-token"); err == nil || !strings.Contains(err.Error(), "conch end-visit") {
		t.Errorf("a member login over a visit = %v", err)
	}
	// Another server is another matter, and both are offered as defaults.
	if err := SaveToken("http://127.0.0.1:10", "member-token"); err != nil {
		t.Fatal(err)
	}
	if got := storedServers(); strings.Join(got, " ") != "http://127.0.0.1:10 "+guestTestServer {
		t.Errorf("storedServers = %v", got)
	}
	if err := replaceGuestWithMember(guestTestServer, "converted-token"); err != nil {
		t.Fatal(err)
	}
	if token, _ := LoadToken(guestTestServer); token != "converted-token" {
		t.Errorf("after conversion the token is %q", token)
	}
	if _, err := os.Stat(guestFile()); !os.IsNotExist(err) {
		t.Errorf("the guest file remains (%v)", err)
	}
	if err := saveGuestVisit(guestTestServer, visit); err == nil || !strings.Contains(err.Error(), "lasting login") {
		t.Errorf("a visit over a member login = %v", err)
	}
}

// Nothing of the guest file is in credentials.json, whatever is stored.
func TestGuestTokenNeverInCredentialsFile(t *testing.T) {
	isolateConfig(t)
	moveGuestClock(t, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	if err := SaveToken("http://127.0.0.1:10", "member-token"); err != nil {
		t.Fatal(err)
	}
	if err := saveGuestVisit(guestTestServer, storedGuestVisit{Token: "the-guest-secret", Handle: "guest-x", ExpiresAt: guestNow().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(memberFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "the-guest-secret") || strings.Contains(string(data), guestTestServer) {
		t.Errorf("credentials.json = %s", data)
	}
	data, err = os.ReadFile(filepath.Join(filepath.Dir(memberFile()), guestVisitsFileName))
	if err != nil || !strings.Contains(string(data), "the-guest-secret") {
		t.Errorf("guest file = %s, %v", data, err)
	}
}
