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
//     refused), by v2 and by v1;
//  3. member channel creation is refused, 403 permission_denied naming the
//     create permission for the body's visibility;
//  4. the directory rows (issue #1190, community-model §13.9; hiderows.go):
//     the directory narrows to the people the viewer may know, bob's and
//     zed's names and ids are markers in alice's register across the routes
//     she reads, carol is known through her link, the authors route names
//     the departed author alone, and conch.directory_changed.v1 reaches
//     only the viewers whose answer changed;
//  5. applies preset group, after which link and channel creation work again,
//     the directory widens and the signal arrives.
//
// Both applications are signalled to the four sockets the phase holds open.

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

	// The hide-members world (hiderows.go), made while members still hold
	// see_member_directory, with its sockets open.
	mtag := newTag(w.tag, w.btag, w.ptag, w.qtag, tag)
	hw, err := w.buildHideWorld(name, mtag, newTag(w.tag, w.btag, w.ptag, w.qtag, tag, mtag))
	if err != nil {
		return 0, fmt.Errorf("the hide-members world: %w", err)
	}
	defer hw.closeSockets()

	// 1. Personal, through the real PUT: every member and operator with a
	// socket is told the directory may have changed.
	if err := hw.signalStep("the owner applies personal",
		func() error { return w.applyPreset(schema.PresetPersonal) },
		hw.aliceF, hw.bobF, hw.zedF, hw.ownerF); err != nil {
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
	if err := refusedNaming("a member makes a v1 link under personal", status, body, err, schema.ErrorCodeInviteNotAllowed, schema.PermMakeMemberLinks); err != nil {
		return rows, err
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

	// 4. The directory rows: under personal the member column lacks
	// see_member_directory, so the directory lists the may-know list (scope
	// shared_channels) and community-model §13.9's rows run.
	n, err := w.hidePersonalRows(hw)
	rows += n
	if err != nil {
		return rows, fmt.Errorf("the directory under personal: %w", err)
	}

	// 5. Group: the column is back, and what was refused works.
	if err := hw.signalStep("the owner applies group",
		func() error { return w.applyPreset(schema.PresetGroup) },
		hw.aliceF, hw.bobF, hw.zedF, hw.ownerF); err != nil {
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
	// The directory widens to everyone (the signal arrived above, on every
	// socket).
	n, err = w.hideGroupRows(hw)
	rows += n
	if err != nil {
		return rows, fmt.Errorf("the directory under group: %w", err)
	}
	return rows, nil
}
