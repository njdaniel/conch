package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// The additive fields of the community model (ADR-013, issue #978) on
// published shapes: each decodes strictly into its type, so the server's
// decoder accepts it, and the new JoinWarning value is known.

func TestCommunityAdditiveFieldsDecodeStrictly(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		into func() any
	}{
		{"ChannelV0 visibility and default", `{"id":7,"name":"general","created_at":"2026-07-13T12:34:56Z","visibility":"public","default":true}`, func() any { return new(ChannelV0) }},
		{"ChannelDocV1 visibility and default", `{"schema":"conch.channel.v1","channel_id":7,"name":"general","created_at":"2026-07-13T12:34:56.000Z","visibility":"private"}`, func() any { return new(ChannelDocV1) }},
		{"PutJoinSettingsRequestV1 open_signup", `{"open_signup":true}`, func() any { return new(PutJoinSettingsRequestV1) }},
		{"PutJoinSettingsRequestV1 member_link_default_expiry_seconds", `{"member_link_default_expiry_seconds":0}`, func() any { return new(PutJoinSettingsRequestV1) }},
		{"JoinSettingsV1 Option B fields", `{"open_joining":"off","guests_enabled":false,"joining_paused":false,"join_voice":true,"max_guests":50,"max_guests_per_address":5,"guest_idle_seconds":7200,"guest_absolute_seconds":86400,"open_channels":[],"open_signup":false,"member_link_default_expiry_seconds":604800}`, func() any { return new(JoinSettingsV1) }},
		{"JoinSettingsResponseV1 default_channels", `{"settings":{"open_joining":"off","guests_enabled":false,"joining_paused":false,"join_voice":true,"max_guests":50,"max_guests_per_address":5,"guest_idle_seconds":7200,"guest_absolute_seconds":86400,"open_channels":[]},"server_name":"c","live_guests":0,"warnings":[],"default_channels":[{"id":3,"name":"general"}]}`, func() any { return new(JoinSettingsResponseV1) }},
		{"PrincipalAdminV1 voice_allowed and may_invite", `{"id":12,"kind":"human","name":"wren","role":"member","created_at":"2026-10-15T13:00:00.000Z","live_credentials":1,"voice_allowed":false,"may_invite":true}`, func() any { return new(PrincipalAdminV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(tt.raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(tt.into()); err != nil {
				t.Errorf("decode: %v", err)
			}
		})
	}
}

// JoinWarning grows by exactly no_default_channel (ADR-013 consequences):
// the vocabulary is closed and client-tolerant, so the addition is recorded,
// not a bump.
func TestJoinWarningGainsNoDefaultChannel(t *testing.T) {
	if !JoinWarning("no_default_channel").Valid() {
		t.Error(`JoinWarning("no_default_channel").Valid() = false, want true`)
	}
	want := []string{"live_guests_remain", "trusted_proxy_unset", "one_address", "reserved_names_in_use", "server_name_in_use", "no_default_channel"}
	if got := Vocabularies["JoinWarning"]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("JoinWarning = %q, want %q", got, want)
	}
}
