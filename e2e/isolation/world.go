package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/pkg/schema"
)

// The world: one server, an operator, and two groups that know nothing of
// each other. Group A joins through an open link the operator puts out;
// group B through personal invites, the second made by a B tester the
// operator let invite. Each group then does what a group does: posts,
// replies in a thread, whispers, posts to a net, uploads a file, starts a
// session and a run, asks for an intervention and an approval, sends a
// problem report, and (with LiveKit) talks in voice and shares a screen.
//
// Everything A makes is registered as a marker (markers.go) as it is made.

// member is a person or agent with a credential.
type member struct {
	name   string
	id     int64
	token  string
	credID int64
	api    api
	// mayInvite is whether an operator let them invite.
	mayInvite bool
	// password is whether they chose one: everyone who joined through an
	// invite did (v2, #713); a person the operator made by hand did not.
	password bool
	// addr is the client address they come from (api.addr): "" for the
	// groups, which all come from loopback; each person of the plaza has
	// one of their own (guests.go).
	addr string
	// guest is the visit they joined with, for a guest.
	guest *schema.GuestV1
}

// group is one group's people and what they made.
type group struct {
	label    string
	channels []string
	chanIDs  []int64
	people   []*member // humans, in the order they joined
	agent    *member
	// hidden is a principal the operator made and hid (A only): the
	// community directory lists it to no one, and a manager's add of it is
	// the one 404 of a principal never issued. Its id and the name it was
	// given when hidden are not markers: the directory makes names public.
	hidden *member

	openInviteID int64  // A's open link
	openCode     string // its code
	invites      []int64

	msgIDs    []int64
	root      int64 // a thread root
	whisper   int64
	netName   string
	netID     int64
	fileName  string
	fileID    int64
	sessionID int64
	runID     int64
	interID   int64
	approval  int64
	// agentSession is the group agent's own session (B only).
	agentSession int64
	reportID     int64
	// signInLink is an operator's sign-in link for one of the group's people,
	// and signInCode its code, which B presents to the password routes as a
	// proof and a reset (issue #708): neither may take it.
	signInLink int64
	signInCode string
}

func (g *group) main() string { return g.channels[0] }

// world is the server and the two groups.
type world struct {
	h    *harness
	d    *conchdProc
	op   api
	tag  string // A's run tag: in every string A writes
	btag string // B's, different, so that B's text holds no piece of A's
	// ntag is in the name of every person and agent of A's (aPerson): the
	// private register of markers.go stops at A's private channels and
	// what is written in them, and A's principals are known to the whole
	// community (design section 8), so their names hold no piece of A's text.
	ntag  string
	marks *markers
	a, b  *group
	// spares are second credentials of the members the matrix runs as, by
	// principal id, spent by the row of DELETE /v1/credentials/current.
	spares map[int64]api
	// prober is the member of B the matrix is running as.
	prober *member
	// onBehalf records how each impersonation row was answered, for the
	// run's output.
	onBehalf []string
	// lk is the LiveKit of the run; nil when the media half is skipped.
	lk *liveKit

	// The plaza and its guests (guests.go), and the plaza's private
	// markers, nil until it is made; ptag is in every private string of
	// it, qtag in every other string of it.
	p          *plaza
	priv       *markers
	ptag, qtag string
	// addrs counts the client addresses handed out (newAddr).
	addrs int
}

// liveKit is what the media half needs: the server, its admin API and lk.
type liveKit struct {
	srv   *lkit.Server
	adm   *lkit.Admin
	lkBin string
	ogg   string
}

func newWorld(h *harness, d *conchdProc, lk *liveKit) *world {
	w := &world{h: h, d: d, op: d.op, tag: newTag(), marks: newMarkers(), lk: lk}
	w.btag = newTag(w.tag)
	w.ptag = newTag(w.tag, w.btag)
	w.qtag = newTag(w.tag, w.btag, w.ptag)
	w.ntag = newTag(w.tag, w.btag, w.ptag, w.qtag)
	// No password is ever in conchd's output: the log scan at the end of the
	// run looks for every one the run sends.
	for _, pw := range passwords {
		h.secret("a password", pw)
	}
	return w
}

// newTag is a run tag, six hex characters, that none of taken is.
func newTag(taken ...string) string {
	for {
		if t := randHex(3); !slices.Contains(taken, t) {
			return t
		}
	}
}

// aName is a string A writes: its tag and hex, never a word, so that any six
// characters of it are A's alone (markers.go). s says what it is for, and
// only picks the hex.
func (w *world) aName(s string) string {
	sum := sha256.Sum256([]byte(w.tag + "/" + s))
	return "alpha-" + w.tag + "-" + hex.EncodeToString(sum[:4])
}

// aPerson is the name of a person or agent of A's: not a marker (the
// directory lists A's people to all of B), so it shares no run of six
// characters with anything aName makes.
func (w *world) aPerson(s string) string { return "folk-" + w.ntag + "-" + s }

// isA reports whether name is one of A's people.
func (w *world) isA(name string) bool { return strings.HasPrefix(name, "folk-"+w.ntag+"-") }

// bName is a string B writes; B's text is not scanned, so it may be plain.
func (w *world) bName(s string) string { return "bravo-" + w.btag + "-" + s }

// a member's REST client.
func (w *world) as(m *member) api {
	return api{h: w.h, base: w.d.baseURL, token: m.token, who: m.name, addr: m.addr}
}

// buildGroups makes both groups' channels and members. Nothing is posted yet:
// B's sockets are opened before A is active.
func (w *world) buildGroups() error {
	w.a = &group{label: "A"}
	w.b = &group{label: "B"}
	for _, c := range []struct {
		g    *group
		name string
	}{
		{w.a, w.aName("camp")}, {w.a, w.aName("den")},
		{w.b, w.bName("yard")}, {w.b, w.bName("loft")},
	} {
		var resp schema.CreateChannelResponse
		if err := w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: c.name}, &resp); err != nil {
			return err
		}
		c.g.channels = append(c.g.channels, c.name)
		c.g.chanIDs = append(c.g.chanIDs, resp.Channel.ID)
	}
	for i, name := range w.a.channels {
		w.marks.str("A's channel name", name)
		w.marks.id("channel", "A's channel "+name, w.a.chanIDs[i])
	}

	// A: one open link, three joiners.
	open, err := w.invite(w.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: w.a.channels, Voice: true})
	if err != nil {
		return err
	}
	w.a.openInviteID, w.a.openCode = open.Invite.ID, open.Code
	w.a.invites = append(w.a.invites, open.Invite.ID)
	w.marks.secret("A's open link", open.Code)
	w.marks.id("invite", "A's open link", open.Invite.ID)
	for _, n := range []string{"ann", "abe", "amy"} {
		m, err := w.redeem(open.Code, w.aPerson(n))
		if err != nil {
			return err
		}
		w.a.people = append(w.a.people, m)
	}

	// B: the operator invites bea, who may invite; bea invites bob.
	may := true
	first, err := w.invite(w.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: w.b.channels, Voice: true, MayInvite: &may})
	if err != nil {
		return err
	}
	bea, err := w.redeem(first.Code, w.bName("bea"))
	if bea != nil {
		bea.mayInvite = true
	}
	if err != nil {
		return err
	}
	second, err := w.invite(bea.api, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: w.b.channels, Voice: true})
	if err != nil {
		return err
	}
	bob, err := w.redeem(second.Code, w.bName("bob"))
	if err != nil {
		return err
	}
	w.b.people = []*member{bea, bob}
	w.b.invites = []int64{first.Invite.ID, second.Invite.ID}

	// ola: a B member the operator made by hand, in B's first channel only.
	ola, err := w.newPerson(w.bName("ola"), w.b.main())
	if err != nil {
		return err
	}
	w.b.people = append(w.b.people, ola)

	// Bea manages B's first channel, for the manager isolation rows.
	manages := true
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", w.b.main(), bea.id), schema.PutChannelMemberRequestV1{Manages: &manages}, nil); err != nil {
		return err
	}

	// A spare credential for each member the matrix runs as, for the
	// sign-out row.
	w.spares = map[int64]api{}
	for _, m := range []*member{bea, ola} {
		var spare schema.CreateCredentialResponseV1
		if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", m.id), schema.CreateCredentialRequestV1{Label: "spare"}, &spare); err != nil {
			return err
		}
		w.h.secret("a B credential", spare.Token)
		w.spares[m.id] = api{h: w.h, base: w.d.baseURL, token: spare.Token, who: m.name + " (spare credential)"}
	}

	// B's agent, with a grant in B's first channel, for the rows an agent
	// reaches differently and for MCP.
	if w.b.agent, err = w.newAgent(w.bName("bot"), w.b); err != nil {
		return err
	}
	if err := w.openAgentSession(); err != nil {
		return err
	}

	// A's agent, made by the operator, with a grant in A's first channel.
	agent, err := w.newAgent(w.aPerson("bot"), w.a)
	if err != nil {
		return err
	}
	w.a.agent = agent

	// A principal of A's that the operator hid: it is in no directory.
	var gone schema.CreatePrincipalResponse
	if err := w.op.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: w.aPerson("gone")}, &gone); err != nil {
		return err
	}
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/hide", gone.Principal.ID), nil, nil); err != nil {
		return err
	}
	w.a.hidden = &member{name: schema.HiddenPrincipalNamePrefix + strconv.FormatInt(gone.Principal.ID, 10), id: gone.Principal.ID}

	// ann may invite too, and makes a personal invite to A's second channel
	// that nobody uses: an invite of A's made by a tester.
	ann := w.a.people[0]
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/invite-permission", ann.id), schema.InvitePermissionRequestV1{MayInvite: true}, nil); err != nil {
		return err
	}
	pi, err := w.invite(ann.api, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{w.a.channels[1]}})
	if err != nil {
		return err
	}
	w.a.invites = append(w.a.invites, pi.Invite.ID)
	w.marks.secret("A's personal invite code", pi.Code)
	w.marks.id("invite", "ann's personal invite", pi.Invite.ID)
	return nil
}

// invite makes an invite as a and registers its code as a secret.
func (w *world) invite(a api, req schema.CreateInviteRequestV1) (schema.CreateInviteResponseV1, error) {
	var resp schema.CreateInviteResponseV1
	if err := a.call(http.MethodPost, "/v1/invites", req, &resp); err != nil {
		return resp, err
	}
	w.h.secret("an invite code", resp.Code)
	return resp, nil
}

// joinPassword is the password every joiner of this world chooses: it
// contains none of their names.
const joinPassword = "a passphrase long enough to pass"

// redeem joins through an invite, with no credential, as a person does:
// POST /v2/invites/redeem, with a name and a password (v1 makes no account
// since #713). conchd listens on loopback, so plain HTTP is allowed.
func (w *world) redeem(code, name string) (*member, error) {
	nobody := api{h: w.h, base: w.d.baseURL, who: name + " (joining)"}
	var resp schema.RedeemInviteResponseV1
	if err := nobody.call(http.MethodPost, "/v2/invites/redeem", schema.RedeemInviteRequestV2{Code: code, Name: name, Password: joinPassword}, &resp); err != nil {
		return nil, err
	}
	m := &member{name: name, id: resp.Principal.ID, token: resp.Token, credID: resp.Credential.ID, password: true}
	m.api = w.as(m)
	w.h.secret(name+"'s credential", resp.Token)
	w.registerMember(m)
	return m, nil
}

// registerMember makes an A member's credential id and token markers. B's
// are not markers, and neither are A's names and ids: the community's member
// list shows them to every member (design section 8, issue #983).
func (w *world) registerMember(m *member) {
	if !w.isA(m.name) {
		return
	}
	w.marks.secret("an A credential", m.token)
	w.marks.id("credential", m.name+"'s credential", m.credID)
}

// newPerson makes a human by the operator's hand, with a credential, a
// member of channel.
func (w *world) newPerson(name, channel string) (*member, error) {
	var created schema.CreatePrincipalResponse
	if err := w.op.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}, &created); err != nil {
		return nil, err
	}
	var cred schema.CreateCredentialResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", created.Principal.ID), schema.CreateCredentialRequestV1{Label: name}, &cred); err != nil {
		return nil, err
	}
	m := &member{name: name, id: created.Principal.ID, token: cred.Token, credID: cred.Credential.ID}
	m.api = w.as(m)
	w.h.secret(name+"'s credential", cred.Token)
	w.registerMember(m)
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", channel, m.id), nil, nil); err != nil {
		return nil, err
	}
	return m, nil
}

// newAgent makes an agent in g's first channel, holding every capability and
// read and post there, so that what it is refused is for being an agent.
func (w *world) newAgent(name string, g *group) (*member, error) {
	var created schema.CreatePrincipalResponse
	if err := w.op.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalAgent, Name: name}, &created); err != nil {
		return nil, err
	}
	var cred schema.CreateCredentialResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", created.Principal.ID), schema.CreateCredentialRequestV1{Label: name}, &cred); err != nil {
		return nil, err
	}
	m := &member{name: name, id: created.Principal.ID, token: cred.Token, credID: cred.Credential.ID}
	m.api = w.as(m)
	w.h.secret(name+"'s credential", cred.Token)
	w.registerMember(m)
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", g.main(), m.id), nil, nil); err != nil {
		return nil, err
	}
	req := schema.PutAgentManifestRequestV1{DisplayName: name, Tier: schema.AgentTierA, Capabilities: schema.Capabilities(),
		Channels: []schema.ChannelGrant{{ChannelID: g.chanIDs[0], Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}}}}
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", m.id), req, nil); err != nil {
		return nil, err
	}
	return m, nil
}

// post posts a v2 message as m and registers it when it is A's.
func (w *world) post(g *group, m *member, channel string, req schema.PostMessageRequestV2) (int64, error) {
	var resp schema.PostMessageResponseV2
	if err := m.api.call(http.MethodPost, "/v2/channels/"+url.PathEscape(channel)+"/messages", req, &resp); err != nil {
		return 0, err
	}
	if g == w.a {
		w.marks.str("A's message", req.Body)
		w.marks.id("message", "A's message", resp.Message.ID)
		g.msgIDs = append(g.msgIDs, resp.Message.ID)
	}
	return resp.Message.ID, nil
}

// activityA has group A do everything a group does, while B's sockets are
// open. Voice and the share are the media half's (media.go).
func (w *world) activityA() error {
	a := w.a
	ann, abe, amy := a.people[0], a.people[1], a.people[2]
	camp, den := a.channels[0], a.channels[1]
	var err error

	// Messages, by each version of the API, a thread and a whisper.
	if a.root, err = w.post(a, ann, camp, schema.PostMessageRequestV2{Body: w.aName("word-root")}); err != nil {
		return err
	}
	if _, err = w.post(a, abe, camp, schema.PostMessageRequestV2{Body: w.aName("word-reply"), ThreadRootID: a.root}); err != nil {
		return err
	}
	if a.whisper, err = w.post(a, ann, camp, schema.PostMessageRequestV2{Body: w.aName("word-whisper"),
		Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{amy.id}}}); err != nil {
		return err
	}
	if _, err = w.post(a, abe, den, schema.PostMessageRequestV2{Body: w.aName("word-den")}); err != nil {
		return err
	}
	var v1 schema.PostMessageResponseV1
	body := w.aName("word-v1")
	w.marks.str("A's message", body)
	if err := amy.api.call(http.MethodPost, "/v1/channels/"+camp+"/messages", schema.PostMessageRequestV1{AuthorID: amy.id, Body: body}, &v1); err != nil {
		return err
	}
	w.marks.id("message", "A's v1 message", v1.Message.ID)
	var v0 schema.PostMessageResponse
	body = w.aName("word-v0")
	w.marks.str("A's message", body)
	if err := amy.api.call(http.MethodPost, "/v0/channels/"+camp+"/messages", schema.PostMessageRequest{AuthorID: amy.id, Body: body}, &v0); err != nil {
		return err
	}
	w.marks.id("message", "A's v0 message", v0.Message.ID)

	// A net, and a message to it.
	a.netName = w.aName("net")
	w.marks.str("A's net", a.netName)
	var net schema.CreateNetResponseV1
	if err := w.op.call(http.MethodPost, "/v1/channels/"+camp+"/nets", schema.CreateNetRequestV1{Name: a.netName}, &net); err != nil {
		return err
	}
	a.netID = net.Net.ID
	w.marks.id("net", "A's net", net.Net.ID)
	for _, m := range []*member{ann, abe} {
		if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/nets/%s/members/%d", camp, a.netName, m.id), schema.PutNetMemberRequestV1{Role: schema.NetRoleMember}, nil); err != nil {
			return err
		}
	}
	if _, err = w.post(a, ann, camp, schema.PostMessageRequestV2{Body: w.aName("word-net"),
		Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: net.Net.ID}}); err != nil {
		return err
	}

	// A file, and a message that carries it.
	a.fileName = w.aName("plans") + ".txt"
	contents := w.aName("file-contents")
	w.marks.str("A's file name", a.fileName)
	w.marks.str("A's file contents", contents)
	status, data, err := ann.api.send(http.MethodPost, "/v2/channels/"+camp+"/files?"+url.Values{"filename": {a.fileName}}.Encode(), "text/plain", []byte(contents))
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("ann uploads a file: status %d: %s", status, w.h.redact(string(data)))
	}
	var file schema.FileV1
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}
	a.fileID = file.ID
	w.marks.id("file", "A's file", file.ID)
	att, err := json.Marshal(schema.FileAttachmentV1{FileIDs: []int64{file.ID}})
	if err != nil {
		return err
	}
	if _, err = w.post(a, ann, camp, schema.PostMessageRequestV2{Body: w.aName("word-attached"),
		Payload: &schema.Payload{Schema: schema.FileAttachmentV1Name, Data: att}}); err != nil {
		return err
	}

	// Read state.
	if err := abe.api.call(http.MethodPut, "/v1/channels/"+camp+"/read-state", schema.MarkReadRequestV1{LastReadID: a.root}, nil); err != nil {
		return err
	}

	// The agent's session and run, and an intervention a person asks for.
	bot := a.agent
	client := w.aName("client")
	w.marks.str("A's session client", client)
	var sess schema.SessionV1
	if err := bot.api.call(http.MethodPost, "/v1/sessions", schema.CreateSessionRequestV1{Client: client, Runtime: "fixture",
		Interventions: []schema.InterventionKind{schema.InterventionKindMessage}}, &sess); err != nil {
		return err
	}
	a.sessionID = sess.ID
	w.marks.id("session", "A's agent session", sess.ID)
	task := w.aName("task")
	w.marks.str("A's run task", task)
	var run schema.RunV1
	if err := bot.api.call(http.MethodPost, fmt.Sprintf("/v1/sessions/%d/runs", sess.ID), schema.CreateRunRequestV1{Channel: camp, Task: task}, &run); err != nil {
		return err
	}
	a.runID = run.ID
	w.marks.id("run", "A's run", run.ID)
	summary := w.aName("activity")
	w.marks.str("A's run activity", summary)
	if err := bot.api.call(http.MethodPost, fmt.Sprintf("/v1/runs/%d/activity", run.ID), schema.AppendActivityRequestV1{Kind: schema.ActivityKindRunStarted, Summary: summary}, nil); err != nil {
		return err
	}
	nudge := w.aName("nudge")
	w.marks.str("A's intervention", nudge)
	var inter schema.InterventionV1
	if err := ann.api.call(http.MethodPost, fmt.Sprintf("/v1/runs/%d/interventions", run.ID), schema.CreateInterventionRequestV1{Kind: schema.InterventionKindMessage, Body: nudge}, &inter); err != nil {
		return err
	}
	a.interID = inter.ID
	w.marks.id("intervention", "A's intervention", inter.ID)

	// An approval in A's channel.
	title := w.aName("approval")
	w.marks.str("A's approval", title)
	var appr schema.CreateApprovalResponseV1
	if err := ann.api.call(http.MethodPost, "/v1/approvals", schema.CreateApprovalRequestV1{
		RequesterID: ann.id, ChannelID: a.chanIDs[0], Title: title, Body: w.aName("approval-body"),
		Options:  []schema.Option{{ID: "yes", Label: "Yes", Kind: schema.OptionKindApprove}, {ID: "no", Label: "No", Kind: schema.OptionKindReject}},
		Deadline: schema.NewTimestamp(time.Now().Add(time.Hour)),
	}, &appr); err != nil {
		return err
	}
	w.marks.str("A's approval body", w.aName("approval-body"))
	a.approval = appr.Approval.ID
	w.marks.id("approval", "A's approval", appr.Approval.ID)

	// B's own approval, which the impersonation rows try to decide as A.
	var bAppr schema.CreateApprovalResponseV1
	bea := w.b.people[0]
	if err := bea.api.call(http.MethodPost, "/v1/approvals", schema.CreateApprovalRequestV1{
		RequesterID: bea.id, ChannelID: w.b.chanIDs[0], Title: w.bName("approval"), Body: w.bName("approval-body"),
		Options:  []schema.Option{{ID: "yes", Label: "Yes", Kind: schema.OptionKindApprove}, {ID: "no", Label: "No", Kind: schema.OptionKindReject}},
		Deadline: schema.NewTimestamp(time.Now().Add(time.Hour)),
	}, &bAppr); err != nil {
		return err
	}
	w.b.approval = bAppr.Approval.ID

	// An operator's sign-in link for ann (issue #493), never used.
	var link schema.CreateSignInLinkResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/sign-in-links", ann.id), schema.CreateSignInLinkRequestV1{}, &link); err != nil {
		return err
	}
	w.h.secret("a sign-in link code", link.Code)
	w.marks.secret("A's sign-in link code", link.Code)
	a.signInLink, a.signInCode = link.SignInLink.ID, link.Code
	w.marks.id("sign_in_link", "ann's sign-in link", link.SignInLink.ID)

	// A problem report.
	report, err := w.report(ann, w.aName("report"))
	if err != nil {
		return err
	}
	a.reportID = report
	w.marks.id("report", "A's problem report", report)
	return nil
}

// report sends a problem report as m whose text carries word.
func (w *world) report(m *member, word string) (int64, error) {
	if w.isA(m.name) {
		w.marks.str("A's report", word)
	}
	var resp schema.CreateProblemReportResponseV1
	err := m.api.call(http.MethodPost, "/v1/reports", schema.CreateProblemReportRequestV1{
		Did: word + " did", Expected: word + " expected", Happened: word + " happened", Often: schema.ProblemReportOftenOnce,
		Client:      schema.ProblemReportClientV1{Name: "isolation-check", Version: "0.0.0", Platform: "linux"},
		Diagnostics: []schema.DiagnosticEventV1{},
	}, &resp)
	return resp.ID, err
}

// controlB is B's own activity, so that each of B's sockets has something of
// B's to carry and what it did not carry means something: in each of B's
// channels a message, a new session of bea's (a roster change) and, with
// LiveKit, someone of B's joining the channel's room (bea the first time,
// bob the second).
func (w *world) controlB(round int) (controls, error) {
	bea, bob := w.b.people[0], w.b.people[1]
	c := controls{message: w.bName(fmt.Sprintf("control-%d", round)), session: w.bName(fmt.Sprintf("web-%d", round)), voice: map[string]string{}}
	for _, ch := range w.b.channels {
		if _, err := w.post(w.b, bea, ch, schema.PostMessageRequestV2{Body: c.message}); err != nil {
			return c, err
		}
	}
	var sess schema.SessionV1
	if err := bea.api.call(http.MethodPost, "/v1/sessions", schema.CreateSessionRequestV1{Client: c.session}, &sess); err != nil {
		return c, err
	}
	joiner := bea
	if round > 1 {
		joiner = bob
	}
	switch {
	case w.lk != nil:
		c.occupancy = "counts"
	case round == 1:
		c.occupancy = schema.VoiceOccupancySchemaV1
	}
	for _, ch := range w.b.channels {
		switch {
		case w.lk != nil:
			s, err := w.session(joiner, ch)
			if err != nil {
				return c, err
			}
			if _, err := w.joinSignal(s.Rooms[0]); err != nil {
				return c, fmt.Errorf("%s joins %s's room: %w", joiner.name, ch, err)
			}
			c.voice[ch] = fmt.Sprintf(`"principal_id":%d`, joiner.id)
		case round == 1:
			// Without LiveKit a voice socket carries one document, which
			// says voice is not configured.
			c.voice[ch] = schema.VoicePresenceSchemaV1
		}
	}
	if _, err := w.report(bea, w.bName("report")); err != nil {
		return c, err
	}
	return c, nil
}

// itoa is for paths.
func itoa(id int64) string { return strconv.FormatInt(id, 10) }
