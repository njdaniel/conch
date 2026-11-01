package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The plaza and its guests (issue #717, docs/design/guests-and-accounts.md
// §9): a third part of the world, a channel the bare address admits to.
//
// Two members make their accounts at the bare address before any guest
// comes (POST /v1/join/account), and the plaza fills with what a channel
// holds: a thread, a whisper, a net's message, a file, an agent's run with
// its activity and an intervention. All of it is registered as the plaza's
// private markers (w.priv), as A's things are A's markers: no guest whose
// visit began later may see any of it. Then the guests come, from the bare
// address and through invites, each from a client address of its own, and
// the members carry on (guests are removed for now, issue #1005: their
// visits are started beside the server, seed.go, as the entry routes did): some of what they say after is for the guests (a
// message, a whisper to one of them, a file, a run), and some is private
// too (a reply in the thread from before, a whisper to the other member,
// activity on the old run).
//
// Every caller of the plaza comes through the trusted proxy with an address
// of its own (api.addr, newAddr). That is what lets the entry routes run for
// real here: the per-address allowance of the redemption routes (ten in ten
// minutes) is the address's, not one the whole run shares, and the
// per-address cap on live visits counts each guest's own.

// plaza is the plaza, its members and what was said in it.
type plaza struct {
	channel string
	chanID  int64
	// pia and pam made their accounts at the bare address; bot is an agent
	// the operator made, with a session in the plaza.
	pia, pam   *member
	bot        *member
	botSession int64
	netName    string
	netID      int64

	// From before the visits began: private markers.
	preRoot, preReply, preWhisper, preNetMsg, preFileMsg int64
	preFile, preRun, preIV                               int64
	preWord                                              string

	// After they began, for the guests to see: the message every guest
	// message socket must carry (control), the whisper to the first guest,
	// the run on the new message (task, which every events socket must
	// carry) and a file.
	postRoot      int64
	control       string
	whisperToNick string
	postRun       int64
	task          string
	postFile      int64

	// invites are the operator's open invites to the plaza, by what they
	// admit.
	invites map[schema.InviteJoinMode]plazaInvite
	// cohort is the guests the rows run as and the scenarios act on.
	nick, nick2, ola, sam *member
	// nickHeld is what the first guest holds: every socket variant, the
	// occupancy socket, a ticket and (with LiveKit) a participant.
	nickHeld holder
}

// plazaInvite is one of the operator's invites to the plaza.
type plazaInvite struct {
	id   int64
	code string
}

// privName is a private string of the plaza: its run tag and hex, never a
// word, as A's are (world.aName).
func (w *world) privName(s string) string {
	sum := sha256.Sum256([]byte(w.ptag + "/" + s))
	return "early-" + w.ptag + "-" + hex.EncodeToString(sum[:4])
}

// pubName is any other string of the plaza. It is not scanned for, so it may
// be plain; its tag is not the private one.
func (w *world) pubName(s string) string { return "plaza-" + w.qtag + "-" + s }

// newAddr is a client address of a caller's own, from the documentation
// ranges (RFC 5737), so it is nobody's.
func (w *world) newAddr() string {
	w.addrs++
	if w.addrs <= 254 {
		return fmt.Sprintf("198.51.100.%d", w.addrs)
	}
	return fmt.Sprintf("203.0.113.%d", w.addrs-254)
}

// newcomer is someone with no credential, at an address of their own.
func (w *world) newcomer(who string) api {
	return api{h: w.h, base: w.d.baseURL, who: who, addr: w.newAddr()}
}

// errorCode is the code of an error document, "" when body is not one.
func errorCode(body []byte) string {
	var e schema.Error
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return e.Code
}

// entry makes a request of a route that is limited server-wide (the
// redemptions and joins that succeed: twenty a minute) and answers what it
// was answered. The limit is the server's for all of this run's newcomers
// together, and the run makes more than twenty of them in a few minutes, so
// a 429 rate_limited is waited out, by its Retry-After, rather than taken
// for the answer; every other answer, guests_full included, is the answer.
func (w *world) entry(a api, method, path string, body any) (int, []byte, error) {
	status, data, _, _, err := w.entryAt(a, method, path, body)
	return status, data, err
}

// entryAt is entry, and the time the request that was answered was sent
// (what a revocation it makes is timed from) and how long it waited for the
// server-wide limit first. Every wait is said in the run's output, so a run
// never claims something happened at a moment it did not.
func (w *world) entryAt(a api, method, path string, body any) (int, []byte, time.Time, time.Duration, error) {
	var waited time.Duration
	for tries := 0; ; tries++ {
		sent := time.Now()
		status, data, hdr, err := a.request(method, path, "application/json", mustJSON(body))
		if err != nil || status != http.StatusTooManyRequests || errorCode(data) != schema.ErrorCodeRateLimited || tries == 5 {
			return status, data, sent, waited, err
		}
		retry, _ := strconv.Atoi(hdr.Get("Retry-After"))
		wait := time.Duration(max(retry, 1)) * time.Second
		w.h.say("     %s %s, as %s: the server-wide redemption limit says wait %s; waiting", method, path, a.who, wait)
		select {
		case <-time.After(wait):
			waited += wait
		case <-stopCtx.Done():
			return 0, nil, sent, waited, errInterrupted
		}
	}
}

// entryCall is entry for a request that must succeed, decoding the answer.
func (w *world) entryCall(a api, method, path string, body, out any) error {
	status, data, err := w.entry(a, method, path, body)
	if err != nil {
		return fmt.Errorf("%s %s as %s: %w", method, path, a.who, err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("%s %s as %s: status %d: %s", method, path, a.who, status, w.h.redact(string(data)))
	}
	return json.Unmarshal(data, out)
}

// joinAccount makes an account at the bare address, from an address of its
// own.
func (w *world) joinAccount(name string) (*member, error) {
	from := w.newcomer(name + " (joining)")
	var resp schema.JoinAccountResponseV1
	if err := w.entryCall(from, http.MethodPost, "/v1/join/account", schema.JoinAccountRequestV1{Name: name, Password: joinPassword}, &resp); err != nil {
		return nil, err
	}
	m := &member{name: name, id: resp.Principal.ID, token: resp.Token, credID: resp.Credential.ID, password: true, addr: from.addr}
	m.api = w.as(m)
	w.h.secret(name+"'s credential", resp.Token)
	return m, nil
}

// guestJoined is a member for the visit an entry route answered.
func (w *world) guestJoined(resp schema.JoinGuestResponseV1, addr string) (*member, error) {
	g := resp.Guest
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("the visit the server answered: %w", err)
	}
	if resp.Principal.Name != g.Handle || schema.ValidateGuestHandle(g.Handle) != nil {
		return nil, fmt.Errorf("a guest is named %q with handle %q: its name must be its handle", resp.Principal.Name, g.Handle)
	}
	m := &member{name: g.Handle, id: resp.Principal.ID, token: resp.Token, credID: resp.Credential.ID, addr: addr, guest: &g}
	m.api = w.as(m)
	w.h.secret(g.Handle+"'s credential", resp.Token)
	return m, nil
}

// joinGuest starts a visit from the bare address, from an address of its own
// unless from is given. No route starts one now (issue #1005): it is started
// beside the server, as POST /v1/join/guest did (seedVisit).
func (w *world) joinGuest(label string, from *api) (*member, error) {
	a := w.newcomer("a guest (joining)")
	if from != nil {
		a = *from
	}
	return w.seedVisit(a, label, nil)
}

// redeemGuest starts a visit through one of the plaza's invites, beside the
// server, as POST /v1/invites/redeem-guest did (seedVisit).
func (w *world) redeemGuest(mode schema.InviteJoinMode, label string) (*member, error) {
	inv := w.p.invites[mode]
	return w.seedVisit(w.newcomer("a guest (joining by invite)"), label, &inv)
}

// joinSettings changes the joining policy as the operator.
func (w *world) joinSettings(req schema.PutJoinSettingsRequestV1) (schema.JoinSettingsResponseV1, error) {
	var resp schema.JoinSettingsResponseV1
	err := w.op.call(http.MethodPut, "/v1/join/settings", req, &resp)
	return resp, err
}

// ptr is a pointer to v, for the settings' optional fields.
func ptr[T any](v T) *T { return &v }

// ppost posts a v2 message in the plaza as m; private says it is one of the
// plaza's private markers.
func (w *world) ppost(m *member, private bool, what string, req schema.PostMessageRequestV2) (int64, error) {
	var resp schema.PostMessageResponseV2
	if err := m.api.call(http.MethodPost, "/v2/channels/"+url.PathEscape(w.p.channel)+"/messages", req, &resp); err != nil {
		return 0, err
	}
	if private {
		w.priv.str("the plaza's "+what, req.Body)
		w.priv.id("message", "the plaza's "+what, resp.Message.ID)
	}
	return resp.Message.ID, nil
}

// upload uploads a text file to the plaza as m.
func (w *world) upload(m *member, name, contents string) (int64, error) {
	status, data, err := m.api.send(http.MethodPost, "/v2/channels/"+url.PathEscape(w.p.channel)+"/files?"+url.Values{"filename": {name}}.Encode(), "text/plain", []byte(contents))
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated {
		return 0, fmt.Errorf("%s uploads a file: status %d: %s", m.name, status, w.h.redact(string(data)))
	}
	var file schema.FileV1
	if err := json.Unmarshal(data, &file); err != nil {
		return 0, err
	}
	return file.ID, nil
}

// attachment is a v2 message carrying file.
func attachment(body string, file int64) schema.PostMessageRequestV2 {
	att, _ := json.Marshal(schema.FileAttachmentV1{FileIDs: []int64{file}})
	return schema.PostMessageRequestV2{Body: body, Payload: &schema.Payload{Schema: schema.FileAttachmentV1Name, Data: att}}
}

// buildPlaza makes the plaza, its two members and its agent, and fills it
// with what is there before any guest comes.
func (w *world) buildPlaza() error {
	w.priv = newMarkers()
	p := &plaza{invites: map[schema.InviteJoinMode]plazaInvite{}}
	w.p = p
	p.channel = w.pubName("square")
	var created schema.CreateChannelResponse
	if err := w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: p.channel}, &created); err != nil {
		return err
	}
	p.chanID = created.Channel.ID

	// The bare address opens to accounts in the plaza; guests are off.
	if _, err := w.joinSettings(schema.PutJoinSettingsRequestV1{OpenJoining: ptr(schema.OpenJoiningAccount), GuestsEnabled: ptr(false),
		OpenChannels: &[]string{p.channel}}); err != nil {
		return err
	}
	var err error
	if p.pia, err = w.joinAccount(w.pubName("pia")); err != nil {
		return err
	}
	if p.pam, err = w.joinAccount(w.pubName("pam")); err != nil {
		return err
	}
	if p.bot, err = w.newAgent(w.pubName("bot"), &group{channels: []string{p.channel}, chanIDs: []int64{p.chanID}}); err != nil {
		return err
	}
	var sess schema.SessionV1
	if err := p.bot.api.call(http.MethodPost, "/v1/sessions", schema.CreateSessionRequestV1{Client: w.pubName("agent"), Runtime: "fixture",
		Interventions: []schema.InterventionKind{schema.InterventionKindMessage}}, &sess); err != nil {
		return err
	}
	p.botSession = sess.ID

	// A net of the two members.
	p.netName = w.privName("net")
	w.priv.str("the plaza's net", p.netName)
	var net schema.CreateNetResponseV1
	if err := w.op.call(http.MethodPost, "/v1/channels/"+url.PathEscape(p.channel)+"/nets", schema.CreateNetRequestV1{Name: p.netName}, &net); err != nil {
		return err
	}
	p.netID = net.Net.ID
	w.priv.id("net", "the plaza's net", net.Net.ID)
	for _, m := range []*member{p.pia, p.pam} {
		if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/nets/%s/members/%d", url.PathEscape(p.channel), p.netName, m.id),
			schema.PutNetMemberRequestV1{Role: schema.NetRoleMember}, nil); err != nil {
			return err
		}
	}

	// What the plaza holds before any guest comes.
	p.preWord = w.privName("word-root")
	if p.preRoot, err = w.ppost(p.pia, true, "message from before", schema.PostMessageRequestV2{Body: p.preWord}); err != nil {
		return err
	}
	if p.preReply, err = w.ppost(p.pam, true, "reply from before", schema.PostMessageRequestV2{Body: w.privName("word-reply"), ThreadRootID: p.preRoot}); err != nil {
		return err
	}
	if p.preWhisper, err = w.ppost(p.pia, true, "whisper from before", schema.PostMessageRequestV2{Body: w.privName("word-whisper"),
		Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{p.pam.id}}}); err != nil {
		return err
	}
	if p.preNetMsg, err = w.ppost(p.pia, true, "net message from before", schema.PostMessageRequestV2{Body: w.privName("word-net"),
		Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: p.netID}}); err != nil {
		return err
	}
	fileName, contents := w.privName("plans")+".txt", w.privName("file-contents")
	w.priv.str("the plaza's file name from before", fileName)
	w.priv.str("the plaza's file contents from before", contents)
	if p.preFile, err = w.upload(p.pia, fileName, contents); err != nil {
		return err
	}
	w.priv.id("file", "the plaza's file from before", p.preFile)
	if p.preFileMsg, err = w.ppost(p.pia, true, "attachment from before", attachment(w.privName("word-attached"), p.preFile)); err != nil {
		return err
	}
	task := w.privName("task")
	w.priv.str("the plaza's run task from before", task)
	var run schema.RunV1
	if err := p.bot.api.call(http.MethodPost, fmt.Sprintf("/v1/sessions/%d/runs", p.botSession), schema.CreateRunRequestV1{Channel: p.channel, MessageID: p.preRoot, Task: task}, &run); err != nil {
		return err
	}
	p.preRun = run.ID
	w.priv.id("run", "the plaza's run from before", run.ID)
	summary := w.privName("activity")
	w.priv.str("the plaza's run activity from before", summary)
	if err := p.bot.api.call(http.MethodPost, fmt.Sprintf("/v1/runs/%d/activity", run.ID), schema.AppendActivityRequestV1{Kind: schema.ActivityKindRunStarted, Summary: summary}, nil); err != nil {
		return err
	}
	nudge := w.privName("nudge")
	w.priv.str("the plaza's intervention from before", nudge)
	var iv schema.InterventionV1
	if err := p.pia.api.call(http.MethodPost, fmt.Sprintf("/v1/runs/%d/interventions", run.ID), schema.CreateInterventionRequestV1{Kind: schema.InterventionKindMessage, Body: nudge}, &iv); err != nil {
		return err
	}
	p.preIV = iv.ID
	w.priv.id("intervention", "the plaza's intervention from before", iv.ID)

	// The operator's invites to the plaza, one for each mode: the two that
	// admit guests as they were made before guests were removed (seed.go).
	for _, mode := range []schema.InviteJoinMode{schema.InviteJoinBoth, schema.InviteJoinGuest, schema.InviteJoinAccount} {
		if p.invites[mode], err = w.legacyInvite(mode); err != nil {
			return err
		}
	}
	return nil
}

// openPlaza is the policy the cohort joins under: the address open to
// accounts (the most it may be, guests being removed), voice for its
// joiners, and the default bounds, which the visits started beside the
// server take.
func (w *world) openPlaza() error {
	_, err := w.joinSettings(schema.PutJoinSettingsRequestV1{OpenJoining: ptr(schema.OpenJoiningAccount), JoiningPaused: ptr(false),
		JoinVoice: ptr(true), MaxGuests: ptr(int64(schema.DefaultMaxGuests)), MaxGuestsPerAddress: ptr(int64(schema.DefaultMaxGuestsPerAddress)),
		GuestIdleSeconds: ptr(int64(schema.DefaultGuestIdleSeconds)), GuestAbsoluteSeconds: ptr(int64(schema.DefaultGuestAbsoluteSeconds))})
	return err
}

// buildCohort lets the cohort in: two guests labelled alike at the bare
// address, one through the guest-only invite, and one who will create an
// account. The first holds every socket and, with LiveKit, a participant in
// the plaza's room. Then the members say what they say after the visits
// began.
func (w *world) buildCohort() (*feed, error) {
	p := w.p
	var err error
	if p.nick, err = w.joinGuest("Nick", nil); err != nil {
		return nil, err
	}
	if p.nick2, err = w.joinGuest("Nick", nil); err != nil {
		return nil, err
	}
	if p.ola, err = w.redeemGuest(schema.InviteJoinGuest, "Ola"); err != nil {
		return nil, err
	}
	if p.sam, err = w.joinGuest("Sam", nil); err != nil {
		return nil, err
	}
	if p.nickHeld, err = w.hold(p.nick, p.channel, ""); err != nil {
		return nil, err
	}
	if w.lk != nil && p.nick.guest == nil {
		// A guest's grant is to join the plaza's room, and nothing more.
		claims, err := videoGrant(p.nickHeld.grant)
		if err != nil {
			return nil, err
		}
		if claims["room"] != p.nickHeld.room || claims["roomAdmin"] == true || claims["roomList"] == true || claims["roomCreate"] == true || claims["roomRecord"] == true {
			return nil, fmt.Errorf("the first guest's voice grant grants more than joining the plaza's room: %v", grantKeys(claims))
		}
		if err := w.joinVoice(p.nick, p.nickHeld.room); err != nil {
			return nil, err
		}
	}
	// pia's own message socket, on which the guests' posts must come with
	// their attribution.
	piaFeed, r, err := dialSocket(p.pia.api, "pia's /v2/ws on the plaza", "/v2/ws", url.Values{"channel": {p.channel}}, false)
	if err != nil {
		return nil, err
	}
	if piaFeed == nil {
		return nil, fmt.Errorf("pia's socket: status %d %s", r.status, w.h.redact(string(r.body)))
	}

	// After the visits began. The private things first and the controls
	// last, so that a socket that has carried its control has been sent
	// everything before it.
	if _, err := w.ppost(p.pia, true, "reply in the thread from before", schema.PostMessageRequestV2{Body: w.privName("old-thread-reply"), ThreadRootID: p.preRoot}); err != nil {
		return nil, err
	}
	if _, err := w.ppost(p.pia, true, "whisper to the other member", schema.PostMessageRequestV2{Body: w.privName("whisper-to-pam"),
		Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{p.pam.id}}}); err != nil {
		return nil, err
	}
	late := w.privName("late-activity")
	w.priv.str("the plaza's activity on the run from before", late)
	if err := p.bot.api.call(http.MethodPost, fmt.Sprintf("/v1/runs/%d/activity", p.preRun), schema.AppendActivityRequestV1{Kind: schema.ActivityKindRunStarted, Summary: late}, nil); err != nil {
		return nil, err
	}
	p.whisperToNick = w.pubName("whisper-to-nick")
	if _, err := w.ppost(p.pia, false, "", schema.PostMessageRequestV2{Body: p.whisperToNick,
		Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{p.nick.id}}}); err != nil {
		return nil, err
	}
	if p.postFile, err = w.upload(p.pia, w.pubName("notes")+".txt", w.pubName("notes-contents")); err != nil {
		return nil, err
	}
	if _, err := w.ppost(p.pia, false, "", attachment(w.pubName("attached"), p.postFile)); err != nil {
		return nil, err
	}
	p.control = w.pubName("control")
	if p.postRoot, err = w.ppost(p.pia, false, "", schema.PostMessageRequestV2{Body: p.control}); err != nil {
		return nil, err
	}
	if _, err := w.ppost(p.pam, false, "", schema.PostMessageRequestV2{Body: w.pubName("reply"), ThreadRootID: p.postRoot}); err != nil {
		return nil, err
	}
	p.task = w.pubName("task")
	var run schema.RunV1
	if err := p.bot.api.call(http.MethodPost, fmt.Sprintf("/v1/sessions/%d/runs", p.botSession), schema.CreateRunRequestV1{Channel: p.channel, MessageID: p.postRoot, Task: p.task}, &run); err != nil {
		return nil, err
	}
	p.postRun = run.ID
	return piaFeed, nil
}

// joinVoice has m join the plaza's room in LiveKit with a fresh grant, and
// waits until LiveKit lists them; room is the room their held grant names.
// The participant is lk, a full client, not a bare signalling connection
// (lkit.DialSignal): a guest's must stay in for a minute and more, and in
// this check's trials LiveKit stopped listing a bare one well before its
// deadline came.
func (w *world) joinVoice(m *member, room string) error {
	if m.guest != nil {
		return nil // a guest holds no voice since A4 (#1016)
	}
	sess, err := w.session(m, w.p.channel)
	if err != nil {
		return err
	}
	if sess.Rooms[0].Room != room {
		return fmt.Errorf("%s's two grants name different rooms", m.name)
	}
	if _, err := w.h.kit.StartLK(w.lk.lkBin, w.lk.hostPort(), sess.Rooms[0].Token, nil); err != nil {
		return fmt.Errorf("%s joins the plaza's room: %w", m.name, err)
	}
	return waitFor("LiveKit to list "+m.name, 30*time.Second, func() (bool, string) {
		ok, _, saw := w.lk.has(room, m.id)
		return ok, saw
	})
}

// checkGuestSockets waits until each socket the first guest holds has
// carried what the members said for the guests after her visit began (the
// control message on a message socket, the whisper to her on v2, the new
// run's task on the events socket), then requires that none of its frames
// holds a private marker or anything of A's, and that it is open. Then a v2
// socket that replays from the start holds the same: the history from her
// bound, and nothing private.
func (w *world) checkGuestSockets() error {
	p := w.p
	for _, f := range p.nickHeld.sockets {
		var want []string
		switch {
		case strings.Contains(f.what, "/v2/ws"):
			want = []string{p.control, p.whisperToNick}
		case strings.Contains(f.what, "/v0/ws"), strings.Contains(f.what, "/v1/ws"):
			want = []string{p.control}
		case strings.Contains(f.what, "/v1/events/ws"):
			want = []string{p.task}
		}
		for _, s := range want {
			if err := waitFor(f.what+" to carry "+s, 30*time.Second, func() (bool, string) {
				frames := f.received()
				for _, fr := range frames {
					if bytes.Contains(fr, []byte(s)) {
						return true, ""
					}
				}
				return false, fmt.Sprintf("%d frames", len(frames))
			}); err != nil {
				return err
			}
		}
		for i, fr := range f.received() {
			if leaks := w.leaks(fr, asked{}); len(leaks) > 0 {
				return fmt.Errorf("frame %d on %s holds %s", i, f.what, strings.Join(leaks, "; "))
			}
		}
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s closed", f.what)
		}
	}
	for _, g := range []*member{p.nick, p.ola} {
		replay, r, err := dialSocket(g.api, g.name+"'s replaying /v2/ws", "/v2/ws", url.Values{"channel": {p.channel}, "after": {"0"}}, false)
		if err != nil {
			return err
		}
		if replay == nil {
			return fmt.Errorf("%s's replaying socket: status %d %s", g.name, r.status, w.h.redact(string(r.body)))
		}
		err = waitFor(replay.what+" to replay up to the control", 30*time.Second, func() (bool, string) {
			for _, fr := range replay.received() {
				if bytes.Contains(fr, []byte(p.control)) {
					return true, ""
				}
			}
			return false, fmt.Sprintf("%d frames", len(replay.received()))
		})
		if err == nil {
			for i, fr := range replay.received() {
				if leaks := w.leaks(fr, asked{}); len(leaks) > 0 {
					err = fmt.Errorf("replayed frame %d on %s holds %s", i, replay.what, strings.Join(leaks, "; "))
					break
				}
			}
		}
		replay.close()
		if err != nil {
			return err
		}
	}
	return nil
}

// messageIn finds message id in the plaza's v2 history as a reads it.
func (w *world) messageIn(a api, id int64) (schema.MessageV2, bool, error) {
	for after := int64(0); ; {
		var page schema.ListMessagesResponseV2
		if err := a.call(http.MethodGet, fmt.Sprintf("/v2/channels/%s/messages?after=%d&limit=100", url.PathEscape(w.p.channel), after), nil, &page); err != nil {
			return schema.MessageV2{}, false, err
		}
		for _, m := range page.Messages {
			if m.ID == id {
				return m, true, nil
			}
		}
		if page.NextAfter <= after {
			return schema.MessageV2{}, false, nil
		}
		after = page.NextAfter
	}
}

// attributed requires that a message carries exactly the attribution of g's
// visit: the handle, the label and the tag it was posted under.
func attributed(m schema.MessageV2, g schema.GuestV1) error {
	want := schema.GuestAttributionV1{Handle: g.Handle, Label: g.Label, Tag: g.Tag}
	if m.GuestAuthor == nil || *m.GuestAuthor != want {
		return fmt.Errorf("message %d is attributed %+v, want %+v", m.ID, m.GuestAuthor, want)
	}
	return nil
}

// checkIdentity: two visits with the same label are two handles with two
// tags; neither label, nor a handle, signs in; a guest's post comes to a
// member's socket and into the history with its visit's attribution, and
// an operator cannot rename it, hide it or give its principal what a guest
// may not have. It returns the first guest's post.
func (w *world) checkIdentity(piaFeed *feed) (int64, error) {
	p := w.p
	n1, n2 := p.nick.guest, p.nick2.guest
	if n1.Label != "Nick" || n2.Label != "Nick" || n1.Handle == n2.Handle || n1.Tag == n2.Tag || n1.Tag != schema.GuestTag(n1.Handle) {
		return 0, fmt.Errorf("two visits labelled Nick: handles %q and %q, tags %q and %q, labels %q and %q", n1.Handle, n2.Handle, n1.Tag, n2.Tag, n1.Label, n2.Label)
	}
	from := w.newcomer("someone signing in")
	for _, name := range []string{"Nick", n1.Handle} {
		status, body, err := from.do(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: name, Password: joinPassword})
		if err != nil {
			return 0, err
		}
		if err := checkRefusal(result{status: status, body: body}, answer{http.StatusUnauthorized, schema.ErrorCodeAuthFailed}, asked{}); err != nil {
			return 0, fmt.Errorf("signing in as %q: %w", name, err)
		}
	}
	// A label is no principal reference either: a route that takes one by
	// id is not told a label means anybody.
	status, body, err := w.op.do(http.MethodGet, "/v1/principals/Nick/credentials", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusBadRequest {
		return 0, fmt.Errorf("the operator lists the credentials of principal \"Nick\": status %d %s, want 400", status, w.h.redact(string(body)))
	}

	// Nick posts; pia is sent it, and reads it, as Nick's visit.
	var posted schema.PostMessageResponseV2
	if err := p.nick.api.call(http.MethodPost, "/v2/channels/"+url.PathEscape(p.channel)+"/messages", schema.PostMessageRequestV2{Body: w.pubName("from-nick")}, &posted); err != nil {
		return 0, err
	}
	if err := attributed(posted.Message, *n1); err != nil {
		return 0, fmt.Errorf("the answer to Nick's post: %w", err)
	}
	if err := waitFor("pia's socket to carry Nick's post", 30*time.Second, func() (bool, string) {
		for _, fr := range piaFeed.received() {
			// A v2 socket's frame is the message itself.
			var m schema.MessageV2
			if json.Unmarshal(fr, &m) == nil && m.ID == posted.Message.ID {
				return attributed(m, *n1) == nil, fmt.Sprintf("the frame attributes it %+v", m.GuestAuthor)
			}
		}
		return false, fmt.Sprintf("%d frames", len(piaFeed.received()))
	}); err != nil {
		return 0, err
	}
	if err := w.checkAttribution(p.pia.api, posted.Message.ID, *n1); err != nil {
		return 0, err
	}

	// What an operator may not do to a live guest.
	for _, c := range []struct {
		what   string
		method string
		path   string
		body   any
		want   answer
	}{
		{"hides a guest", http.MethodPost, fmt.Sprintf("/v1/principals/%d/hide", p.nick2.id), nil, answer{http.StatusConflict, schema.ErrorCodeGuestNotHideable}},
		{"renames a guest", http.MethodPut, fmt.Sprintf("/v1/principals/%d/name", p.nick2.id), schema.RenamePrincipalRequestV1{Name: w.pubName("renamed")},
			answer{http.StatusConflict, schema.ErrorCodeGuestNotHideable}},
		{"issues a guest a second credential", http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", p.nick2.id), schema.CreateCredentialRequestV1{Label: "second"},
			answer{http.StatusForbidden, schema.ErrorCodeGuestNotAllowed}},
		{"lets a guest invite", http.MethodPut, fmt.Sprintf("/v1/principals/%d/invite-permission", p.nick2.id), schema.InvitePermissionRequestV1{MayInvite: true},
			answer{http.StatusForbidden, schema.ErrorCodeGuestNotAllowed}},
	} {
		status, body, err := w.op.do(c.method, c.path, c.body)
		if err != nil {
			return 0, err
		}
		if err := checkRefusal(result{status: status, body: body}, c.want, asked{ids: []kindID{{"principal", p.nick2.id}}}); err != nil {
			return 0, fmt.Errorf("the operator %s: %w", c.what, err)
		}
	}
	for _, purpose := range []schema.SignInLinkPurpose{schema.SignInLinkPurposeSignIn, schema.SignInLinkPurposeReset} {
		if err := refusedForTarget(w.op, p.nick2.id, purpose, "the operator makes a "+string(purpose)+" link for a guest"); err != nil {
			return 0, err
		}
	}
	var who schema.WhoAmIResponseV1
	if err := p.nick2.api.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return 0, err
	}
	if who.Name != n2.Handle || who.MayInvite || who.IdentityClass != schema.IdentityGuest {
		return 0, fmt.Errorf("after the operator's refused writes the second Nick is %q (%s), may invite %v", who.Name, who.IdentityClass, who.MayInvite)
	}
	return posted.Message.ID, nil
}

// checkAttribution requires that a reads message id with g's attribution.
func (w *world) checkAttribution(a api, id int64, g schema.GuestV1) error {
	m, ok, err := w.messageIn(a, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s cannot read message %d", a.who, id)
	}
	if err := attributed(m, g); err != nil {
		return fmt.Errorf("as %s reads it: %w", a.who, err)
	}
	return nil
}
