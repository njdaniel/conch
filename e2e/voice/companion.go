package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The companion scenario (issue #933, docs/design/voice-control-plane.md §13):
// a person in voice holds one connection, p<id>, and conchd issues a second
// participant under a derived identity: the companion p<id>.screen, which may
// publish a screen and nothing else, and the viewer p<id>.view, which
// publishes nothing. Everything here is checked in LiveKit itself: the claims
// of the signed tokens, the permission LiveKit reports for each connection,
// what it refuses to publish and what it delivers.

// requestCompanion asks for a companion or viewer token as p and returns the
// decoded answer, or the status and error code of a refusal.
func (h *harness) requestCompanion(p *person, req schema.VoiceCompanionRequestV1) (schema.VoiceCompanionResponseV1, int, string, error) {
	var resp schema.VoiceCompanionResponseV1
	status, data, err := p.api.do(http.MethodPost, "/v1/channels/companion/voice/companion", req)
	if err != nil {
		return resp, status, "", err
	}
	if status != http.StatusOK {
		var e schema.Error
		_ = decodeStrict(data, &e)
		return resp, status, e.Code, nil
	}
	if err := decodeStrict(data, &resp); err != nil {
		return resp, status, "", fmt.Errorf("decode the companion response: %w", err)
	}
	h.secret("a companion join token", resp.Token)
	h.room(resp.Room)
	if err := resp.Validate(); err != nil {
		return resp, status, "", fmt.Errorf("the companion response does not satisfy the schema: %w", err)
	}
	if p.d.lk != nil {
		wantID := identity(p.id) + ".screen"
		if req.Publish == schema.VoiceCompanionPublishNone {
			wantID = identity(p.id) + ".view"
		}
		if resp.Identity != wantID || resp.Publish != req.Publish || resp.Subscribe != req.Subscribe {
			return resp, status, "", fmt.Errorf("the companion response names %s publish=%s subscribe=%v, want %s publish=%s subscribe=%v",
				resp.Identity, resp.Publish, resp.Subscribe, wantID, req.Publish, req.Subscribe)
		}
		if err := h.checkCompanionToken(resp.Token, p.d.lk, resp.Identity, resp.Room, req.Publish == schema.VoiceCompanionPublishScreen, req.Subscribe, resp.ExpiresAt.Time()); err != nil {
			return resp, status, "", fmt.Errorf("%s's companion token: %w", p.name, err)
		}
	}
	return resp, status, "", nil
}

// mustCompanion requests a companion and requires it to be granted. While the
// poller has not yet seen the person's own connection a screen companion is
// refused with voice_no_session; that is retried for up to 20 s.
func (h *harness) mustCompanion(p *person, req schema.VoiceCompanionRequestV1) (schema.VoiceCompanionResponseV1, error) {
	var resp schema.VoiceCompanionResponseV1
	err := waitForOrFail(p.name+" to be issued a companion", 20*time.Second, func() (bool, string, error) {
		r, status, code, err := h.requestCompanion(p, req)
		if err != nil {
			return false, "", err
		}
		if status == http.StatusOK {
			resp = r
			return true, "", nil
		}
		if status == http.StatusConflict && code == schema.ErrorCodeVoiceNoSession {
			return false, fmt.Sprintf("status %d %s", status, code), nil
		}
		return false, "", fmt.Errorf("status %d code %q", status, code)
	})
	return resp, err
}

// removeCompanion calls DELETE and requires 204.
func (p *person) removeCompanion(query string) error {
	status, _, err := p.api.do(http.MethodDelete, "/v1/channels/companion/voice/companion"+query, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("DELETE companion%s: status %d, want 204", query, status)
	}
	return nil
}

// checkCompanionPermission compares the permission LiveKit reports for a
// companion or a viewer with exactly its grant, field by field.
func checkCompanionPermission(perm map[string]any, publishScreen, canSubscribe bool) error {
	sources := []any{}
	if publishScreen {
		sources = []any{"SCREEN_SHARE"}
	}
	got := map[string]any{}
	for k, v := range perm {
		got[k] = v
	}
	// An empty source list may be reported as [] or left out.
	if v, ok := got["can_publish_sources"]; !ok || v == nil {
		got["can_publish_sources"] = []any{}
	}
	want := map[string]any{
		"can_subscribe":            canSubscribe,
		"can_publish":              publishScreen,
		"can_publish_data":         false,
		"can_publish_sources":      sources,
		"hidden":                   false,
		"recorder":                 false,
		"can_update_metadata":      false,
		"agent":                    false,
		"can_subscribe_metrics":    false,
		"can_manage_agent_session": false,
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("permission is %v, want exactly %v", perm, want)
	}
	return nil
}

// lkHasIdentity asks LiveKit for the participant with exactly this identity.
func (l *live) lkHasIdentity(room, id string) (bool, lkit.Participant, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	parts, err := l.adm.ListParticipants(ctx, room)
	if err != nil {
		return false, lkit.Participant{}, err.Error()
	}
	var idents []string
	for _, p := range parts {
		idents = append(idents, p.Identity)
		if p.Identity == id {
			return true, p, ""
		}
	}
	return false, lkit.Participant{}, fmt.Sprintf("LiveKit lists %v", idents)
}

func (l *live) lkWaitHas(what, room, id string, hl *lkit.Headless) error {
	return waitForOrFail("LiveKit to list "+what, 60*time.Second, func() (bool, string, error) {
		ok, _, saw := l.lkHasIdentity(room, id)
		if !ok && hl != nil && hl.Exited() {
			return false, "", errors.New("lk exited: " + hl.Tail())
		}
		return ok, saw, nil
	})
}

func (l *live) lkWaitGone(what, room, id string, within time.Duration) error {
	return waitFor("LiveKit to stop listing "+what, within, func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		parts, err := l.adm.ListParticipants(ctx, room)
		if lkit.RoomNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		for _, p := range parts {
			if p.Identity == id {
				return false, "LiveKit still lists it"
			}
		}
		return true, ""
	})
}

// companion is the scenario. cam is the person; wes watches.
func (l *live) companion(ctx context.Context) error {
	h, d := l.h, l.d
	channelID, err := d.createChannel("companion")
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("channel:%d", channelID)
	cam, err := h.newPerson(d, "cam", "companion")
	if err != nil {
		return err
	}
	wes, err := h.newPerson(d, "wes", "companion")
	if err != nil {
		return err
	}
	dir, err := h.mkdir("companion-")
	if err != nil {
		return err
	}
	shareAudio := filepath.Join(dir, "share-audio.ogg")
	if err := lkit.WriteSilentOpus(shareAudio, 900); err != nil {
		return err
	}
	hostPort := strings.TrimPrefix(l.srv.WSURL, "ws://")
	screenReq := schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishScreen, Subscribe: true}

	// Before the person has a connection there is nothing to join, and
	// nothing is issued.
	if _, status, code, err := h.requestCompanion(cam, screenReq); err != nil {
		return err
	} else if status != http.StatusConflict || code != schema.ErrorCodeVoiceNoSession {
		return fmt.Errorf("a companion before any session: status %d code %q, want 409 %s", status, code, schema.ErrorCodeVoiceNoSession)
	}

	// cam joins as herself, with a microphone. The session names the identity
	// her companion would have.
	sc, err := h.session(cam, "companion")
	if err != nil {
		return err
	}
	room := sc.Rooms[0].Room
	camHL, err := l.joinHeadless(sc.Rooms[0])
	if err != nil {
		return err
	}
	defer camHL.Stop()
	if err := l.lkWaitHas("cam", room, identity(cam.id), camHL); err != nil {
		return err
	}
	screenID, viewID := identity(cam.id)+".screen", identity(cam.id)+".view"

	// The companion: issued, its claims checked (requestCompanion), connected
	// on that token alone. Gated, so that it publishes only when the share is
	// granted. Both connections stay listed: a second connection under one
	// identity would have displaced the first, and these are two identities.
	comp, err := h.mustCompanion(cam, screenReq)
	if err != nil {
		return err
	}
	if comp.Room != room {
		return errors.New("the companion was given another room than the person's")
	}
	gate := lkit.NewShareFilter(true)
	compLK, err := h.startLK(l.lkBin, hostPort, comp.Token, gate.Filter, "--publish-demo")
	if err != nil {
		return err
	}
	defer compLK.Stop()
	if err := l.lkWaitHas("cam's companion", room, screenID, compLK); err != nil {
		return err
	}
	select {
	case <-gate.Held():
	case <-time.After(30 * time.Second):
		return errors.New("the companion's lk did not try to publish within 30 s")
	}
	// A companion issued while nothing is claimed has its screen withdrawn by
	// the next pass, until the person claims (§13). It is not removed.
	if err := waitFor("the unclaimed companion's screen to be withdrawn", 20*time.Second, func() (bool, string) {
		ok, p, saw := l.lkHasIdentity(room, screenID)
		if !ok {
			return false, saw
		}
		got := p.Permission["can_publish"]
		return got == false, fmt.Sprintf("can_publish=%v", got)
	}); err != nil {
		return err
	}
	if ok, p, saw := l.lkHasIdentity(room, screenID); !ok {
		return fmt.Errorf("the unclaimed companion left the room: %s", saw)
	} else if screen, audio := lkit.ShareTracks(p); screen || audio {
		return fmt.Errorf("the unclaimed companion published %v", p.Sources())
	}
	if ok, _, saw := l.lkHasIdentity(room, identity(cam.id)); !ok {
		return fmt.Errorf("cam's own connection is not listed beside her companion: %s", saw)
	}
	h.say("ok   companion issued: its token's claims are exactly screen_share only, no data, no microphone; both p%d and its .screen stay listed (no DUPLICATE_IDENTITY); unclaimed, its screen is withdrawn and it is not removed", cam.id)

	// cam claims. The claim is hers; the grant reaches her companion.
	before := len(compLK.Relay.IssuedTokens())
	if share, status, code, err := claimShare(cam, "companion"); err != nil {
		return err
	} else if status != http.StatusOK || share.PrincipalID != cam.id {
		return fmt.Errorf("cam claims the share: status %d code %q share %+v", status, code, share)
	}
	if err := waitFor("LiveKit to give cam's companion the screen again", 20*time.Second, func() (bool, string) {
		ok, p, saw := l.lkHasIdentity(room, screenID)
		if !ok {
			return false, saw
		}
		return checkCompanionPermission(p.Permission, true, true) == nil, fmt.Sprint(p.Permission)
	}); err != nil {
		return err
	}
	if err := waitFor("LiveKit to send the companion a token carrying the screen", 10*time.Second, func() (bool, string) {
		return len(compLK.Relay.IssuedTokens()) > before, "no token since the claim"
	}); err != nil {
		return err
	}
	gate.Release()
	if err := waitForOrFail("LiveKit to list the companion's screen", 30*time.Second, func() (bool, string, error) {
		ok, p, saw := l.lkHasIdentity(room, screenID)
		if !ok {
			if compLK.Exited() {
				return false, "", errors.New("lk exited: " + compLK.Tail())
			}
			return false, saw, nil
		}
		screen, audio := lkit.ShareTracks(p)
		if audio {
			return false, "", fmt.Errorf("the companion published share audio: %v", p.Sources())
		}
		return screen, fmt.Sprintf("sources %v", p.Sources()), nil
	}); err != nil {
		return err
	}
	// Presence: the person once, sharing, however many connections.
	if err := waitFor("wes's presence to show cam once, sharing", 30*time.Second, func() (bool, string) {
		doc, err := wes.presence("companion")
		if err != nil {
			return false, err.Error()
		}
		n, sharing := 0, false
		for _, r := range doc.Rooms {
			for _, p := range r.Participants {
				if p.PrincipalID == cam.id {
					n++
					sharing = p.Sharing
				}
			}
		}
		return n == 1 && sharing, fmt.Sprintf("cam listed %d times, sharing=%v", n, sharing)
	}); err != nil {
		return err
	}
	h.say("ok   claimed: the claim gave the companion exactly the screen; it published; presence shows cam once, sharing")

	// A second companion for cam is refused while the first is there.
	if err := waitFor("a second companion to be refused", 20*time.Second, func() (bool, string) {
		_, status, code, err := h.requestCompanion(cam, screenReq)
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusConflict && code == schema.ErrorCodeVoiceCompanionBusy, fmt.Sprintf("status %d code %q", status, code)
	}); err != nil {
		return err
	}

	// wes, on his own connection, watches cam's screen: it is a track of the
	// companion, found by its source.
	sw, err := h.session(wes, "companion")
	if err != nil {
		return err
	}
	watcher, err := h.startLK(l.lkBin, hostPort, sw.Rooms[0].Token, nil, "--auto-subscribe")
	if err != nil {
		return err
	}
	defer watcher.Stop()
	if err := waitFor("wes to be subscribed to cam's screen", 30*time.Second, func() (bool, string) {
		got := watcher.LogLines("track subscribed", `"participant": "`+screenID+`"`)
		return lkit.ContainsAll(got, `"source": "SCREEN_SHARE",`), fmt.Sprintf("%d subscriptions to the companion's tracks", len(got))
	}); err != nil {
		return err
	}

	// A viewer for cam: no parent needed beyond the entitlement, nothing
	// published, and the share is delivered to it.
	vreq := schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishNone, Subscribe: true}
	view, status, code, err := h.requestCompanion(cam, vreq)
	if err != nil {
		return fmt.Errorf("cam asks for a viewer: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("cam asks for a viewer: status %d code %q", status, code)
	}
	viewLK, err := h.startLK(l.lkBin, hostPort, view.Token, lkit.NewShareFilter(false).Filter, "--auto-subscribe", "--publish-demo")
	if err != nil {
		return err
	}
	defer viewLK.Stop()
	if err := l.lkWaitHas("cam's viewer", room, viewID, viewLK); err != nil {
		return err
	}
	if _, vp, _ := l.lkHasIdentity(room, viewID); true {
		if err := checkCompanionPermission(vp.Permission, false, true); err != nil {
			return fmt.Errorf("LiveKit's permission for the viewer: %w", err)
		}
	}
	if err := waitFor("the viewer to receive cam's screen", 30*time.Second, func() (bool, string) {
		got := viewLK.LogLines("track subscribed", `"participant": "`+screenID+`"`)
		return lkit.ContainsAll(got, `"source": "SCREEN_SHARE",`), fmt.Sprintf("%d subscriptions", len(got))
	}); err != nil {
		return err
	}
	// Its attempt to publish (the demo video, as a screen) is never answered.
	if err := waitForOrFail("the viewer's publish to be refused", 45*time.Second, func() (bool, string, error) {
		if ok, p, _ := l.lkHasIdentity(room, viewID); ok {
			if screen, audio := lkit.ShareTracks(p); screen || audio || len(p.Tracks) > 0 {
				return false, "", fmt.Errorf("the viewer published %v", p.Sources())
			}
		}
		return viewLK.Exited(), "lk is still trying to publish", nil
	}); err != nil {
		return err
	}
	if !strings.Contains(viewLK.Tail(), "timed out publishing track") {
		return fmt.Errorf("the viewer's lk exited without the refusal: %s", viewLK.Tail())
	}
	viewLK.Stop()
	h.say("ok   viewer: its token publishes nothing (LiveKit refused its publish), receives cam's screen; a viewer is not refused while a companion exists")

	// DELETE removes the companion and only it: cam's own connection and her
	// presence are untouched, and the claim stands.
	if err := cam.removeCompanion("?publish=screen"); err != nil {
		return err
	}
	if err := l.lkWaitGone("cam's companion", room, screenID, 20*time.Second); err != nil {
		return err
	}
	if ok, _, saw := l.lkHasIdentity(room, identity(cam.id)); !ok {
		return fmt.Errorf("DELETE removed cam's own connection: %s", saw)
	}
	if ok, _, _ := l.lkHas(room, wes.id); !ok {
		return errors.New("DELETE removed wes")
	}
	if _, status, code, err := h.requestCompanion(cam, schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishScreen}); err != nil {
		return fmt.Errorf("a new publish-only companion after DELETE: %w", err)
	} else if status != http.StatusOK {
		return fmt.Errorf("a new publish-only companion after DELETE: status %d code %q", status, code)
	}
	h.say("ok   removed: DELETE took the companion out of LiveKit and nothing else; a new companion was issued at once")

	// The publish-only companion (sub-model B1): its permission forbids
	// receiving, and it is delivered nothing. It also cannot publish a
	// microphone.
	if err := cam.removeCompanion(""); err != nil {
		return err
	}
	b1, err := h.mustCompanion(cam, schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishScreen})
	if err != nil {
		return err
	}
	b1LK, err := h.startLK(l.lkBin, hostPort, b1.Token, nil, "--auto-subscribe", "--publish", l.ogg)
	if err != nil {
		return err
	}
	defer b1LK.Stop()
	if err := l.lkWaitHas("the publish-only companion", room, screenID, b1LK); err != nil {
		return err
	}
	if _, p, _ := l.lkHasIdentity(room, screenID); true {
		// The share is still cam's, so the grant stands unless a pass took it.
		if p.Permission["can_subscribe"] != false {
			return fmt.Errorf("LiveKit's permission for the publish-only companion lets it receive: %v", p.Permission)
		}
	}
	if err := waitForOrFail("the companion's microphone to be refused", 45*time.Second, func() (bool, string, error) {
		if ok, p, _ := l.lkHasIdentity(room, screenID); ok {
			if _, mic := p.Mic(); mic {
				return false, "", fmt.Errorf("the companion published a microphone: %v", p.Sources())
			}
		}
		return b1LK.Exited(), "lk is still trying to publish", nil
	}); err != nil {
		return err
	}
	if !strings.Contains(b1LK.Tail(), "timed out publishing track") {
		return fmt.Errorf("the publish-only companion's lk exited without the refusal: %s", b1LK.Tail())
	}
	if got := b1LK.LogLines("track subscribed"); len(got) > 0 {
		return fmt.Errorf("the publish-only companion was delivered %d tracks", len(got))
	}
	b1LK.Stop()
	h.say("ok   publish-only companion: LiveKit's permission forbids receiving, it was delivered no track, and its microphone was refused")

	// Share audio is not in the companion's grant either (its sound is
	// another step, issue #898): the sound is refused.
	if err := cam.removeCompanion(""); err != nil {
		return err
	}
	snd, err := h.mustCompanion(cam, screenReq)
	if err != nil {
		return err
	}
	sndLK, err := h.startLK(l.lkBin, hostPort, snd.Token, lkit.NewShareFilter(false).Filter, "--publish", shareAudio)
	if err != nil {
		return err
	}
	defer sndLK.Stop()
	if err := waitForOrFail("the companion's share audio to be refused", 45*time.Second, func() (bool, string, error) {
		if ok, p, _ := l.lkHasIdentity(room, screenID); ok {
			if _, audio := lkit.ShareTracks(p); audio {
				return false, "", fmt.Errorf("the companion published share audio: %v", p.Sources())
			}
		}
		return sndLK.Exited(), "lk is still trying to publish", nil
	}); err != nil {
		return err
	}
	if !strings.Contains(sndLK.Tail(), "timed out publishing track") {
		return fmt.Errorf("the companion's lk exited without the refusal: %s", sndLK.Tail())
	}
	sndLK.Stop()
	h.say("ok   no share audio: LiveKit refused screen_share_audio from the companion")

	// The person leaving takes the companion with her, after the grace and a
	// pass (voiceCompanionGrace).
	if err := cam.removeCompanion(""); err != nil {
		return err
	}
	last, err := h.mustCompanion(cam, screenReq)
	if err != nil {
		return err
	}
	conn, status, body, err := lkit.DialSignal(ctx, l.srv.WSURL, last.Token)
	if err != nil {
		return fmt.Errorf("the last companion connecting: HTTP %d %s", status, h.redact(body))
	}
	defer conn.Close()
	if err := l.lkWaitHas("the last companion", room, screenID, nil); err != nil {
		return err
	}
	camHL.Stop()
	if err := l.lkWaitGone("cam's companion after cam left", room, screenID, 40*time.Second); err != nil {
		return err
	}
	if err := l.waitAudit("an orphaned companion's removal row", store.AuditVoiceCompanionRemoved, subject, "system", 1); err != nil {
		return err
	}
	h.say("ok   orphaned: when cam's own connection left, the companion was removed within the grace and a pass (voice_companion_removed reason=orphaned)")

	// The audit log: one voice_session_issued row per grant, each naming the
	// identity and the grant, and the removals with their reasons.
	rows, err := d.auditRows(store.AuditVoiceSessionIssued)
	if err != nil {
		return err
	}
	var grants []string
	for _, e := range rows {
		if e.Subject == subject && e.Actor == fmt.Sprintf("principal:%d", cam.id) && strings.Contains(e.Detail, "grants=companion:") {
			grants = append(grants, e.Detail[strings.Index(e.Detail, "identity="):])
		}
	}
	wantGrants := []string{
		"identity=" + screenID + " grants=companion:screen:subscribe",
		"identity=" + viewID + " grants=companion:view",
		"identity=" + screenID + " grants=companion:screen:publish_only",
		"identity=" + screenID + " grants=companion:screen:publish_only",
		"identity=" + screenID + " grants=companion:screen:subscribe",
		"identity=" + screenID + " grants=companion:screen:subscribe",
	}
	if strings.Join(grants, "|") != strings.Join(wantGrants, "|") {
		return fmt.Errorf("companion grant rows %q, want %q", grants, wantGrants)
	}
	removals, err := d.auditRows(store.AuditVoiceCompanionRemoved)
	if err != nil {
		return err
	}
	for _, e := range removals {
		if e.Subject != subject {
			continue
		}
		if !strings.Contains(e.Detail, fmt.Sprintf("principal=%d", cam.id)) || !strings.Contains(e.Detail, "participant=screen") ||
			(!strings.HasSuffix(e.Detail, "reason=requested") && !strings.HasSuffix(e.Detail, "reason=orphaned")) {
			return fmt.Errorf("a voice_companion_removed row has detail %q", e.Detail)
		}
	}
	h.say("ok   audited: one voice_session_issued row per companion grant, voice_companion_removed for each removal, and no token or room name (the final scan covers every row)")
	return nil
}
