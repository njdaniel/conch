package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/pkg/schema"
)

// Revocation, timed (private-alpha.md §1, exit step 7). A tester holds every
// socket variant, a LiveKit participant and the room's share; the operator
//
//  1. revokes a second credential of the tester's, which holds every socket
//     variant too, and nothing else (issue #519),
//  2. disables the tester,
//  3. disables everyone who joined through A's open link, and
//  4. closes the link,
//
// and each step is timed from the moment the operator's request is sent to
// the moment each socket is seen closed and LiveKit no longer lists the
// participant. The bounds are the issue's: two seconds for a socket, and
// for LiveKit one pass of conchd's voice poller plus a room rotation. That
// bound is read from the server's own source (voice_presence.go), so it
// follows the code rather than a guess written down here. After each step
// the revoked must stay out (staysOut).

// socketBound is how soon a revoked person's sockets must be closed.
const socketBound = 2 * time.Second

// voiceBounds are the two constants of internal/server/voice_presence.go the
// LiveKit bound is made of: the longest gap between two poller passes, and
// the time conchd gives LiveKit to rotate a room or remove a participant.
type voiceBounds struct {
	pass, evict time.Duration
}

func (b voiceBounds) total() time.Duration { return b.pass + b.evict }

func (b voiceBounds) String() string {
	return fmt.Sprintf("%s (one poller pass, voicePassGapMax %s, plus a room rotation, voiceEvictTimeout %s)", b.total(), b.pass, b.evict)
}

// readVoiceBounds reads voicePassGapMax and voiceEvictTimeout out of the
// server's source.
func readVoiceBounds(repo string) (voiceBounds, error) {
	path := filepath.Join(repo, "internal", "server", "voice_presence.go")
	src, err := os.ReadFile(path) // #nosec G304 -- the repository this run is in
	if err != nil {
		return voiceBounds{}, err
	}
	consts, err := durationConsts(string(src))
	if err != nil {
		return voiceBounds{}, fmt.Errorf("%s: %w", path, err)
	}
	b := voiceBounds{pass: consts["voicePassGapMax"], evict: consts["voiceEvictTimeout"]}
	if b.pass <= 0 || b.evict <= 0 {
		return voiceBounds{}, fmt.Errorf("%s: cannot read voicePassGapMax and voiceEvictTimeout as durations (read %v)", path, consts)
	}
	return b, nil
}

// durationConsts evaluates the constants of a Go file written as
// <integer> * time.<Unit>, or time.<Unit> alone.
func durationConsts(src string) (map[string]time.Duration, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil {
		return nil, err
	}
	units := map[string]time.Duration{"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond,
		"Millisecond": time.Millisecond, "Second": time.Second, "Minute": time.Minute, "Hour": time.Hour}
	out := map[string]time.Duration{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, v := range vs.Values {
				if sel, ok := v.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" {
						if unit, known := units[sel.Sel.Name]; known {
							out[vs.Names[i].Name] = unit
						}
					}
					continue
				}
				be, ok := v.(*ast.BinaryExpr)
				if !ok || be.Op != token.MUL {
					continue
				}
				lit, ok := be.X.(*ast.BasicLit)
				sel, ok2 := be.Y.(*ast.SelectorExpr)
				if !ok || !ok2 || lit.Kind != token.INT {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				unit, known := units[sel.Sel.Name]
				if !ok || pkg.Name != "time" || !known {
					continue
				}
				n, err := strconv.ParseInt(lit.Value, 0, 64)
				if err != nil {
					continue
				}
				out[vs.Names[i].Name] = time.Duration(n) * unit
			}
		}
	}
	return out, nil
}

// holder is a person who holds sockets and, with LiveKit, a participant, and
// keeps what would let them back in: a socket ticket not yet spent and, with
// LiveKit, a join grant for their room not yet used.
type holder struct {
	m       *member
	sockets []*feed
	room    string // their voice room; "" without LiveKit
	ticket  string
	grant   string
}

// hold has m open every socket variant on channel and keep a ticket and
// (with LiveKit) a fresh grant to their room, which is room when they are
// already in one.
func (w *world) hold(m *member, channel, room string) (holder, error) {
	h := holder{m: m, room: room}
	var err error
	if h.sockets, err = openAll(m, channel); err != nil {
		return h, err
	}
	occ, err := openOccupancy(m)
	if err != nil {
		return h, err
	}
	h.sockets = append(h.sockets, occ...)
	if h.ticket, err = m.api.ticket(); err != nil {
		return h, err
	}
	if w.lk != nil {
		sess, err := w.session(m, channel)
		if err != nil {
			return h, err
		}
		if room != "" && sess.Rooms[0].Room != room {
			return h, fmt.Errorf("%s's fresh grant names another room than the one they are in", m.name)
		}
		h.room, h.grant = sess.Rooms[0].Room, sess.Rooms[0].Token
	}
	return h, nil
}

// staysOut checks that what each holder kept no longer lets them in: the
// unspent ticket opens no socket (401), and the unused grant does not admit
// them to LiveKit. The server rotates every room a revoked person was issued
// a session for (voicePrincipalLostAccess: voiceEnforceHolders), so the room
// the grant names no longer exists and LiveKit answers 404; with no rotation
// the poller would have to remove them again, which this would not show.
// Then, after one more poller pass, LiveKit lists them in no room at all.
func (w *world) staysOut(holders []holder, bounds voiceBounds) error {
	for _, h := range holders {
		f, r, err := dialWithTicket(h.m.api, h.m.name+"'s kept ticket", "/v2/ws", url.Values{"channel": {w.a.main()}}, h.ticket)
		if err != nil {
			return err
		}
		if f != nil {
			f.close()
			return fmt.Errorf("%s's ticket, kept from before the revocation, opened a socket", h.m.name)
		}
		if r.status != http.StatusUnauthorized {
			return fmt.Errorf("%s's kept ticket: status %d %s, want 401", h.m.name, r.status, strings.TrimSpace(string(r.body)))
		}
		if w.lk == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(stopCtx, 15*time.Second)
		c, status, body, err := lkit.DialSignal(ctx, w.lk.srv.WSURL, h.grant)
		cancel()
		if err == nil {
			c.Close()
			return fmt.Errorf("LiveKit admitted %s on a grant kept from before the revocation", h.m.name)
		}
		if status != http.StatusNotFound || !strings.Contains(body, "room does not exist") {
			return fmt.Errorf("%s's kept grant: LiveKit answered %d %q, want 404: the room was not rotated", h.m.name, status, w.h.redact(body))
		}
	}
	if w.lk == nil {
		return nil
	}
	pass := time.NewTimer(bounds.pass)
	select {
	case <-pass.C:
	case <-stopCtx.Done():
		return errInterrupted
	}
	for _, h := range holders {
		if where, err := w.lk.anywhere(h.m.id); err != nil {
			return err
		} else if where != 0 {
			return fmt.Errorf("one poller pass later LiveKit lists %s in %d room(s)", h.m.name, where)
		}
	}
	return nil
}

// timing is what one revocation step measured.
type timing struct {
	what    string
	sockets []time.Duration
	livekit []time.Duration
	// bound is the sockets' bound when it is not socketBound: a timed
	// expiry's adds the sweeper's fallback, a host command's is the
	// sockets' re-check interval (guests' phase).
	bound time.Duration
}

func (t timing) String() string {
	bound := socketBound
	if t.bound > 0 {
		bound = t.bound
	}
	s := fmt.Sprintf("%s: %d sockets closed in %s (slowest %s; bound %s)", t.what, len(t.sockets), durations(t.sockets), slowest(t.sockets), bound)
	if len(t.livekit) > 0 {
		s += fmt.Sprintf("; LiveKit stopped listing %d participant(s) within %s", len(t.livekit), durations(t.livekit))
	}
	return s
}

func durations(ds []time.Duration) string {
	var parts []string
	for _, d := range ds {
		parts = append(parts, d.Round(100*time.Microsecond).String())
	}
	return strings.Join(parts, ", ")
}

func slowest(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		m = max(m, d)
	}
	return m.Round(100 * time.Microsecond)
}

// measure sends the operator's request and times how long each holder's
// sockets and participant take to go.
func (w *world) measure(what string, holders []holder, bounds voiceBounds, act func() error) (timing, error) {
	t := timing{what: what}
	// LiveKit is watched from before the request, every 2 ms, each
	// participant on its own, so the time measured is when LiveKit first
	// stopped listing them, not when this program got round to asking.
	type seen struct {
		after time.Duration
		err   error
	}
	var watches []chan seen
	start := time.Now()
	if w.lk != nil {
		for _, h := range holders {
			if h.room == "" {
				continue
			}
			found := make(chan seen, 1)
			watches = append(watches, found)
			go func(h holder) {
				deadline := start.Add(60 * time.Second)
				for time.Now().Before(deadline) && stopCtx.Err() == nil {
					if gone, _ := w.lk.gone(h.room, h.m.id); gone {
						found <- seen{after: time.Since(start)}
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
				_, saw := w.lk.gone(h.room, h.m.id)
				found <- seen{err: fmt.Errorf("LiveKit still lists %s after 60 s: %s", h.m.name, saw)}
			}(h)
		}
	}
	if err := act(); err != nil {
		return t, err
	}
	// Generous waits, so that a slow close is measured, not merely failed.
	for _, h := range holders {
		for _, f := range h.sockets {
			if err := waitFor(f.what+" to close", 30*time.Second, func() (bool, string) {
				ended, _ := f.ended()
				return ended, "still open"
			}); err != nil {
				return t, err
			}
			_, at := f.ended()
			t.sockets = append(t.sockets, at.Sub(start))
		}
	}
	for _, found := range watches {
		s := <-found
		if s.err != nil {
			return t, s.err
		}
		t.livekit = append(t.livekit, s.after)
	}
	var over []string
	for i, d := range t.sockets {
		if d > socketBound {
			over = append(over, fmt.Sprintf("socket %d closed after %s, bound %s", i, d.Round(time.Millisecond), socketBound))
		}
	}
	for i, d := range t.livekit {
		if d > bounds.total() {
			over = append(over, fmt.Sprintf("LiveKit participant %d gone after %s, bound %s", i, d.Round(time.Millisecond), bounds.total()))
		}
	}
	if len(over) > 0 {
		return t, fmt.Errorf("%s: %s", what, strings.Join(over, "; "))
	}
	return t, nil
}

// secondCredential has the operator issue m a second credential, and returns
// m signed in with it. Its token and id are markers of A, as m's first are.
func (w *world) secondCredential(m *member) (*member, error) {
	var cred schema.CreateCredentialResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", m.id), schema.CreateCredentialRequestV1{Label: "second sign-in"}, &cred); err != nil {
		return nil, err
	}
	second := &member{name: m.name + " (second credential)", id: m.id, token: cred.Token, credID: cred.Credential.ID}
	second.api = w.as(second)
	w.h.secret(second.name, cred.Token)
	w.marks.secret("an A credential", cred.Token)
	w.marks.id("credential", second.name, second.credID)
	return second, nil
}

// revoke runs the four steps and returns what they measured.
func (w *world) revoke(v *voiceA, bounds voiceBounds) ([]timing, error) {
	ann, abe, amy := w.a.people[0], w.a.people[1], w.a.people[2]
	camp := w.a.main()
	var out []timing

	// The tester: ann, who holds every socket variant and, with LiveKit, her
	// lk participant and the share.
	room := ""
	if v != nil {
		room = v.room
		if ok, p, saw := w.lk.has(v.room, ann.id); !ok {
			return nil, fmt.Errorf("before the revocation ann is not in A's room: %s", saw)
		} else if screen, _ := lkit.ShareTracks(p); !screen {
			return nil, errors.New("before the revocation ann is not sharing")
		}
	}
	tester, err := w.hold(ann, camp, room)
	if err != nil {
		return nil, err
	}

	// 1. One of her credentials: ann signs in a second time and holds every
	// socket variant with that credential too, and the operator revokes it
	// alone. It holds no voice session, so the room is not rotated and her
	// participant stays for step 2.
	second, err := w.secondCredential(ann)
	if err != nil {
		return nil, err
	}
	feeds, err := openAll(second, camp)
	if err != nil {
		return nil, err
	}
	occ, err := openOccupancy(second)
	if err != nil {
		return nil, err
	}
	feeds = append(feeds, occ...)
	t, err := w.measure("revoke one of the tester's credentials (ann's second)", []holder{{m: second, sockets: feeds}}, bounds, func() error {
		return w.op.call(http.MethodDelete, fmt.Sprintf("/v1/credentials/%d", second.credID), nil, nil)
	})
	if err != nil {
		return nil, err
	}
	if err := closedRevoked(feeds); err != nil {
		return nil, err
	}
	out = append(out, t)
	// The revoke closed its sockets before it answered, so one of her first
	// credential's closed by mistake would have closed with them. Give that a
	// second to show, then every one of them must still be open.
	settle := time.NewTimer(time.Second)
	select {
	case <-settle.C:
	case <-stopCtx.Done():
		return nil, errInterrupted
	}
	for _, f := range tester.sockets {
		if ended, _ := f.ended(); ended {
			return nil, fmt.Errorf("%s closed when only her second credential was revoked", f.what)
		}
	}
	for _, c := range []struct {
		m    *member
		want int
	}{{second, http.StatusUnauthorized}, {ann, http.StatusOK}} {
		if status, _, err := c.m.api.do(http.MethodGet, "/v1/whoami", nil); err != nil {
			return nil, err
		} else if status != c.want {
			return nil, fmt.Errorf("%s after the second credential was revoked: whoami status %d, want %d", c.m.name, status, c.want)
		}
	}
	w.h.say("ok   revoked 1/4: %s; the %d sockets of her other credential stay open", t, len(tester.sockets))

	// 2. The tester herself.
	t, err = w.measure("disable the tester (ann)", []holder{tester}, bounds, func() error {
		return w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/disable", ann.id), nil, nil)
	})
	if err != nil {
		return nil, err
	}
	if err := closedRevoked(tester.sockets); err != nil {
		return nil, err
	}
	out = append(out, t)
	if status, _, err := ann.api.do(http.MethodGet, "/v1/whoami", nil); err != nil {
		return nil, err
	} else if status != http.StatusUnauthorized {
		return nil, fmt.Errorf("ann's credential after she was disabled: status %d, want 401", status)
	}
	if v != nil {
		// The share went with her: nobody is sharing in A's channel, as an
		// operator reads it.
		var doc schema.VoicePresenceV1
		if err := w.op.call(http.MethodGet, "/v1/channels/"+camp+"/voice", nil, &doc); err != nil {
			return nil, err
		}
		for _, r := range doc.Rooms {
			for _, p := range r.Participants {
				if p.Sharing {
					return nil, fmt.Errorf("presence still shows principal %d sharing after ann was disabled", p.PrincipalID)
				}
			}
		}
	}
	if err := w.staysOut([]holder{tester}, bounds); err != nil {
		return nil, fmt.Errorf("after ann was disabled: %w", err)
	}
	w.h.say("ok   revoked 2/4: %s; she stays out", t)

	// 3. Everyone from A's open link: abe and amy, each holding every socket
	// variant and a participant in A's room (the new one: disabling ann
	// rotated it).
	var joiners []holder
	for _, m := range []*member{abe, amy} {
		hd, err := w.hold(m, camp, "")
		if err != nil {
			return nil, err
		}
		if v != nil {
			sess, err := w.session(m, camp)
			if err != nil {
				return nil, err
			}
			if _, err := w.joinSignal(sess.Rooms[0]); err != nil {
				return nil, fmt.Errorf("%s joins A's room: %w", m.name, err)
			}
			if err := waitFor("LiveKit to list "+m.name, 30*time.Second, func() (bool, string) {
				ok, _, saw := w.lk.has(hd.room, m.id)
				return ok, saw
			}); err != nil {
				return nil, err
			}
		}
		joiners = append(joiners, hd)
	}
	keep := false
	var resp schema.DisableJoinersResponseV1
	t, err = w.measure("disable everyone from A's open link", joiners, bounds, func() error {
		return w.op.call(http.MethodPost, fmt.Sprintf("/v1/invites/%d/disable-joiners", w.a.openInviteID), schema.DisableJoinersRequestV1{Close: &keep}, &resp)
	})
	if err != nil {
		return nil, err
	}
	for _, hd := range joiners {
		if err := closedRevoked(hd.sockets); err != nil {
			return nil, err
		}
	}
	out = append(out, t)
	if err := w.staysOut(joiners, bounds); err != nil {
		return nil, fmt.Errorf("after the joiners were disabled: %w", err)
	}
	w.h.say("ok   revoked 3/4: %s; they stay out", t)

	// 4. The link is closed: a new redeem, and a preview, are invalid. The
	// v1 redeem makes no account with any code (#713), so it answers
	// password_required, as it would have before the close.
	if err := w.op.call(http.MethodDelete, fmt.Sprintf("/v1/invites/%d", w.a.openInviteID), nil, nil); err != nil {
		return nil, err
	}
	nobody := api{h: w.h, base: w.d.baseURL, who: "a newcomer"}
	for _, p := range []struct {
		path string
		body any
		want answer
	}{
		{"/v2/invites/redeem", schema.RedeemInviteRequestV2{Code: w.a.openCode, Name: w.aName("late"), Password: joinPassword},
			answer{http.StatusNotFound, schema.ErrorCodeInviteInvalid}},
		{"/v1/invites/preview", schema.InvitePreviewRequestV1{Code: w.a.openCode}, answer{http.StatusNotFound, schema.ErrorCodeInviteInvalid}},
		{"/v1/invites/redeem", schema.RedeemInviteRequestV1{Code: w.a.openCode, Name: w.aName("late")},
			answer{http.StatusConflict, schema.ErrorCodePasswordRequired}},
	} {
		status, body, err := nobody.do(http.MethodPost, p.path, p.body)
		if err != nil {
			return nil, err
		}
		if err := checkRefusal(result{status: status, body: body}, p.want, asked{}); err != nil {
			return nil, fmt.Errorf("%s with the closed link: %w", p.path, err)
		}
	}
	w.h.say("ok   revoked 4/4: the open link is closed; redeeming and previewing it answer 404 invite_invalid (v1 redeem: 409 password_required)")
	return out, nil
}
