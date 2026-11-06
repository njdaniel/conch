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

// checkMemberLinkRedemption redeems A's member link as a newcomer (issue
// #986) and requires that the newcomer lands in the default channels as they
// are now and in nothing else: never in a private channel of A's, which the
// link's maker, ann, is in.
func (w *world) checkMemberLinkRedemption() error {
	// From an address of its own, so the redemption spends nothing of the
	// per-address budget the groups' loopback shares with later phases.
	name := w.bName("newcomer")
	var resp schema.RedeemInviteResponseV1
	if err := w.newcomer(name+" (joining)").call(http.MethodPost, "/v2/invites/redeem",
		schema.RedeemInviteRequestV2{Code: w.a.memberLinkCode, Name: name, Password: joinPassword}, &resp); err != nil {
		return fmt.Errorf("redeem A's member link: %w", err)
	}
	w.h.secret(name+"'s credential", resp.Token)
	m := &member{name: name, id: resp.Principal.ID, token: resp.Token, credID: resp.Credential.ID, password: true}
	m.api = w.as(m)
	var settings schema.JoinSettingsV1
	if err := w.op.call(http.MethodGet, "/v1/join/settings", nil, &settings); err != nil {
		return err
	}
	var chans schema.ListChannelsResponse
	if err := m.api.call(http.MethodGet, "/v1/channels", nil, &chans); err != nil {
		return err
	}
	got := []string{}
	for _, c := range chans.Channels {
		if slices.Contains(w.a.channels, c.Name) {
			return fmt.Errorf("the newcomer through A's member link is in %q, a private channel of A's", c.Name)
		}
		got = append(got, c.Name)
	}
	want := append([]string{}, settings.OpenChannels...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("the newcomer through A's member link is in %v, want the default channels %v", got, want)
	}
	return nil
}
