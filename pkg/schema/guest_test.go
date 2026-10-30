package schema

import (
	"strings"
	"testing"
	"time"
)

func TestGuestGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"guest-attribution-v1.json", func() any { return new(GuestAttributionV1) }},
		{"guest-attribution-v1-no-label.json", func() any { return new(GuestAttributionV1) }},
		{"guest-v1.json", func() any { return new(GuestV1) }},
		{"guest-v1-ended.json", func() any { return new(GuestV1) }},
		{"guest-v1-converted.json", func() any { return new(GuestV1) }},
		{"guest-admin-v1.json", func() any { return new(GuestAdminV1) }},
		{"guest-admin-v1-invite.json", func() any { return new(GuestAdminV1) }},
		{"guest-admin-v1-ended.json", func() any { return new(GuestAdminV1) }},
		{"guest-admin-v1-converted.json", func() any { return new(GuestAdminV1) }},
		{"list-guests-response-v1.json", func() any { return new(ListGuestsResponseV1) }},
		{"list-guests-response-v1-empty.json", func() any { return new(ListGuestsResponseV1) }},
		{"end-guest-response-v1.json", func() any { return new(EndGuestResponseV1) }},
		{"end-visit-response-v1.json", func() any { return new(EndVisitResponseV1) }},
		{"end-guests-request-v1.json", func() any { return new(EndGuestsRequestV1) }},
		{"end-guests-request-v1-dry-run.json", func() any { return new(EndGuestsRequestV1) }},
		{"end-guests-request-v1-invite.json", func() any { return new(EndGuestsRequestV1) }},
		{"end-guests-response-v1.json", func() any { return new(EndGuestsResponseV1) }},
		{"end-guests-response-v1-dry-run.json", func() any { return new(EndGuestsResponseV1) }},
		{"join-info-v1.json", func() any { return new(JoinInfoV1) }},
		{"join-info-v1-closed.json", func() any { return new(JoinInfoV1) }},
		{"join-info-v1-full.json", func() any { return new(JoinInfoV1) }},
		{"join-info-v1-paused.json", func() any { return new(JoinInfoV1) }},
		{"join-info-v1-auth-off.json", func() any { return new(JoinInfoV1) }},
		{"join-settings-v1.json", func() any { return new(JoinSettingsV1) }},
		{"join-settings-v1-open.json", func() any { return new(JoinSettingsV1) }},
		{"put-join-settings-request-v1.json", func() any { return new(PutJoinSettingsRequestV1) }},
		{"put-join-settings-request-v1-pause.json", func() any { return new(PutJoinSettingsRequestV1) }},
		{"join-settings-response-v1.json", func() any { return new(JoinSettingsResponseV1) }},
		{"join-settings-response-v1-warnings.json", func() any { return new(JoinSettingsResponseV1) }},
		{"join-guest-request-v1.json", func() any { return new(JoinGuestRequestV1) }},
		{"join-guest-request-v1-label.json", func() any { return new(JoinGuestRequestV1) }},
		{"join-guest-response-v1.json", func() any { return new(JoinGuestResponseV1) }},
		{"join-account-request-v1.json", func() any { return new(JoinAccountRequestV1) }},
		{"join-account-response-v1.json", func() any { return new(JoinAccountResponseV1) }},
		{"convert-guest-request-v1.json", func() any { return new(ConvertGuestRequestV1) }},
		{"convert-guest-request-v1-permit.json", func() any { return new(ConvertGuestRequestV1) }},
		{"convert-guest-response-v1.json", func() any { return new(ConvertGuestResponseV1) }},
		{"create-conversion-permit-request-v1.json", func() any { return new(CreateConversionPermitRequestV1) }},
		{"create-conversion-permit-request-v1-default.json", func() any { return new(CreateConversionPermitRequestV1) }},
		{"redeem-invite-request-v2.json", func() any { return new(RedeemInviteRequestV2) }},
		{"redeem-invite-guest-request-v1.json", func() any { return new(RedeemInviteGuestRequestV1) }},
		{"redeem-invite-guest-request-v1-label.json", func() any { return new(RedeemInviteGuestRequestV1) }},
		{"error-guests-full.json", func() any { return new(Error) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

const (
	goodHandle = "guest-a7k4m2q9x3h5t8wz"
	goodPass   = "correct horse battery staple"
)

func TestValidateGuestHandle(t *testing.T) {
	tests := []struct {
		name, handle string
		ok           bool
	}{
		{"well-formed", goodHandle, true},
		{"digits and letters", "guest-0123456789abcdef", true},
		{"the last of the alphabet", "guest-ghjkmnpqrstvwxyz", true},
		{"empty", "", false},
		{"short", "guest-a7k4", false},
		{"long", goodHandle + "a", false},
		{"upper case", "guest-A7K4M2Q9X3H5T8WZ", false},
		{"an i", "guest-i7k4m2q9x3h5t8wz", false},
		{"an l", "guest-l7k4m2q9x3h5t8wz", false},
		{"an o", "guest-o7k4m2q9x3h5t8wz", false},
		{"a u", "guest-u7k4m2q9x3h5t8wz", false},
		{"no prefix", "a7k4m2q9x3h5t8wz", false},
		{"another prefix", "ghost-a7k4m2q9x3h5t8wz", false},
		{"a permanent name", "river", false},
		{"non-ASCII", "guest-a7k4m2q9x3h5t8wé", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGuestHandle(tt.handle)
			if (err == nil) != tt.ok {
				t.Fatalf("ValidateGuestHandle = %v, want ok %v", err, tt.ok)
			}
			if err != nil && tt.handle != "" && strings.Contains(err.Error(), tt.handle) {
				t.Errorf("the error echoes the handle: %v", err)
			}
		})
	}
	if GuestHandleLength != len(goodHandle) || GuestHandleSuffixLength*5 != GuestHandleRandomBytes*8 {
		t.Errorf("handle length %d, suffix %d characters for %d bytes", GuestHandleLength, GuestHandleSuffixLength, GuestHandleRandomBytes)
	}
	if len(GuestHandleAlphabet) != 32 || strings.ContainsAny(GuestHandleAlphabet, "ilou") {
		t.Errorf("alphabet %q is not Crockford base32 in lower case", GuestHandleAlphabet)
	}
}

func TestGuestTagAndDisplayName(t *testing.T) {
	if got := GuestTag(goodHandle); got != "a7k4" {
		t.Errorf("GuestTag = %q, want a7k4", got)
	}
	if got := GuestTag("river"); got != "" {
		t.Errorf("GuestTag of a name = %q, want empty", got)
	}
	if got, want := GuestDisplayName("Pat", goodHandle), "Guest: Pat · a7k4"; got != want {
		t.Errorf("GuestDisplayName = %q, want %q", got, want)
	}
	if got := GuestDisplayName("", goodHandle); got != goodHandle {
		t.Errorf("GuestDisplayName without a label = %q, want the handle", got)
	}
}

// The reserved namespace is the folded prefix "guest" on a permanent name.
// A look-alike in another script passes here and is the server's to refuse
// through the name's confusable skeleton; the schema check is the cheap
// one, never the only one.
func TestIsReservedGuestName(t *testing.T) {
	tests := []struct {
		name     string
		reserved bool
	}{
		{"guest", true},
		{"Guest", true},
		{"GUEST", true},
		{"guest-a7k4m2q9x3h5t8wz", true},
		{"Guest-Pat", true},
		{"guests", true},
		{"guestbook", true},
		{"gues", false},
		{"", false},
		{"a guest", false},
		{"river", false},
		{"hidden-12", false},
		{"ɡuest", false}, // U+0261: the skeleton check, not this one
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReservedGuestName(tt.name); got != tt.reserved {
				t.Errorf("IsReservedGuestName(%q) = %v, want %v", tt.name, got, tt.reserved)
			}
		})
	}
}

func TestValidateGuestLabel(t *testing.T) {
	tests := []struct {
		name, label string
		wantErr     string
	}{
		{"a name", "Pat", ""},
		{"two words", "Pat Smith", ""},
		{"another script", "山田 太郎", ""},
		{"the most", strings.Repeat("a", MaxGuestLabelLength), ""},
		{"one past the most", strings.Repeat("a", MaxGuestLabelLength+1), "1 to 32 characters"},
		{"empty", "", "1 to 32 characters"},
		{"blank", "   ", "whitespace"},
		{"leading space", " Pat", "whitespace"},
		{"the separator", "Pat · zx9w", "separator"},
		{"a run of spaces", "Pat  zx9w", "run of whitespace"},
		{"a tab", "Pat\tzx9w", "control"},
		{"a newline", "Pat\nzx9w", "control"},
		{"a bidi override", "Pat\u202ezx9w", "bidi"},
		{"a zero-width space", "Pat\u200bSmith", "invisible"},
		{"the reserved prefix", "Guest", "reserved"},
		{"the reserved prefix in capitals", "GUEST-zx9w", "reserved"},
		{"the prefix inside", "a guest", ""},
		{"invalid UTF-8", "Pa\xfft", "UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGuestLabel(tt.label)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("ValidateGuestLabel = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("ValidateGuestLabel = %v, want an error containing %q", err, tt.wantErr)
			}
			if err != nil && strings.TrimSpace(tt.label) != "" && strings.Contains(err.Error(), tt.label) {
				t.Errorf("the error echoes the label: %v", err)
			}
		})
	}
}

func TestGuestV1Validate(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) Timestamp { return NewTimestamp(now.Add(d)) }
	ptr := func(ts Timestamp) *Timestamp { return &ts }
	live := GuestV1{State: GuestStateActive, Origin: GuestOriginAddress, Handle: goodHandle, Label: "Pat", Tag: "a7k4",
		StartedAt: at(0), ExpiresAt: at(24 * time.Hour), IdleExpiresAt: at(2 * time.Hour), CanConvert: true}
	with := func(f func(g *GuestV1)) GuestV1 {
		g := live
		f(&g)
		return g
	}
	tests := []struct {
		name    string
		guest   GuestV1
		wantErr string
	}{
		{"live", live, ""},
		{"no label", with(func(g *GuestV1) { g.Label = "" }), ""},
		{"ended", with(func(g *GuestV1) { g.State, g.EndedAt, g.EndReason = GuestStateEnded, ptr(at(time.Hour)), GuestEndIdle }), ""},
		{"converted", with(func(g *GuestV1) { g.State = GuestStateConverted }), ""},
		{"unknown state", with(func(g *GuestV1) { g.State = "paused" }), "state"},
		{"unknown origin", with(func(g *GuestV1) { g.Origin = "import" }), "origin"},
		{"a name for a handle", with(func(g *GuestV1) { g.Handle = "river" }), "handle"},
		{"wrong tag", with(func(g *GuestV1) { g.Tag = "zx9w" }), "tag"},
		{"bad label", with(func(g *GuestV1) { g.Label = "Guest" }), "reserved"},
		{"no times", with(func(g *GuestV1) { g.StartedAt = Timestamp{} }), "required"},
		{"idle after the deadline", with(func(g *GuestV1) { g.IdleExpiresAt = at(25 * time.Hour) }), "idle_expires_at"},
		{"ended without a reason", with(func(g *GuestV1) { g.State, g.EndedAt = GuestStateEnded, ptr(at(time.Hour)) }), "end_reason"},
		{"ended without a time", with(func(g *GuestV1) { g.State, g.EndReason = GuestStateEnded, GuestEndVisit }), "ended_at"},
		{"ended with an unknown reason", with(func(g *GuestV1) { g.State, g.EndedAt, g.EndReason = GuestStateEnded, ptr(at(time.Hour)), "bored" }), "end_reason"},
		{"live with an end", with(func(g *GuestV1) { g.EndedAt = ptr(at(time.Hour)) }), "no ended_at"},
		{"converted with a reason", with(func(g *GuestV1) { g.State, g.EndReason = GuestStateConverted, GuestEndVisit }), "no ended_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.guest.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestJoinSettingsV1Validate(t *testing.T) {
	open := JoinSettingsV1{OpenJoining: OpenJoiningBoth, GuestsEnabled: true, MaxGuests: 50, MaxGuestsPerAddress: 5,
		GuestIdleSeconds: 7200, GuestAbsoluteSeconds: 86400, OpenChannels: []string{"lobby"}}
	with := func(f func(s *JoinSettingsV1)) JoinSettingsV1 {
		s := open
		s.OpenChannels = append([]string(nil), open.OpenChannels...)
		f(&s)
		return s
	}
	tests := []struct {
		name     string
		settings JoinSettingsV1
		wantErr  string
	}{
		{"defaults", DefaultJoinSettings(), ""},
		{"open", open, ""},
		{"guest only", with(func(s *JoinSettingsV1) { s.OpenJoining = OpenJoiningGuest }), ""},
		{"account only, guests off", with(func(s *JoinSettingsV1) { s.OpenJoining, s.GuestsEnabled = OpenJoiningAccount, false }), ""},
		{"both, guests off", with(func(s *JoinSettingsV1) { s.GuestsEnabled = false }), ""},
		{"no guests at all", with(func(s *JoinSettingsV1) { s.MaxGuests = 0 }), ""},
		{"idle equal to absolute", with(func(s *JoinSettingsV1) { s.GuestIdleSeconds = 86400 }), ""},
		{"unknown mode", with(func(s *JoinSettingsV1) { s.OpenJoining = "anyone" }), "open_joining"},
		{"guest only, guests off", with(func(s *JoinSettingsV1) { s.OpenJoining, s.GuestsEnabled = OpenJoiningGuest, false }), "guests_enabled"},
		{"open with no channel", with(func(s *JoinSettingsV1) { s.OpenChannels = []string{} }), "open channel"},
		{"off with no channel", with(func(s *JoinSettingsV1) { s.OpenJoining, s.OpenChannels = OpenJoiningOff, nil }), ""},
		{"negative cap", with(func(s *JoinSettingsV1) { s.MaxGuests = -1 }), "max_guests"},
		{"cap too high", with(func(s *JoinSettingsV1) { s.MaxGuests = MaxMaxGuests + 1 }), "max_guests"},
		{"per-address cap of nothing", with(func(s *JoinSettingsV1) { s.MaxGuestsPerAddress = 0 }), "max_guests_per_address"},
		{"idle under a minute", with(func(s *JoinSettingsV1) { s.GuestIdleSeconds = 59 }), "guest_idle_seconds"},
		{"idle past absolute", with(func(s *JoinSettingsV1) { s.GuestIdleSeconds = 86401 }), "guest_idle_seconds"},
		{"absolute past a day", with(func(s *JoinSettingsV1) { s.GuestAbsoluteSeconds = 86401 }), "guest_absolute_seconds"},
		{"too many channels", with(func(s *JoinSettingsV1) { s.OpenChannels = manyNames(MaxOpenChannels + 1) }), "at most"},
		{"a channel twice", with(func(s *JoinSettingsV1) { s.OpenChannels = []string{"lobby", "lobby"} }), "twice"},
		{"a blank channel", with(func(s *JoinSettingsV1) { s.OpenChannels = []string{" "} }), "blank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.settings.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func manyNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "channel-" + strings.Repeat("a", i+1)
	}
	return out
}

func TestPutJoinSettingsRequestV1(t *testing.T) {
	mode := func(m OpenJoiningMode) *OpenJoiningMode { return &m }
	b := func(v bool) *bool { return &v }
	i := func(v int64) *int64 { return &v }
	t.Run("validate", func(t *testing.T) {
		tests := []struct {
			name    string
			req     PutJoinSettingsRequestV1
			wantErr string
		}{
			{"one field", PutJoinSettingsRequestV1{GuestsEnabled: b(true)}, ""},
			{"channels", PutJoinSettingsRequestV1{OpenChannels: &[]string{"lobby"}}, ""},
			{"clear channels", PutJoinSettingsRequestV1{OpenChannels: &[]string{}}, ""},
			{"nothing", PutJoinSettingsRequestV1{}, "changes no setting"},
			{"unknown mode", PutJoinSettingsRequestV1{OpenJoining: mode("anyone")}, "open_joining"},
			{"a channel twice", PutJoinSettingsRequestV1{OpenChannels: &[]string{"a", "a"}}, "twice"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := tt.req.Validate()
				switch {
				case tt.wantErr == "" && err != nil:
					t.Errorf("Validate = %v, want nil", err)
				case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
					t.Errorf("Validate = %v, want an error containing %q", err, tt.wantErr)
				}
			})
		}
	})
	t.Run("apply merges", func(t *testing.T) {
		base := DefaultJoinSettings()
		got := PutJoinSettingsRequestV1{OpenJoining: mode(OpenJoiningBoth), GuestsEnabled: b(true), MaxGuests: i(10), OpenChannels: &[]string{"lobby"}}.Apply(base)
		if got.OpenJoining != OpenJoiningBoth || !got.GuestsEnabled || got.MaxGuests != 10 || len(got.OpenChannels) != 1 || got.OpenChannels[0] != "lobby" {
			t.Errorf("Apply = %+v", got)
		}
		if got.GuestIdleSeconds != DefaultGuestIdleSeconds || got.MaxGuestsPerAddress != DefaultMaxGuestsPerAddress {
			t.Errorf("Apply changed a field the request left out: %+v", got)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("the applied settings do not validate: %v", err)
		}
		if len(base.OpenChannels) != 0 {
			t.Errorf("Apply modified the receiver's channels: %v", base.OpenChannels)
		}
	})
	t.Run("turning guests off turns a guest-only address off", func(t *testing.T) {
		guestOnly := JoinSettingsV1{OpenJoining: OpenJoiningGuest, GuestsEnabled: true, MaxGuests: 50, MaxGuestsPerAddress: 5,
			GuestIdleSeconds: 7200, GuestAbsoluteSeconds: 86400, OpenChannels: []string{"lobby"}}
		got := PutJoinSettingsRequestV1{GuestsEnabled: b(false)}.Apply(guestOnly)
		if got.OpenJoining != OpenJoiningOff {
			t.Errorf("open_joining = %s, want off", got.OpenJoining)
		}
		// With both, account signup stays available.
		both := guestOnly
		both.OpenJoining = OpenJoiningBoth
		if got := (PutJoinSettingsRequestV1{GuestsEnabled: b(false)}).Apply(both); got.OpenJoining != OpenJoiningBoth {
			t.Errorf("open_joining = %s, want both", got.OpenJoining)
		}
		// An explicit contradictory mode is kept for Validate to refuse,
		// not silently corrected.
		got = PutJoinSettingsRequestV1{GuestsEnabled: b(false), OpenJoining: mode(OpenJoiningGuest)}.Apply(guestOnly)
		if got.OpenJoining != OpenJoiningGuest || got.Validate() == nil {
			t.Errorf("an explicit guest-only mode with guests off should be kept and refused: %+v", got)
		}
	})
	t.Run("nil channels become an empty list", func(t *testing.T) {
		got := PutJoinSettingsRequestV1{JoinVoice: b(true)}.Apply(JoinSettingsV1{OpenJoining: OpenJoiningOff})
		if got.OpenChannels == nil {
			t.Error("OpenChannels is nil")
		}
	})
}

func TestCreateConversionPermitRequestV1(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *Timestamp {
		ts := NewTimestamp(now.Add(d))
		return &ts
	}
	tests := []struct {
		name    string
		req     CreateConversionPermitRequestV1
		wantErr string
		expiry  time.Time
	}{
		{"default", CreateConversionPermitRequestV1{}, "", now.Add(10 * time.Minute)},
		{"an hour", CreateConversionPermitRequestV1{ExpiresAt: at(time.Hour)}, "", now.Add(time.Hour)},
		{"past an hour", CreateConversionPermitRequestV1{ExpiresAt: at(time.Hour + time.Second)}, "within 1h", time.Time{}},
		{"past", CreateConversionPermitRequestV1{ExpiresAt: at(-time.Second)}, "future", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate(now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Validate = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if got := tt.req.EffectiveExpiry(now); !got.Equal(tt.expiry) {
				t.Errorf("EffectiveExpiry = %v, want %v", got, tt.expiry)
			}
		})
	}
}

func TestEndGuestsRequestV1Validate(t *testing.T) {
	dry, real := true, false
	tests := []struct {
		name    string
		req     EndGuestsRequestV1
		wantErr string
	}{
		{"everyone", EndGuestsRequestV1{DryRun: &real}, ""},
		{"dry run of everyone", EndGuestsRequestV1{DryRun: &dry}, ""},
		{"an origin", EndGuestsRequestV1{DryRun: &real, Origin: GuestOriginAddress}, ""},
		{"an invite", EndGuestsRequestV1{DryRun: &real, InviteID: 7}, ""},
		{"an invite and its origin", EndGuestsRequestV1{DryRun: &real, InviteID: 7, Origin: GuestOriginInvite}, ""},
		{"dry_run left out", EndGuestsRequestV1{Origin: GuestOriginAddress}, "dry_run is required"},
		{"unknown origin", EndGuestsRequestV1{DryRun: &dry, Origin: "import"}, "origin"},
		{"negative invite", EndGuestsRequestV1{DryRun: &dry, InviteID: -1}, "positive"},
		{"an invite and the address", EndGuestsRequestV1{DryRun: &dry, InviteID: 7, Origin: GuestOriginAddress}, "cannot be address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// Every request that names a permanent account refuses the reserved guest
// namespace and a password under the policy; every one that takes a label
// holds it to the label rules; and a code of another kind is never a permit.
func TestJoinRequestsValidate(t *testing.T) {
	inv := InviteCodePrefix + strings.Repeat("A", 43)
	sil := SignInLinkCodePrefix + strings.Repeat("A", 43)
	tests := []struct {
		name    string
		req     interface{ Validate() error }
		wantErr string
	}{
		{"guest, no label", JoinGuestRequestV1{}, ""},
		{"guest, a label", JoinGuestRequestV1{Label: "Pat"}, ""},
		{"guest, a reserved label", JoinGuestRequestV1{Label: "guest"}, "reserved"},
		{"account", JoinAccountRequestV1{Name: "pat", Password: goodPass}, ""},
		{"account, reserved name", JoinAccountRequestV1{Name: "Guest-Pat", Password: goodPass}, "reserved"},
		{"account, hidden-reserved name is the store's", JoinAccountRequestV1{Name: "hidden-12", Password: goodPass}, ""},
		{"account, empty name", JoinAccountRequestV1{Name: "", Password: goodPass}, "empty"},
		{"account, short password", JoinAccountRequestV1{Name: "pat", Password: "frog"}, ""},
		{"account, no password", JoinAccountRequestV1{Name: "pat"}, "password is required"},
		{"convert", ConvertGuestRequestV1{Name: "pat", Password: goodPass}, ""},
		{"convert with a permit", ConvertGuestRequestV1{Name: "pat", Password: goodPass, Permit: sil}, ""},
		{"convert with an invite code for a permit", ConvertGuestRequestV1{Name: "pat", Password: goodPass, Permit: inv}, "permit"},
		{"convert to a reserved name", ConvertGuestRequestV1{Name: "guest-pat", Password: goodPass}, "reserved"},
		{"convert, short password", ConvertGuestRequestV1{Name: "pat", Password: "frog"}, ""},
		{"convert, no password", ConvertGuestRequestV1{Name: "pat"}, "password is required"},
		{"redeem v2", RedeemInviteRequestV2{Code: inv, Name: "river", Password: goodPass}, ""},
		{"redeem v2, short password", RedeemInviteRequestV2{Code: inv, Name: "river", Password: "frog"}, ""},
		{"redeem v2, no password", RedeemInviteRequestV2{Code: inv, Name: "river"}, "password is required"},
		{"redeem v2, reserved name", RedeemInviteRequestV2{Code: inv, Name: "GUEST", Password: goodPass}, "reserved"},
		{"redeem v2, bad code", RedeemInviteRequestV2{Code: sil, Name: "river", Password: goodPass}, "invite code"},
		{"redeem as guest", RedeemInviteGuestRequestV1{Code: inv}, ""},
		{"redeem as guest with a label", RedeemInviteGuestRequestV1{Code: inv, Label: "Pat"}, ""},
		{"redeem as guest, bad code", RedeemInviteGuestRequestV1{Code: sil}, "invite code"},
		{"redeem as guest, bad label", RedeemInviteGuestRequestV1{Code: inv, Label: "a  b"}, "whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate = %v, want an error containing %q", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), goodPass) {
				t.Errorf("the error echoes the password: %v", err)
			}
		})
	}
}

// No guest or joining shape carries a password, a hash, a secret, a code,
// a token or a challenge, except the ones documented to: the responses
// that issue a credential's token exactly once, and the request bodies
// that must carry a password or a code. Each exception is named here.
// Every json key is walked, omitempty ones included.
func TestGuestShapesCarryNoSecret(t *testing.T) {
	for _, v := range []any{
		GuestAttributionV1{}, GuestV1{}, GuestAdminV1{}, GuestRefV1{}, ListGuestsResponseV1{},
		EndGuestResponseV1{}, EndVisitResponseV1{}, EndGuestsRequestV1{}, EndGuestsResponseV1{},
		JoinOptionsV1{}, JoinInfoV1{}, JoinSettingsV1{}, PutJoinSettingsRequestV1{}, JoinSettingsResponseV1{},
		JoinGuestRequestV1{}, CreateConversionPermitRequestV1{},
	} {
		assertNoSecretKeys(t, v)
	}
	// Responses that issue the new credential's token, shown exactly once.
	assertNoSecretKeys(t, JoinGuestResponseV1{}, "token")
	assertNoSecretKeys(t, JoinAccountResponseV1{}, "token")
	assertNoSecretKeys(t, ConvertGuestResponseV1{}, "token")
	assertNoSecretKeys(t, RedeemInviteResponseV1{}, "token")
	// Request bodies that must carry a password or an invite code; a
	// permit is a link code under its own key.
	assertNoSecretKeys(t, JoinAccountRequestV1{}, "password")
	assertNoSecretKeys(t, ConvertGuestRequestV1{}, "password")
	assertNoSecretKeys(t, RedeemInviteRequestV2{}, "code", "password")
	assertNoSecretKeys(t, RedeemInviteGuestRequestV1{}, "code")
	// The visit's wire fields are exactly the documented ones.
	if got, want := jsonKeys(t, GuestV1{}), "can_convert,expires_at,handle,idle_expires_at,origin,started_at,state,tag"; got != want {
		t.Errorf("GuestV1 keys = %s, want %s", got, want)
	}
	if got, want := jsonKeys(t, JoinInfoV1{}), "guest_absolute_seconds,guest_idle_seconds,join,password_login,server_name,voice"; got != want {
		t.Errorf("JoinInfoV1 keys = %s, want %s", got, want)
	}
}

func TestGuestConstants(t *testing.T) {
	if DefaultGuestIdleSeconds != 7200 || DefaultGuestAbsoluteSeconds != 86400 || MaxGuestLifetimeSeconds != 86400 {
		t.Errorf("guest bounds = %d idle, %d absolute, %d max", DefaultGuestIdleSeconds, DefaultGuestAbsoluteSeconds, MaxGuestLifetimeSeconds)
	}
	if DefaultMaxGuests != 50 || DefaultMaxGuestsPerAddress != 5 {
		t.Errorf("guest caps = %d, %d per address", DefaultMaxGuests, DefaultMaxGuestsPerAddress)
	}
	if !strings.HasPrefix(GuestNamePrefix, ReservedGuestNameKeyPrefix) {
		t.Errorf("the handle prefix %q is not in the reserved namespace %q", GuestNamePrefix, ReservedGuestNameKeyPrefix)
	}
	if err := DefaultJoinSettings().Validate(); err != nil {
		t.Errorf("the default settings do not validate: %v", err)
	}
	for _, c := range []string{ErrorCodeGuestsDisabled, ErrorCodeJoiningClosed, ErrorCodeJoiningPaused, ErrorCodeGuestsFull, ErrorCodeGuestNotAllowed,
		ErrorCodeGuestNotHideable, ErrorCodeGuestEnded, ErrorCodeNotAGuest, ErrorCodeConversionNotAllowed, ErrorCodeConversionPermitInvalid, ErrorCodeJoinPolicyConflict} {
		if c == "" || c != strings.ToLower(c) || strings.ContainsAny(c, " -") {
			t.Errorf("error code %q is not lower snake case", c)
		}
	}
}
