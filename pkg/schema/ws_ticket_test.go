package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWSTicketGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"ws-ticket-v1.json", func() any { return new(WSTicketV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// TestWSTicketFixtureIsWellFormed keeps the fixture's ticket the length the
// constants say a real one is, so the constants and the example cannot drift.
func TestWSTicketFixtureIsWellFormed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ws-ticket-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ticket WSTicketV1
	if err := json.Unmarshal(raw, &ticket); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ticket.Ticket, WSTicketPrefix) || len(ticket.Ticket) != WSTicketLength {
		t.Errorf("fixture ticket %q: want prefix %q and length %d", ticket.Ticket, WSTicketPrefix, WSTicketLength)
	}
}

// TestWSTicketConstants pins the values clients and tests rely on.
func TestWSTicketConstants(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"prefix", WSTicketPrefix, "conch_wst_"},
		{"length", WSTicketLength, 53},
		{"lifetime", WSTicketLifetimeSeconds, 30},
		{"per credential", MaxWSTicketsPerCredential, 8},
		{"tickets_require_auth", ErrorCodeTicketsRequireAuth, "tickets_require_auth"},
		{"too_many_tickets", ErrorCodeTooManyTickets, "too_many_tickets"},
		{"too_many_sockets", ErrorCodeTooManySockets, "too_many_sockets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}
