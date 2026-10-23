package schema

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestInviteGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"invite-v1.json", func() any { return new(InviteV1) }},
		{"invite-v1-open.json", func() any { return new(InviteV1) }},
		{"create-invite-request-v1.json", func() any { return new(CreateInviteRequestV1) }},
		{"create-invite-request-v1-open.json", func() any { return new(CreateInviteRequestV1) }},
		{"create-invite-response-v1.json", func() any { return new(CreateInviteResponseV1) }},
		{"invite-preview-request-v1.json", func() any { return new(InvitePreviewRequestV1) }},
		{"invite-preview-v1.json", func() any { return new(InvitePreviewV1) }},
		{"redeem-invite-request-v1.json", func() any { return new(RedeemInviteRequestV1) }},
		{"redeem-invite-response-v1.json", func() any { return new(RedeemInviteResponseV1) }},
		{"list-invites-response-v1.json", func() any { return new(ListInvitesResponseV1) }},
		{"list-invites-response-v1-empty.json", func() any { return new(ListInvitesResponseV1) }},
		{"whoami-response-v1-may-invite.json", func() any { return new(WhoAmIResponseV1) }},
		// Operator administration (issue #438).
		{"invite-joiners-response-v1.json", func() any { return new(InviteJoinersResponseV1) }},
		{"invite-joiners-response-v1-empty.json", func() any { return new(InviteJoinersResponseV1) }},
		{"disable-joiners-request-v1.json", func() any { return new(DisableJoinersRequestV1) }},
		{"disable-joiners-request-v1-keep-open.json", func() any { return new(DisableJoinersRequestV1) }},
		{"disable-joiners-response-v1.json", func() any { return new(DisableJoinersResponseV1) }},
		{"disable-joiners-response-v1-dry-run.json", func() any { return new(DisableJoinersResponseV1) }},
		{"disable-invitees-request-v1.json", func() any { return new(DisableInviteesRequestV1) }},
		{"disable-invitees-request-v1-direct.json", func() any { return new(DisableInviteesRequestV1) }},
		{"invite-permission-request-v1.json", func() any { return new(InvitePermissionRequestV1) }},
		{"invite-settings-v1.json", func() any { return new(InviteSettingsV1) }},
		// Join modes (ADR-009, issue #704).
		{"invite-v1-guest.json", func() any { return new(InviteV1) }},
		{"create-invite-request-v1-guest.json", func() any { return new(CreateInviteRequestV1) }},
		{"invite-preview-v1-guest.json", func() any { return new(InvitePreviewV1) }},
		{"invite-preview-v1-guests-off.json", func() any { return new(InvitePreviewV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestInviteCodeFixturesAreWellFormed(t *testing.T) {
	if err := ValidateInviteCode("conch_inv_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatalf("fixture code: %v", err)
	}
}

// TestInviteV1HasNoSecretFields pins the JSON key set of a fully populated
// invite: no code or hash may ever appear on it.
func TestInviteV1HasNoSecretFields(t *testing.T) {
	want := []string{
		"channels", "closed_at", "created_at", "created_by", "credential_lifetime_seconds",
		"expires_at", "id", "kind", "max_uses", "may_invite", "uses", "voice",
	}
	ts := NewTimestamp(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	one := int64(1)
	raw, err := json.Marshal(InviteV1{ID: 1, Kind: InviteKindOpen, CreatedAt: ts, ExpiresAt: ts, MaxUses: &one, ClosedAt: &ts})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("InviteV1 keys = %v, want %v", got, want)
	}
}

func TestInvitePreviewV1Keys(t *testing.T) {
	raw, err := json.Marshal(InvitePreviewV1{})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if want := "channels,expires_at,inviter,kind,voice"; strings.Join(got, ",") != want {
		t.Errorf("InvitePreviewV1 keys = %v, want %s", got, want)
	}
}

func TestCreateInviteRequestV1Validate(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *Timestamp { ts := NewTimestamp(now.Add(d)); return &ts }
	i64 := func(n int64) *int64 { return &n }
	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "c" + strings.Repeat("x", i)
		}
		return out
	}
	tests := []struct {
		name    string
		req     CreateInviteRequestV1
		wantErr bool
	}{
		{"personal defaults", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"general"}}, false},
		{"open defaults", CreateInviteRequestV1{Kind: InviteKindOpen, Channels: []string{"general"}}, false},
		{"unknown kind", CreateInviteRequestV1{Kind: "guest", Channels: []string{"general"}}, true},
		{"no kind", CreateInviteRequestV1{Channels: []string{"general"}}, true},
		{"personal expiry at max", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a"}, ExpiresAt: at(MaxPersonalInviteLifetime)}, false},
		{"personal expiry over max", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a"}, ExpiresAt: at(MaxPersonalInviteLifetime + time.Second)}, true},
		{"open expiry at max", CreateInviteRequestV1{Kind: InviteKindOpen, Channels: []string{"a"}, ExpiresAt: at(MaxOpenInviteLifetime)}, false},
		{"open expiry over max", CreateInviteRequestV1{Kind: InviteKindOpen, Channels: []string{"a"}, ExpiresAt: at(MaxOpenInviteLifetime + time.Second)}, true},
		{"expiry now", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a"}, ExpiresAt: at(0)}, true},
		{"expiry past", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a"}, ExpiresAt: at(-time.Hour)}, true},
		{"personal with max_uses", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a"}, MaxUses: i64(1)}, true},
		{"open with max_uses", CreateInviteRequestV1{Kind: InviteKindOpen, Channels: []string{"a"}, MaxUses: i64(25)}, false},
		{"open max_uses zero", CreateInviteRequestV1{Kind: InviteKindOpen, Channels: []string{"a"}, MaxUses: i64(0)}, true},
		{"open max_uses negative", CreateInviteRequestV1{Kind: InviteKindOpen, Channels: []string{"a"}, MaxUses: i64(-1)}, true},
		{"open max_uses over bound", CreateInviteRequestV1{Kind: InviteKindOpen, Channels: []string{"a"}, MaxUses: i64(MaxInviteMaxUses + 1)}, true},
		{"no channels", CreateInviteRequestV1{Kind: InviteKindPersonal}, true},
		{"32 channels", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: names(32)}, false},
		{"33 channels", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: names(33)}, true},
		{"blank channel", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"  "}}, true},
		{"control character in channel", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a\x1bb"}}, true},
		{"overlong channel", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{strings.Repeat("a", MaxChannelNameLength+1)}}, true},
		{"duplicate channel", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a", "a"}}, true},
		{"lifetime zero", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a"}, CredentialLifetimeSeconds: i64(0)}, true},
		{"lifetime at max", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a"}, CredentialLifetimeSeconds: i64(MaxInviteCredentialLifetimeSeconds)}, false},
		{"lifetime over max", CreateInviteRequestV1{Kind: InviteKindPersonal, Channels: []string{"a"}, CredentialLifetimeSeconds: i64(MaxInviteCredentialLifetimeSeconds + 1)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.req.Validate(now); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreateInviteRequestV1Effective(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	exp := NewTimestamp(now.Add(time.Hour))
	life := int64(60)
	tests := []struct {
		name         string
		req          CreateInviteRequestV1
		wantExpiry   time.Time
		wantMayInv   bool
		wantLifetime time.Duration
	}{
		{"personal defaults", CreateInviteRequestV1{Kind: InviteKindPersonal}, now.Add(7 * 24 * time.Hour), false, 90 * 24 * time.Hour},
		{"open defaults", CreateInviteRequestV1{Kind: InviteKindOpen}, now.Add(24 * time.Hour), false, 90 * 24 * time.Hour},
		{"personal says no", CreateInviteRequestV1{Kind: InviteKindPersonal, MayInvite: &no}, now.Add(7 * 24 * time.Hour), false, 90 * 24 * time.Hour},
		{"open says yes", CreateInviteRequestV1{Kind: InviteKindOpen, MayInvite: &yes}, now.Add(24 * time.Hour), true, 90 * 24 * time.Hour},
		{"explicit", CreateInviteRequestV1{Kind: InviteKindOpen, ExpiresAt: &exp, CredentialLifetimeSeconds: &life}, exp.Time(), false, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.req.EffectiveExpiry(now); !got.Equal(tt.wantExpiry) {
				t.Errorf("EffectiveExpiry = %v, want %v", got, tt.wantExpiry)
			}
			if got := tt.req.EffectiveMayInvite(); got != tt.wantMayInv {
				t.Errorf("EffectiveMayInvite = %v, want %v", got, tt.wantMayInv)
			}
			if got := tt.req.EffectiveCredentialLifetime(); got != tt.wantLifetime {
				t.Errorf("EffectiveCredentialLifetime = %v, want %v", got, tt.wantLifetime)
			}
		})
	}
}

func TestValidateInviteCode(t *testing.T) {
	good := InviteCodePrefix + strings.Repeat("A", 43)
	tests := []struct {
		name    string
		code    string
		wantErr bool
	}{
		{"well formed", good, false},
		{"url-safe characters", InviteCodePrefix + strings.Repeat("a-_9", 10) + "Zzz", false},
		{"empty", "", true},
		{"credential token", CredentialTokenPrefix + strings.Repeat("A", 43), true},
		{"too short", good[:len(good)-1], true},
		{"too long", good + "A", true},
		{"padding", InviteCodePrefix + strings.Repeat("A", 42) + "=", true},
		{"space", InviteCodePrefix + strings.Repeat("A", 42) + " ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateInviteCode(tt.code)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateInviteCode = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && tt.code != "" && strings.Contains(err.Error(), tt.code) {
				t.Errorf("error echoes the code: %v", err)
			}
		})
	}
}

func TestInviteRequestValidate(t *testing.T) {
	good := InviteCodePrefix + strings.Repeat("A", 43)
	preview := []struct {
		name    string
		req     InvitePreviewRequestV1
		wantErr bool
	}{
		{"ok", InvitePreviewRequestV1{Code: good}, false},
		{"bad code", InvitePreviewRequestV1{Code: "nope"}, true},
	}
	for _, tt := range preview {
		t.Run("preview "+tt.name, func(t *testing.T) {
			if err := tt.req.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
	redeem := []struct {
		name    string
		req     RedeemInviteRequestV1
		wantErr bool
	}{
		{"ok", RedeemInviteRequestV1{Code: good, Name: "river"}, false},
		{"bad code", RedeemInviteRequestV1{Code: "nope", Name: "river"}, true},
		{"blank name", RedeemInviteRequestV1{Code: good, Name: " "}, true},
		{"control name", RedeemInviteRequestV1{Code: good, Name: "ri\x1bver"}, true},
		{"overlong name", RedeemInviteRequestV1{Code: good, Name: strings.Repeat("a", MaxPrincipalNameLength+1)}, true},
	}
	for _, tt := range redeem {
		t.Run("redeem "+tt.name, func(t *testing.T) {
			if err := tt.req.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDisableJoinersRequestV1EffectiveClose(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name string
		req  DisableJoinersRequestV1
		want bool
	}{
		{"absent closes", DisableJoinersRequestV1{}, true},
		{"true closes", DisableJoinersRequestV1{Close: &yes}, true},
		{"false keeps it open", DisableJoinersRequestV1{Close: &no}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.req.EffectiveClose(); got != tt.want {
				t.Errorf("EffectiveClose = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDisableInviteesRequestV1(t *testing.T) {
	tests := []struct {
		name      string
		req       DisableInviteesRequestV1
		wantErr   bool
		wantDepth InviteeDepth
	}{
		{"absent is direct", DisableInviteesRequestV1{}, false, InviteeDepthDirect},
		{"direct", DisableInviteesRequestV1{Depth: InviteeDepthDirect}, false, InviteeDepthDirect},
		{"all", DisableInviteesRequestV1{Depth: InviteeDepthAll}, false, InviteeDepthAll},
		{"unknown", DisableInviteesRequestV1{Depth: "2"}, true, "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.req.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
			if got := tt.req.EffectiveDepth(); got != tt.wantDepth {
				t.Errorf("EffectiveDepth = %q, want %q", got, tt.wantDepth)
			}
		})
	}
}

// An invite admits accounts unless it says otherwise, and an unknown mode
// is refused rather than read as one of the known ones.
func TestCreateInviteRequestV1JoinAs(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	base := CreateInviteRequestV1{Kind: InviteKindOpen, Channels: []string{"lobby"}}
	tests := []struct {
		name    string
		joinAs  InviteJoinMode
		wantErr string
		want    InviteJoinMode
	}{
		{"absent", "", "", InviteJoinAccount},
		{"account", InviteJoinAccount, "", InviteJoinAccount},
		{"guest", InviteJoinGuest, "", InviteJoinGuest},
		{"both", InviteJoinBoth, "", InviteJoinBoth},
		{"off is not a mode an invite has", "off", "join_as", ""},
		{"unknown", "anyone", "join_as", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			req.JoinAs = tt.joinAs
			err := req.Validate(now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Validate = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if got := req.EffectiveJoinAs(); got != tt.want {
				t.Errorf("EffectiveJoinAs = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestInviteErrorCodes(t *testing.T) {
	want := map[string]string{
		ErrorCodeInviteInvalid:             "invite_invalid",
		ErrorCodeInvitesPaused:             "invites_paused",
		ErrorCodeInviteAllowanceExceeded:   "invite_allowance_exceeded",
		ErrorCodeInviteGrantExceedsInviter: "invite_grant_exceeds_inviter",
		ErrorCodeInviteNotAllowed:          "invite_not_allowed",
		ErrorCodeInviteNotFound:            "invite_not_found",
		ErrorCodePrincipalExists:           "principal_exists",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("code = %q, want %q", got, w)
		}
	}
}

// TestInviteTypesHaveNoManagesField pins ADR-010's invariant that an invite
// never grants channel management: no invite request or response type carries
// a manages field or json key at any depth.
func TestInviteTypesHaveNoManagesField(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(InviteChannelV1{}),
		reflect.TypeOf(InviteV1{}),
		reflect.TypeOf(CreateInviteRequestV1{}),
		reflect.TypeOf(CreateInviteResponseV1{}),
		reflect.TypeOf(InvitePreviewRequestV1{}),
		reflect.TypeOf(InvitePreviewV1{}),
		reflect.TypeOf(RedeemInviteRequestV1{}),
		reflect.TypeOf(RedeemInviteRequestV2{}),
		reflect.TypeOf(RedeemInviteGuestRequestV1{}),
		reflect.TypeOf(RedeemInviteResponseV1{}),
		reflect.TypeOf(ListInvitesResponseV1{}),
		reflect.TypeOf(InviteJoinerV1{}),
		reflect.TypeOf(InviteJoinersResponseV1{}),
		reflect.TypeOf(DisableJoinersRequestV1{}),
		reflect.TypeOf(DisableJoinersResponseV1{}),
		reflect.TypeOf(DisableInviteesRequestV1{}),
		reflect.TypeOf(InvitePermissionRequestV1{}),
		reflect.TypeOf(InviteSettingsV1{}),
	}
	seen := map[reflect.Type]bool{}
	for _, root := range types {
		walkStructFields(root, seen, func(owner reflect.Type, f reflect.StructField) {
			if f.Name == "Manages" {
				t.Errorf("%s declares forbidden field Manages", owner)
			}
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "manages" {
				t.Errorf("%s.%s has forbidden json tag %q", owner, f.Name, tag)
			}
		})
	}
}
