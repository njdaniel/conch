package schema

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestInviteV2GoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"invite-v2.json", func() any { return new(InviteV2) }},
		{"invite-v2-no-expiry.json", func() any { return new(InviteV2) }},
		{"invite-v2-closed.json", func() any { return new(InviteV2) }},
		{"invite-v2-legacy.json", func() any { return new(InviteV2) }},
		{"invite-v2-landing.json", func() any { return new(InviteV2) }},
		{"create-invite-request-v2.json", func() any { return new(CreateInviteRequestV2) }},
		{"create-invite-request-v2-default.json", func() any { return new(CreateInviteRequestV2) }},
		{"create-invite-request-v2-never-expires.json", func() any { return new(CreateInviteRequestV2) }},
		{"create-invite-request-v2-landing.json", func() any { return new(CreateInviteRequestV2) }},
		{"create-invite-response-v2.json", func() any { return new(CreateInviteResponseV2) }},
		{"list-invites-response-v2.json", func() any { return new(ListInvitesResponseV2) }},
		{"list-invites-response-v2-empty.json", func() any { return new(ListInvitesResponseV2) }},
		{"invite-preview-v2.json", func() any { return new(InvitePreviewV2) }},
		{"invite-preview-v2-no-expiry.json", func() any { return new(InvitePreviewV2) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// The request fixtures are requests the schema accepts.
func TestCreateInviteRequestV2FixturesValidate(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, file := range []string{
		"create-invite-request-v2.json", "create-invite-request-v2-default.json",
		"create-invite-request-v2-never-expires.json", "create-invite-request-v2-landing.json",
	} {
		t.Run(file, func(t *testing.T) {
			var r CreateInviteRequestV2
			decodeFixture(t, file, &r)
			if err := r.Validate(now); err != nil {
				t.Errorf("Validate = %v, want nil", err)
			}
		})
	}
}

// The v1 invite family is published and unchanged: its fixtures are
// byte-identical (scripts/schema-compat.sh) and its key sets are what they
// were. The v2 shapes sit beside them.
func TestInviteV1UnchangedBesideV2(t *testing.T) {
	want := map[string][]string{
		"InviteV1":              {"channels", "closed_at", "created_at", "created_by", "credential_lifetime_seconds", "expires_at", "id", "join_as", "kind", "max_uses", "may_invite", "other_channels", "uses", "voice"},
		"CreateInviteRequestV1": {"channels", "credential_lifetime_seconds", "expires_at", "join_as", "kind", "max_uses", "may_invite", "voice"},
	}
	got := map[string][]string{
		"InviteV1":              structJSONTags(reflect.TypeOf(InviteV1{})),
		"CreateInviteRequestV1": structJSONTags(reflect.TypeOf(CreateInviteRequestV1{})),
	}
	for name, keys := range want {
		if !reflect.DeepEqual(got[name], keys) {
			t.Errorf("%s keys = %v, want %v", name, got[name], keys)
		}
	}
}

// structJSONTags is the sorted json names of every field of a struct,
// omitempty ones included (jsonKeys marshals a value, so it drops them).
func structJSONTags(typ reflect.Type) []string {
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		keys = append(keys, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(keys)
	return keys
}

// TestInviteV2HasNoSecretOrGuestFields pins the JSON key set of a fully
// populated v2 invite: no code or hash, and none of the guest-link fields the
// amendment of 2026-10-30 left out (voice).
func TestInviteV2HasNoSecretOrGuestFields(t *testing.T) {
	want := []string{
		"channels", "closed_at", "created_at", "created_by", "created_by_name", "credential_lifetime_seconds",
		"expires_at", "id", "joiners", "kind", "landing_channels", "legacy", "max_uses", "other_channels", "uses",
	}
	ts := NewTimestamp(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	one := int64(1)
	raw, err := json.Marshal(InviteV2{
		ID: 1, Kind: InviteKindMember, CreatedAt: ts, ExpiresAt: &ts, MaxUses: &one, ClosedAt: &ts,
		Channels: []InviteChannelV1{{ID: 3, Name: "general"}}, LandingChannels: []InviteChannelV1{{ID: 9, Name: "belgium"}},
		OtherChannels: 1, Legacy: true,
	})
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InviteV2 keys = %v, want %v", got, want)
	}
}

// None of the dropped guest-link fields exist on any v2 shape (ADR-013
// amendment of 2026-10-30; design §9.1): no voice anywhere, no channels on
// the create request, and no manages (ADR-010) at any depth.
func TestInviteV2TypesHaveNoGuestOrManagesFields(t *testing.T) {
	forbidden := map[reflect.Type][]string{
		reflect.TypeOf(InviteV2{}):               {"voice", "manages"},
		reflect.TypeOf(CreateInviteRequestV2{}):  {"voice", "channels", "manages"},
		reflect.TypeOf(CreateInviteResponseV2{}): {"voice", "manages"},
		reflect.TypeOf(ListInvitesResponseV2{}):  {"voice", "manages"},
		reflect.TypeOf(InvitePreviewV2{}):        {"voice", "manages"},
	}
	for root, tags := range forbidden {
		seen := map[reflect.Type]bool{}
		walkStructFields(root, seen, func(owner reflect.Type, f reflect.StructField) {
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			for _, forbid := range tags {
				if tag == forbid {
					t.Errorf("%s.%s has forbidden json tag %q", owner, f.Name, tag)
				}
			}
		})
	}
}

// The request decodes strictly: a channels or voice field, which a member
// link does not have, is refused as unknown rather than dropped, and so is
// any v1 field v2 does not carry.
func TestCreateInviteRequestV2RefusesUnknownFields(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"member","channels":["general"]}`,
		`{"kind":"member","voice":true}`,
		`{"kind":"member","may_invite":true}`,
		`{"kind":"member","join_as":"account"}`,
	} {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var r CreateInviteRequestV2
		if err := dec.Decode(&r); err == nil {
			t.Errorf("%s decoded into CreateInviteRequestV2", raw)
		}
	}
	// And the fields it does have decode.
	dec := json.NewDecoder(strings.NewReader(`{"kind":"member","expires_at":"2026-11-06T12:00:00Z","never_expires":false,"max_uses":5,"credential_lifetime_seconds":60,"landing_channels":["belgium"]}`))
	dec.DisallowUnknownFields()
	var r CreateInviteRequestV2
	if err := dec.Decode(&r); err != nil {
		t.Errorf("decode: %v", err)
	}
}

func TestCreateInviteRequestV2Validate(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *Timestamp { ts := NewTimestamp(now.Add(d)); return &ts }
	tsOf := func(t time.Time) *Timestamp { ts := NewTimestamp(t); return &ts }
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
		req     CreateInviteRequestV2
		wantErr string
	}{
		// The kind.
		{"member defaults", CreateInviteRequestV2{Kind: InviteKindMember}, ""},
		{"no kind", CreateInviteRequestV2{}, "invite kind must be one of: member"},
		{"guest is not a kind", CreateInviteRequestV2{Kind: "guest"}, "invite kind must be one of: member"},
		{"a v1 kind", CreateInviteRequestV2{Kind: "open"}, "invite kind must be one of: member"},
		{"personal is not a kind", CreateInviteRequestV2{Kind: "personal"}, "invite kind must be one of: member"},
		// The expiry: in the future, at most MaxMemberLinkExpirySeconds ahead,
		// never the no-expiry sentinel, and never with never_expires.
		{"expiry in the future", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: at(time.Hour)}, ""},
		{"expiry at the bound", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: at(time.Duration(MaxMemberLinkExpirySeconds) * time.Second)}, ""},
		{"expiry past the bound", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: at(time.Duration(MaxMemberLinkExpirySeconds)*time.Second + time.Second)}, "expires_at must be within 31536000 seconds"},
		{"expiry far ahead", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: at(400 * 24 * time.Hour)}, "expires_at must be within 31536000 seconds"},
		// The sentinel, and anything past it, is "never" to a reader; only
		// never_expires may ask for that, and the store gates it (S1).
		{"expiry at the never sentinel", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: tsOf(time.UnixMilli(NoExpiryMillis))}, "expires_at cannot mean never; ask never_expires"},
		{"expiry past the never sentinel", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: tsOf(time.UnixMilli(NoExpiryMillis + 1000))}, "expires_at cannot mean never; ask never_expires"},
		{"expiry just before the never sentinel", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: tsOf(time.UnixMilli(NoExpiryMillis - 1000))}, "expires_at must be within 31536000 seconds"},
		{"expiry now", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: at(0)}, "expires_at must be in the future"},
		{"expiry past", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: at(-time.Hour)}, "expires_at must be in the future"},
		{"never expires", CreateInviteRequestV2{Kind: InviteKindMember, NeverExpires: true}, ""},
		{"never expires with an expiry", CreateInviteRequestV2{Kind: InviteKindMember, NeverExpires: true, ExpiresAt: at(time.Hour)}, "never_expires cannot be combined with expires_at"},
		{"never expires with a past expiry", CreateInviteRequestV2{Kind: InviteKindMember, NeverExpires: true, ExpiresAt: at(-time.Hour)}, "never_expires cannot be combined with expires_at"},
		// The use cap.
		{"max_uses one", CreateInviteRequestV2{Kind: InviteKindMember, MaxUses: i64(1)}, ""},
		{"max_uses at the bound", CreateInviteRequestV2{Kind: InviteKindMember, MaxUses: i64(MaxInviteMaxUses)}, ""},
		{"max_uses zero", CreateInviteRequestV2{Kind: InviteKindMember, MaxUses: i64(0)}, "max_uses must be between 1 and"},
		{"max_uses negative", CreateInviteRequestV2{Kind: InviteKindMember, MaxUses: i64(-1)}, "max_uses must be between 1 and"},
		{"max_uses over the bound", CreateInviteRequestV2{Kind: InviteKindMember, MaxUses: i64(MaxInviteMaxUses + 1)}, "max_uses must be between 1 and"},
		// The credential lifetime, as in v1.
		{"lifetime one second", CreateInviteRequestV2{Kind: InviteKindMember, CredentialLifetimeSeconds: i64(1)}, ""},
		{"lifetime at max", CreateInviteRequestV2{Kind: InviteKindMember, CredentialLifetimeSeconds: i64(MaxInviteCredentialLifetimeSeconds)}, ""},
		{"lifetime zero", CreateInviteRequestV2{Kind: InviteKindMember, CredentialLifetimeSeconds: i64(0)}, "credential_lifetime_seconds must be between 1 and"},
		{"lifetime over max", CreateInviteRequestV2{Kind: InviteKindMember, CredentialLifetimeSeconds: i64(MaxInviteCredentialLifetimeSeconds + 1)}, "credential_lifetime_seconds must be between 1 and"},
		// Landing channels (O5): 1 to MaxInviteChannels distinct valid names.
		{"one landing channel", CreateInviteRequestV2{Kind: InviteKindMember, LandingChannels: []string{"belgium"}}, ""},
		{"32 landing channels", CreateInviteRequestV2{Kind: InviteKindMember, LandingChannels: names(MaxInviteChannels)}, ""},
		{"33 landing channels", CreateInviteRequestV2{Kind: InviteKindMember, LandingChannels: names(MaxInviteChannels + 1)}, "at most 32 landing channels"},
		{"an empty landing list", CreateInviteRequestV2{Kind: InviteKindMember, LandingChannels: []string{}}, "landing_channels names 1 to 32 channels, got 0"},
		{"a blank landing channel", CreateInviteRequestV2{Kind: InviteKindMember, LandingChannels: []string{" "}}, "landing channel name must not be blank"},
		{"a landing channel with a control character", CreateInviteRequestV2{Kind: InviteKindMember, LandingChannels: []string{"a\x1bb"}}, "control characters"},
		{"an overlong landing channel", CreateInviteRequestV2{Kind: InviteKindMember, LandingChannels: []string{strings.Repeat("a", MaxChannelNameLength+1)}}, "at most"},
		{"a landing channel twice", CreateInviteRequestV2{Kind: InviteKindMember, LandingChannels: []string{"a", "a"}}, "landing channel named twice"},
		// Everything together.
		{"everything", CreateInviteRequestV2{Kind: InviteKindMember, ExpiresAt: at(24 * time.Hour), MaxUses: i64(10), CredentialLifetimeSeconds: i64(3600), LandingChannels: []string{"belgium", "general"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(now), tt.wantErr)
		})
	}
}

func TestCreateInviteRequestV2EffectiveCredentialLifetime(t *testing.T) {
	life := int64(60)
	if got := (CreateInviteRequestV2{Kind: InviteKindMember}).EffectiveCredentialLifetime(); got != 90*24*time.Hour {
		t.Errorf("default lifetime = %v, want 90 days", got)
	}
	if got := (CreateInviteRequestV2{Kind: InviteKindMember, CredentialLifetimeSeconds: &life}).EffectiveCredentialLifetime(); got != time.Minute {
		t.Errorf("explicit lifetime = %v, want 1m", got)
	}
}

// InviteKindV2 has exactly one value, member: the vocabulary is closed and
// client-tolerant, so guest returns later as an addition (#1004).
func TestInviteKindV2(t *testing.T) {
	if got := Vocabularies["InviteKindV2"]; !reflect.DeepEqual(got, []string{"member"}) {
		t.Errorf("InviteKindV2 = %q, want [member]", got)
	}
	for v, want := range map[InviteKindV2]bool{InviteKindMember: true, "guest": false, "open": false, "personal": false, "": false} {
		if got := v.Valid(); got != want {
			t.Errorf("InviteKindV2(%q).Valid() = %v, want %v", v, got, want)
		}
	}
}

func TestInviteV2Constants(t *testing.T) {
	if InviteKindMember != "member" {
		t.Errorf("InviteKindMember = %q", InviteKindMember)
	}
	if DefaultMemberLinkExpirySeconds != 7*24*60*60 {
		t.Errorf("DefaultMemberLinkExpirySeconds = %d, want 7 days", DefaultMemberLinkExpirySeconds)
	}
	if MaxMemberLinkExpirySeconds != 365*24*60*60 {
		t.Errorf("MaxMemberLinkExpirySeconds = %d, want one year", MaxMemberLinkExpirySeconds)
	}
	if DefaultMemberLinkExpirySeconds > MaxMemberLinkExpirySeconds {
		t.Error("the default member-link expiry exceeds the bound")
	}
	// 9999-12-31T23:59:59Z, the largest instant a reader treats as an
	// expiry at all.
	if want := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC).UnixMilli(); NoExpiryMillis != want {
		t.Errorf("NoExpiryMillis = %d, want %d", NoExpiryMillis, want)
	}
	if ErrorCodeInviteKindRetired != "invite_kind_retired" {
		t.Errorf("ErrorCodeInviteKindRetired = %q", ErrorCodeInviteKindRetired)
	}
}

func TestListInvitesResponseV2EmptyEncodesArray(t *testing.T) {
	got, err := json.Marshal(ListInvitesResponseV2{Invites: []InviteV2{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"invites":[]}` {
		t.Fatalf("got %s", got)
	}
}
