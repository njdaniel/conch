package main

import (
	"fmt"
	"net/http"

	"github.com/njdaniel/conch/pkg/schema"
)

// The personal-preset phase (issue #1017, ADR-014, docs/design/admin-roles.md
// §7.7 item 3; it replaces community-model §13.9's hide-members phase and
// absorbs it when #988 lands). The owner, through the real
// PUT /v1/permissions:
//
//  1. applies preset personal, which turns every member permission off but
//     use_voice and share_screen;
//  2. member link creation is refused, 403 invite_not_allowed naming
//     make_member_links (design §7.4: one code per question, whichever check
//     refused), by v2; v1 answers 409 invite_kind_retired to a member
//     whatever the right (issue #990);
//  3. member channel creation is refused, 403 permission_denied naming the
//     create permission for the body's visibility;
//  4. the directory rows (placeholder: #988);
//  5. applies preset group, after which link and channel creation work again
//     and the directory widens (placeholder: #988).
//
// A step that waits for a route that does not exist yet is a marked
// placeholder naming the issue that fills it.

// applyPreset applies a preset as the owner, through the real PUT.
func (w *world) applyPreset(p schema.PermissionPreset) error {
	var doc schema.PermissionsResponseV1
	if err := w.op.call(http.MethodGet, "/v1/permissions", nil, &doc); err != nil {
		return err
	}
	return w.op.call(http.MethodPut, "/v1/permissions", schema.PutPermissionsRequestV1{Revision: doc.Revision, Preset: p}, nil)
}

// wantCreated requires a 201 answer.
func wantCreated(what string, status int, body []byte, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("%s = %d %s, want 201", what, status, body)
	}
	return nil
}

// presetPhase runs the personal-preset phase. It answers how many rows were
// run.
func (w *world) presetPhase() (int, error) {
	tag := newTag(w.tag, w.btag, w.ptag, w.qtag)
	name := func(s string) string { return "pre-" + tag + "-" + s }
	room := name("room")
	if err := w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: room}, nil); err != nil {
		return 0, err
	}
	m, err := w.newPerson(name("member"), room)
	if err != nil {
		return 0, err
	}
	// The person's own brake is open, so that the column is what decides.
	if err := w.op.call(http.MethodPut, "/v1/principals/"+itoa(m.id)+"/invite-permission", schema.InvitePermissionRequestV1{MayInvite: true}, nil); err != nil {
		return 0, err
	}
	memberLink := func() (int, []byte, error) {
		return m.api.do(http.MethodPost, "/v2/invites", schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	}
	createChannel := func(vis schema.ChannelVisibility, label string) (int, []byte, error) {
		return m.api.do(http.MethodPost, "/v1/channels", schema.CreateChannelRequestV1{Name: name(label), Visibility: vis})
	}

	// The control: before the preset, a member makes a member link.
	status, body, err := memberLink()
	if err := wantCreated("a member's member link before the preset", status, body, err); err != nil {
		return 0, err
	}

	// 1. Personal, through the real PUT.
	if err := w.applyPreset(schema.PresetPersonal); err != nil {
		return 0, fmt.Errorf("the owner applies personal: %w", err)
	}
	rows := 0

	// 2. Member link creation: invite_not_allowed, make_member_links.
	status, body, err = memberLink()
	if err := refusedNaming("a member makes a member link under personal", status, body, err, schema.ErrorCodeInviteNotAllowed, schema.PermMakeMemberLinks); err != nil {
		return rows, err
	}
	rows++
	status, body, err = m.api.do(http.MethodPost, "/v1/invites", schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{room}})
	// The v1 route refuses a caller without manage_members before it looks at
	// the body (issue #990): 409 invite_kind_retired, naming no permission.
	if err != nil {
		return rows, fmt.Errorf("a member makes a v1 link under personal: %w", err)
	}
	if e, derr := errorOf(body); status != http.StatusConflict || derr != nil || e.Code != schema.ErrorCodeInviteKindRetired || e.Permission != "" {
		return rows, fmt.Errorf("a member makes a v1 link under personal = %d %s, want 409 %s naming no permission", status, body, schema.ErrorCodeInviteKindRetired)
	}
	rows++

	// 3. Member channel creation: permission_denied, the permission of the
	// visibility asked.
	for _, c := range []struct {
		vis  schema.ChannelVisibility
		perm schema.Permission
	}{
		{schema.ChannelPublic, schema.PermCreatePublicChannels},
		{schema.ChannelPrivate, schema.PermCreatePrivateChannels},
	} {
		status, body, err := createChannel(c.vis, "denied-"+string(c.vis))
		if err := refusedNaming("a member creates a "+string(c.vis)+" channel under personal", status, body, err, schema.ErrorCodePermissionDenied, c.perm); err != nil {
			return rows, err
		}
		rows++
	}

	// 4. PLACEHOLDER (#988): the directory rows. Under personal the member
	// column lacks see_member_directory, so GET /v1/directory/members lists
	// the may-know list (scope shared_channels) and community-model §13.9's
	// rows run unchanged. #988 fills this in; #983's route answers today
	// but the may-know predicate is its.

	// 5. Group: the column is back, and what was refused works.
	if err := w.applyPreset(schema.PresetGroup); err != nil {
		return rows, fmt.Errorf("the owner applies group: %w", err)
	}
	status, body, err = memberLink()
	if err := wantCreated("a member's member link after group", status, body, err); err != nil {
		return rows, err
	}
	status, body, err = createChannel(schema.ChannelPrivate, "back")
	if err := wantCreated("a member creates a private channel after group", status, body, err); err != nil {
		return rows, err
	}
	rows += 2
	// PLACEHOLDER (#988): the directory widens to everyone, and
	// conch.directory_changed.v1 arrives on the member's events socket.
	return rows, nil
}
