package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/pkg/schema"
)

// phoneUA is the User-Agent of the phone-sized client alice uses. The API
// does not look at it; it only makes the run's requests say what the story
// says.
const phoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148"

// joinPassword is the password alice chooses: it contains none of her name.
const joinPassword = "a passphrase long enough to pass"

type step struct {
	name string
	run  func() error
}

// scenario is the community under test and the people in it.
type scenario struct {
	h     *harness
	d     *conchdProc
	lk    *lkit.Server
	adm   *lkit.Admin
	lkBin string
	ogg   string

	owner     api
	ownerID   int64
	general   schema.ChannelV0
	project   schema.ChannelV0
	generalH  []string // bodies posted in #general before alice joined
	projectH  []string // bodies posted in #project before it was public
	link      schema.CreateInviteResponseV2
	alice     api
	aliceID   int64
	aliceName string

	voiceOff time.Duration
}

func newScenario(h *harness, d *conchdProc, lk *lkit.Server, lkBin string) (*scenario, error) {
	s := &scenario{h: h, d: d, lk: lk, adm: lk.Admin(), lkBin: lkBin, owner: d.owner, aliceName: "alice"}
	dir, err := h.mkdir("ogg-")
	if err != nil {
		return nil, err
	}
	s.ogg = filepath.Join(dir, "silence.ogg")
	if err := lkit.WriteSilentOpus(s.ogg, 900); err != nil {
		return nil, err
	}
	var who schema.WhoAmIResponseV1
	if err := s.owner.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return nil, err
	}
	s.ownerID = who.ID

	var created schema.CreateChannelResponseV1
	if err := s.owner.call(http.MethodPost, "/v1/channels", schema.CreateChannelRequestV1{Name: "general", Visibility: schema.ChannelPublic}, &created); err != nil {
		return nil, fmt.Errorf("create #general: %w", err)
	}
	s.general = created.Channel
	yes := true
	if err := s.owner.call(http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", s.general.ID), schema.ChannelSettingsRequestV1{Default: &yes}, nil); err != nil {
		return nil, fmt.Errorf("make #general the default channel: %w", err)
	}
	if err := s.owner.call(http.MethodPost, "/v1/channels", schema.CreateChannelRequestV1{Name: "project", Visibility: schema.ChannelPrivate}, &created); err != nil {
		return nil, fmt.Errorf("create #project: %w", err)
	}
	s.project = created.Channel
	for _, body := range []string{"welcome to general, first", "general has history before you came"} {
		if err := s.post("general", body); err != nil {
			return nil, err
		}
		s.generalH = append(s.generalH, body)
	}
	for _, body := range []string{"project notes, first", "project history from when it was private"} {
		if err := s.post("project", body); err != nil {
			return nil, err
		}
		s.projectH = append(s.projectH, body)
	}
	h.say("ok   the community: the owner (%d), #general (public, the default) and #project (private), each with two messages posted before anyone joins", s.ownerID)
	return s, nil
}

func (s *scenario) post(channel, body string) error {
	return s.owner.call(http.MethodPost, "/v2/channels/"+channel+"/messages", schema.PostMessageRequestV2{Body: body}, nil)
}

func (s *scenario) steps() []step {
	return []step{
		{"the owner's member link; alice at the bare address is offered Sign in only", s.step1},
		{"alice redeems the link, lands in #general with its full history and sees the owner", s.step2},
		{"#project turned public: alice finds it, joins it, reads its history and joins voice", s.step3},
		{"alice's voice switched off removes her from the room within the bound; on, she rejoins", s.step4},
		{"the owner closes the link and it no longer redeems", s.step5},
		{"the server admits no guest", s.step6},
	}
}

// messages lists the bodies of a channel as who reads them.
func messages(a api, channel string) ([]string, error) {
	var resp schema.ListMessagesResponseV2
	if err := a.call(http.MethodGet, "/v2/channels/"+channel+"/messages?limit=100", nil, &resp); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range resp.Messages {
		out = append(out, m.Body)
	}
	return out, nil
}

func hasAll(got, want []string) error {
	for _, w := range want {
		if !slices.Contains(got, w) {
			return fmt.Errorf("the history lacks %q (it has %q)", w, got)
		}
	}
	return nil
}

// ------------------------------------------------------------------- step 1

func (s *scenario) step1() error {
	if err := s.owner.call(http.MethodPost, "/v2/invites", schema.CreateInviteRequestV2{Kind: schema.InviteKindMember}, &s.link); err != nil {
		return fmt.Errorf("make a member link: %w", err)
	}
	s.h.secret("an invite code", s.link.Code)
	if s.link.Invite.Kind != schema.InviteKindMember || s.link.Invite.CreatedBy != s.ownerID || s.link.Invite.ClosedAt != nil {
		return fmt.Errorf("the link is %+v, want an open member link made by the owner", s.link.Invite)
	}
	s.alice = api{h: s.h, base: s.d.baseURL, who: "alice (a phone)", ua: phoneUA}

	var door schema.JoinInfoV1
	if err := s.alice.call(http.MethodGet, "/v1/join", nil, &door); err != nil {
		return err
	}
	if door.Join.Account || door.Join.Guest || door.Join.GuestsFull {
		return fmt.Errorf("the bare address offers %+v, want neither an account nor a guest", door.Join)
	}
	if !door.PasswordLogin {
		return fmt.Errorf("the bare address offers no Sign in (password_login false)")
	}

	var pv schema.InvitePreviewV2
	if err := s.alice.call(http.MethodPost, "/v2/invites/preview", schema.InvitePreviewRequestV1{Code: s.link.Code}, &pv); err != nil {
		return fmt.Errorf("preview the link: %w", err)
	}
	if pv.Kind != schema.InviteKindMember || pv.Inviter != "owner" || pv.ServerName != door.ServerName {
		return fmt.Errorf("the preview is %+v, want a member link from the owner on %q", pv, door.ServerName)
	}
	if !slices.Equal(pv.Channels, []string{"general"}) {
		return fmt.Errorf("the preview names the channels %v, want #general alone (#project is private)", pv.Channels)
	}
	if pv.Join.Guest {
		return fmt.Errorf("the preview offers a guest: %+v", pv.Join)
	}
	return nil
}

// ------------------------------------------------------------------- step 2

func (s *scenario) step2() error {
	var resp schema.RedeemInviteResponseV1
	if err := s.alice.call(http.MethodPost, "/v2/invites/redeem",
		schema.RedeemInviteRequestV2{Code: s.link.Code, Name: s.aliceName, Password: joinPassword}, &resp); err != nil {
		return fmt.Errorf("redeem: %w", err)
	}
	s.h.secret("alice's credential", resp.Token)
	if resp.Principal.Name != s.aliceName || resp.Principal.Kind != schema.PrincipalHuman || resp.Token == "" {
		return fmt.Errorf("redeeming made %+v, want a human named alice with a credential", resp.Principal)
	}
	s.aliceID = resp.Principal.ID
	s.alice.token = resp.Token

	var who schema.WhoAmIResponseV1
	if err := s.alice.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return err
	}
	if who.ID != s.aliceID || who.Name != s.aliceName {
		return fmt.Errorf("whoami says %+v after redeeming as %d", who, s.aliceID)
	}

	var chans schema.ListChannelsResponse
	if err := s.alice.call(http.MethodGet, "/v1/channels", nil, &chans); err != nil {
		return err
	}
	var names []string
	for _, c := range chans.Channels {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"general"}) {
		return fmt.Errorf("alice is in %v, want #general alone", names)
	}

	// Her socket, opened as the browser does: a ticket, then the channel.
	ticket, err := s.alice.ticket()
	if err != nil {
		return err
	}
	live, err := s.dialChannel("general", ticket)
	if err != nil {
		return err
	}
	defer live.close()

	// The full history, posted before she joined.
	got, err := messages(s.alice, "general")
	if err != nil {
		return err
	}
	if err := hasAll(got, s.generalH); err != nil {
		return fmt.Errorf("#general: %w", err)
	}
	// #project is private and she is not in it: not found, not forbidden.
	if err := s.alice.refused(http.MethodGet, "/v2/channels/project/messages", nil, http.StatusNotFound, channelNotFound); err != nil {
		return err
	}

	var dir schema.ServerMembersResponseV1
	if err := s.alice.call(http.MethodGet, "/v1/directory/members", nil, &dir); err != nil {
		return err
	}
	var seenOwner, seenSelf bool
	for _, m := range dir.Members {
		seenOwner = seenOwner || (m.ID == s.ownerID && m.Name == "owner")
		seenSelf = seenSelf || m.ID == s.aliceID
	}
	if !seenOwner || !seenSelf {
		return fmt.Errorf("the member directory lists %+v, want the owner and alice", dir.Members)
	}

	// A message posted afterwards reaches her socket.
	const liveBody = "posted after alice joined"
	if err := s.post("general", liveBody); err != nil {
		return err
	}
	return live.waitFor(liveBody)
}

// socket is an open channel socket and the frames it received.
type socket struct {
	c      *websocket.Conn
	cancel context.CancelFunc
	mu     sync.Mutex
	frames []string
}

func (s *scenario) dialChannel(channel, ticket string) (*socket, error) {
	u := strings.Replace(s.d.baseURL, "http://", "ws://", 1) + "/v2/ws?" + url.Values{"channel": {channel}, "ticket": {ticket}}.Encode()
	ctx, cancel := context.WithCancel(stopCtx)
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	c, resp, err := websocket.Dial(dctx, u, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		cancel()
		if resp != nil {
			return nil, fmt.Errorf("open alice's socket on #%s: status %d: %w", channel, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("open alice's socket on #%s: %w", channel, err)
	}
	sock := &socket{c: c, cancel: cancel}
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			sock.mu.Lock()
			sock.frames = append(sock.frames, string(data))
			sock.mu.Unlock()
		}
	}()
	return sock, nil
}

func (k *socket) close() {
	k.cancel()
	_ = k.c.CloseNow()
}

// waitFor requires a frame carrying text to arrive.
func (k *socket) waitFor(text string) error {
	return waitFor("alice's socket to carry "+strconv.Quote(text), 10*time.Second, func() (bool, string) {
		k.mu.Lock()
		defer k.mu.Unlock()
		for _, f := range k.frames {
			if strings.Contains(f, text) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d frames, none with it", len(k.frames))
	})
}

// ------------------------------------------------------------------- step 3

func (s *scenario) listing() (map[string]schema.ChannelListingV1, error) {
	var dir schema.ChannelDirectoryResponseV1
	if err := s.alice.call(http.MethodGet, "/v1/directory/channels", nil, &dir); err != nil {
		return nil, err
	}
	out := map[string]schema.ChannelListingV1{}
	for _, c := range dir.Channels {
		out[c.Name] = c
	}
	return out, nil
}

func (s *scenario) step3() error {
	// Before: #project is private and alice is not in it, so the directory
	// does not list it and she cannot join it.
	before, err := s.listing()
	if err != nil {
		return err
	}
	if _, ok := before["project"]; ok {
		return fmt.Errorf("the channel directory lists private #project to a non-member")
	}
	if g, ok := before["general"]; !ok || !g.Joined || !g.Default || g.Visibility != schema.ChannelPublic {
		return fmt.Errorf("the directory lists #general as %+v, want public, default and joined", g)
	}
	if err := s.alice.refused(http.MethodPost, "/v1/channels/project/join", nil, http.StatusNotFound, channelNotFound); err != nil {
		return err
	}

	pub := schema.ChannelPublic
	if err := s.owner.call(http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", s.project.ID), schema.ChannelSettingsRequestV1{Visibility: &pub}, nil); err != nil {
		return fmt.Errorf("turn #project public: %w", err)
	}
	after, err := s.listing()
	if err != nil {
		return err
	}
	if p, ok := after["project"]; !ok || p.Joined || p.Visibility != schema.ChannelPublic {
		return fmt.Errorf("the directory lists #project as %+v (present %v), want public and not yet joined", p, ok)
	}

	var joined schema.JoinChannelResponseV1
	if err := s.alice.call(http.MethodPost, "/v1/channels/project/join", nil, &joined); err != nil {
		return fmt.Errorf("join #project: %w", err)
	}
	if joined.Channel.Name != "project" || joined.Member.PrincipalID != s.aliceID {
		return fmt.Errorf("joining answered %+v", joined)
	}
	got, err := messages(s.alice, "project")
	if err != nil {
		return err
	}
	if err := hasAll(got, s.projectH); err != nil {
		return fmt.Errorf("#project, posted while it was private: %w", err)
	}
	after, err = s.listing()
	if err != nil {
		return err
	}
	if !after["project"].Joined {
		return fmt.Errorf("the directory still lists #project as not joined")
	}

	room, hl, err := s.joinVoice("project")
	if err != nil {
		return fmt.Errorf("join voice in #project: %w", err)
	}
	defer hl.Stop()
	if err := s.waitListed(room, true, 60*time.Second); err != nil {
		return err
	}
	return nil
}

// joinVoice asks for a session in channel as alice and joins its room with
// lk. It returns the room and the participant.
func (s *scenario) joinVoice(channel string) (string, *lkit.Headless, error) {
	var sess schema.VoiceSessionResponseV1
	if err := s.alice.call(http.MethodPost, "/v1/channels/"+channel+"/voice/session", nil, &sess); err != nil {
		return "", nil, err
	}
	if len(sess.Rooms) != 1 {
		return "", nil, fmt.Errorf("the session has %d rooms, want 1", len(sess.Rooms))
	}
	g := sess.Rooms[0]
	s.h.secret("a voice join token", g.Token)
	s.h.hide("a voice room name", g.Room)
	hl, err := s.h.kit.StartHeadless(s.lkBin, s.ogg, strings.TrimPrefix(s.lk.WSURL, "ws://"), g.Token)
	if err != nil {
		return "", nil, err
	}
	return g.Room, hl, nil
}

// listed asks LiveKit whether alice (identity p<id>) is in room.
func (s *scenario) listed(room string) (bool, string) {
	ctx, cancel := context.WithTimeout(stopCtx, 5*time.Second)
	defer cancel()
	parts, err := s.adm.ListParticipants(ctx, room)
	if lkit.RoomNotFound(err) {
		return false, "the room does not exist"
	}
	if err != nil {
		return false, "LiveKit could not be asked: " + err.Error()
	}
	var ids []string
	for _, p := range parts {
		ids = append(ids, p.Identity)
		if p.Identity == fmt.Sprintf("p%d", s.aliceID) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("LiveKit lists %v", ids)
}

// gone is true only when LiveKit answered and alice is not in the room (or
// the room is gone); a failed call says nothing.
func (s *scenario) gone(room string) (bool, string) {
	ctx, cancel := context.WithTimeout(stopCtx, 5*time.Second)
	defer cancel()
	parts, err := s.adm.ListParticipants(ctx, room)
	if lkit.RoomNotFound(err) {
		return true, "the room does not exist"
	}
	if err != nil {
		return false, "LiveKit could not be asked: " + err.Error()
	}
	for _, p := range parts {
		if p.Identity == fmt.Sprintf("p%d", s.aliceID) {
			return false, "LiveKit lists her in the room"
		}
	}
	return true, "LiveKit does not list her"
}

func (s *scenario) waitListed(room string, want bool, within time.Duration) error {
	return waitFor(fmt.Sprintf("LiveKit to list alice in the room: %v", want), within, func() (bool, string) {
		ok, saw := s.listed(room)
		return ok == want, saw
	})
}

// ------------------------------------------------------------------- step 4

func (s *scenario) setVoice(allowed bool) error {
	var resp schema.VoicePermissionResponseV1
	path := fmt.Sprintf("/v1/principals/%d/voice-permission", s.aliceID)
	if err := s.owner.call(http.MethodPut, path, schema.VoicePermissionRequestV1{VoiceAllowed: allowed}, &resp); err != nil {
		return err
	}
	if resp.PrincipalID != s.aliceID || resp.VoiceAllowed != allowed || !resp.Changed {
		return fmt.Errorf("the voice switch answered %+v, want alice's voice_allowed %v, changed", resp, allowed)
	}
	return nil
}

func (s *scenario) step4() error {
	room, hl, err := s.joinVoice("project")
	if err != nil {
		return fmt.Errorf("join voice: %w", err)
	}
	defer hl.Stop()
	if err := s.waitListed(room, true, 60*time.Second); err != nil {
		return err
	}

	// LiveKit is watched from before the request, every 2 ms, so the time
	// measured is when it first stopped listing her, not when this program
	// got round to asking.
	type seen struct {
		after time.Duration
		err   error
	}
	found := make(chan seen, 1)
	start := time.Now()
	go func() {
		deadline := start.Add(60 * time.Second)
		for time.Now().Before(deadline) && stopCtx.Err() == nil {
			if ok, _ := s.gone(room); ok {
				found <- seen{after: time.Since(start)}
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		_, saw := s.gone(room)
		found <- seen{err: fmt.Errorf("LiveKit still lists alice 60 s after the switch: %s", saw)}
	}()
	if err := s.setVoice(false); err != nil {
		return fmt.Errorf("switch alice's voice off: %w", err)
	}
	f := <-found
	if f.err != nil {
		return f.err
	}
	s.voiceOff = f.after
	bound, err := readVoiceBound(s.h.repo)
	if err != nil {
		return err
	}
	if s.voiceOff > bound {
		return fmt.Errorf("alice was out of the room after %s, over the bound of %s", s.voiceOff.Round(time.Millisecond), bound)
	}
	s.h.say("     alice out of LiveKit's room %s after the switch (bound %s)", s.voiceOff.Round(100*time.Microsecond), bound)

	// She cannot start again while it is off.
	if err := s.alice.refused(http.MethodPost, "/v1/channels/project/voice/session", nil, http.StatusForbidden, schema.ErrorCodeVoiceNotAllowed); err != nil {
		return err
	}
	// The switch is in the member list the owner reads.
	if err := s.directoryVoice(false); err != nil {
		return err
	}
	hl.Stop()

	if err := s.setVoice(true); err != nil {
		return fmt.Errorf("switch alice's voice on: %w", err)
	}
	if err := s.directoryVoice(true); err != nil {
		return err
	}
	room, hl2, err := s.joinVoice("project")
	if err != nil {
		return fmt.Errorf("rejoin voice after the switch was turned on: %w", err)
	}
	defer hl2.Stop()
	return s.waitListed(room, true, 60*time.Second)
}

// directoryVoice requires the owner's member list to carry alice's switch.
func (s *scenario) directoryVoice(want bool) error {
	var dir schema.ServerMembersResponseV1
	if err := s.owner.call(http.MethodGet, "/v1/directory/members", nil, &dir); err != nil {
		return err
	}
	for _, m := range dir.Members {
		if m.ID == s.aliceID {
			if m.VoiceAllowed == nil || *m.VoiceAllowed != want {
				return fmt.Errorf("the owner's member list has alice's voice_allowed as %v, want %v", m.VoiceAllowed, want)
			}
			return nil
		}
	}
	return fmt.Errorf("the owner's member list lacks alice")
}

// ------------------------------------------------------------------- step 5

func (s *scenario) step5() error {
	if err := s.owner.call(http.MethodDelete, fmt.Sprintf("/v1/invites/%d", s.link.Invite.ID), nil, nil); err != nil {
		return fmt.Errorf("close the link: %w", err)
	}
	nobody := api{h: s.h, base: s.d.baseURL, who: "a second newcomer", ua: phoneUA}
	if err := nobody.refused(http.MethodPost, "/v2/invites/redeem",
		schema.RedeemInviteRequestV2{Code: s.link.Code, Name: "bob", Password: joinPassword}, http.StatusNotFound, schema.ErrorCodeInviteInvalid); err != nil {
		return fmt.Errorf("redeem the closed link: %w", err)
	}
	if err := nobody.refused(http.MethodPost, "/v2/invites/preview",
		schema.InvitePreviewRequestV1{Code: s.link.Code}, http.StatusNotFound, schema.ErrorCodeInviteInvalid); err != nil {
		return fmt.Errorf("preview the closed link: %w", err)
	}
	// No one was made by the refused attempt, and alice, who joined before
	// the link closed, still has her account.
	var dir schema.ServerMembersResponseV1
	if err := s.owner.call(http.MethodGet, "/v1/directory/members", nil, &dir); err != nil {
		return err
	}
	for _, m := range dir.Members {
		if m.Name == "bob" {
			return fmt.Errorf("a person named bob exists after the closed link was refused")
		}
	}
	return s.alice.call(http.MethodGet, "/v1/whoami", nil, nil)
}

// ------------------------------------------------------------------- step 6

func (s *scenario) step6() error {
	yes := true
	if err := s.owner.refused(http.MethodPut, "/v1/join/settings", schema.PutJoinSettingsRequestV1{GuestsEnabled: &yes},
		http.StatusConflict, schema.ErrorCodeJoinPolicyConflict); err != nil {
		return err
	}
	var settings schema.JoinSettingsV1
	if err := s.owner.call(http.MethodGet, "/v1/join/settings", nil, &settings); err != nil {
		return err
	}
	if settings.GuestsEnabled {
		return fmt.Errorf("guests_enabled reads true after the refused PUT")
	}
	nobody := api{h: s.h, base: s.d.baseURL, who: "a visitor", ua: phoneUA}
	var door schema.JoinInfoV1
	if err := nobody.call(http.MethodGet, "/v1/join", nil, &door); err != nil {
		return err
	}
	if door.Join.Guest {
		return fmt.Errorf("the bare address offers a guest: %+v", door.Join)
	}
	return nobody.refused(http.MethodPost, "/v1/join/guest", nil, http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
}

// --------------------------------------------------------------- the bound

// readVoiceBound is design §2.5's bound on a voice switch: at worst one
// poller pass plus the lifetime of a join token already minted. Both are read
// from the server's source, so the bound follows the code.
func readVoiceBound(repo string) (time.Duration, error) {
	pass, err := durationConst(repo, "voice_presence.go", "voicePassGapMax")
	if err != nil {
		return 0, err
	}
	life, err := durationConst(repo, "voice.go", "voiceTokenLifetime")
	if err != nil {
		return 0, err
	}
	return pass + life, nil
}

var durationRe = `\b%s\s*=\s*(\d+)\s*\*\s*time\.(Millisecond|Second)\b`

// durationConst reads a constant written as <integer> * time.<Unit> from a
// file of internal/server.
func durationConst(repo, file, name string) (time.Duration, error) {
	path := filepath.Join(repo, "internal", "server", file)
	src, err := os.ReadFile(path) // #nosec G304 -- the repository this run is in
	if err != nil {
		return 0, err
	}
	m := regexp.MustCompile(fmt.Sprintf(durationRe, name)).FindStringSubmatch(string(src))
	if m == nil {
		return 0, fmt.Errorf("%s: cannot read %s as <integer> * time.<Unit>", path, name)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: %s is %q", path, name, m[1])
	}
	unit := time.Millisecond
	if m[2] == "Second" {
		unit = time.Second
	}
	return time.Duration(n) * unit, nil
}

// channelNotFound is the code of a channel route answer about a channel the
// caller cannot see; the server writes it as a literal.
const channelNotFound = "channel_not_found"
