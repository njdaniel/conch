package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/njdaniel/conch/pkg/schema"
)

// Member links (issue #985, design §3.3). The rows of the matrix and of the
// agent's table cover the routes; what a preview of A's member link may say
// is the one thing a row cannot state as a refusal, so it is checked here.

// checkMemberLinkPreview requires that the preview of A's member link, as a
// stranger with its code gets it, is exactly InvitePreviewV2 and says the
// inviter's name, the server's and the default channels as they are now, with
// no private channel of A's among them and no field beyond the document's.
func (w *world) checkMemberLinkPreview(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var pv schema.InvitePreviewV2
	if err := dec.Decode(&pv); err != nil {
		return fmt.Errorf("the preview is not exactly an InvitePreviewV2 (%w): %s", err, body)
	}
	// The wire's fields are the document's and nothing else.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	allowed := []string{"kind", "server_name", "inviter", "channels", "expires_at", "join"}
	for k := range fields {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("the preview carries a field %q beyond %v", k, allowed)
		}
	}
	var door schema.JoinInfoV1
	if err := w.op.call(http.MethodGet, "/v1/join", nil, &door); err != nil {
		return err
	}
	var settings schema.JoinSettingsV1
	if err := w.op.call(http.MethodGet, "/v1/join/settings", nil, &settings); err != nil {
		return err
	}
	if pv.Kind != schema.InviteKindMember {
		return fmt.Errorf("the preview's kind is %q, want member", pv.Kind)
	}
	if pv.Inviter != w.a.people[0].name {
		return fmt.Errorf("the preview names the inviter %q, want the link's maker", pv.Inviter)
	}
	if pv.ServerName != door.ServerName {
		return fmt.Errorf("the preview names the server %q, want %q", pv.ServerName, door.ServerName)
	}
	if !slices.Equal(pv.Channels, settings.OpenChannels) {
		return fmt.Errorf("the preview's channels are %v, want the default channels %v", pv.Channels, settings.OpenChannels)
	}
	for _, name := range pv.Channels {
		if slices.Contains(w.a.channels, name) {
			return fmt.Errorf("the preview names %q, a private channel of A's", name)
		}
	}
	if pv.Join.Guest || pv.Join.GuestsFull {
		return fmt.Errorf("the preview offers guests: %+v", pv.Join)
	}
	return nil
}
