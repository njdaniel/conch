package main

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// p317Moderation exercises channel moderation against the real server. P3 has
// no LiveKit, so voice removal covers routing, authorization and idempotency;
// it cannot make a media assertion.
func (s *suite) p317Moderation() error {
	nickID, guestID := s.pid("nick-test"), s.pid("jamie-test")
	if err := s.admin("member", "manage", chGeneral, id(nickID)); err != nil {
		return fmt.Errorf("make nick-test a manager: %w", err)
	}

	// A plain member cannot kick. A manager can, even with voice unconfigured
	// and the target absent, and a repeat is the same successful no-op. Chat
	// membership and access are unchanged.
	if _, err := s.cliAs(partGuest, "voice", "kick", "--yes", chGeneral, id(nickID)); err == nil {
		return errors.New("a plain member's voice kick succeeded")
	}
	for i := 0; i < 2; i++ {
		out, err := s.cliAs(partNick, "voice", "kick", "--yes", chGeneral, id(guestID))
		if err != nil {
			return fmt.Errorf("manager voice kick %d: %w", i+1, err)
		}
		if !strings.Contains(out, "voice removal completed") || !strings.Contains(out, "chat access is unchanged") {
			return fmt.Errorf("manager voice kick %d said %q", i+1, out)
		}
	}
	if _, err := s.cliAs(partGuest, "send", chGeneral, "p317voicechat"); err != nil {
		return fmt.Errorf("the voice target can still post: %w", err)
	}
	if _, err := s.waitMessage(partNick, chGeneral, "p317voicechat"); err != nil {
		return err
	}

	member := func(want bool) error {
		var list schema.ListChannelMembersResponseV1
		asOperator := api{base: s.r.baseURL(), token: s.r.fx.operatorToken}
		if err := asOperator.call(http.MethodGet, "/v1/channels/"+chGeneral+"/members", nil, &list); err != nil {
			return err
		}
		got := slices.ContainsFunc(list.Members, func(m schema.ChannelMemberV1) bool { return m.PrincipalID == guestID })
		if got != want {
			return fmt.Errorf("jamie-test's membership = %v, want %v", got, want)
		}
		return nil
	}

	// A manager cannot remove or ban an approver. Make jamie-test a plain
	// member for those checks; the operator's explicit re-add keeps that right
	// off so the later ban is the manager's to make.
	if err := s.admin("member", "decide", "--off", chGeneral, id(guestID)); err != nil {
		return fmt.Errorf("remove jamie-test's approval right: %w", err)
	}
	// Membership removal is neither a ban nor an account revocation, and an
	// explicit add restores access.
	out, err := s.cliAs(partNick, "admin", "member", "remove", "--yes", chGeneral, id(guestID))
	if err != nil {
		return fmt.Errorf("member removal: %w", err)
	}
	if !strings.Contains(out, "this is not a ban") {
		return fmt.Errorf("member removal said %q", out)
	}
	if err := member(false); err != nil {
		return err
	}
	if _, err := s.history(partGuest, chGeneral, 0); !channelNotFound(err) {
		return fmt.Errorf("removed member's history: %s, want 404 channel_not_found", describe(err))
	}
	if err := s.admin("member", "add", "--no-decide", chGeneral, id(guestID)); err != nil {
		return fmt.Errorf("re-add after member removal: %w", err)
	}
	if err := member(true); err != nil {
		return err
	}
	if _, err := s.history(partGuest, chGeneral, 0); err != nil {
		return fmt.Errorf("re-added member's history: %w", err)
	}

	t := s.tuiA
	if err := t.selectChannel(chGeneral); err != nil {
		return err
	}
	confirm := func(command, question, done string) error {
		if err := t.sendLine(command); err != nil {
			return err
		}
		if err := t.waitFor("the confirmation for "+command, 10*time.Second, func(rows []string) bool {
			return showing(rows, question)
		}); err != nil {
			return err
		}
		if err := t.press("y"); err != nil {
			return err
		}
		return t.waitFor("completion of "+command, 20*time.Second, func(rows []string) bool { return showing(rows, done) })
	}

	// Ban through the TUI, then read the same ban through its list command and
	// the CLI's JSON document. While banned, even a manager cannot re-add the
	// principal. Unbanning does not restore the membership the ban removed.
	if err := confirm("/ban "+id(guestID), "remove principal "+id(guestID)+" from #general", "ban is in force"); err != nil {
		return err
	}
	if err := t.sendLine("/bans"); err != nil {
		return err
	}
	if err := t.waitFor("the ban list", 20*time.Second, func(rows []string) bool {
		return showing(rows, "bans in #general:", id(guestID), "jamie-test")
	}); err != nil {
		return err
	}
	out, err = s.cliAs(partNick, "admin", "ban", "list", "--json", chGeneral)
	if err != nil {
		return err
	}
	var bans schema.ListChannelBansResponseV1
	if err := decodeStrict([]byte(out), &bans); err != nil {
		return fmt.Errorf("ban list JSON: %w", err)
	}
	if err := bans.Validate(); err != nil || len(bans.Bans) != 1 || bans.Bans[0].PrincipalID != guestID || bans.Bans[0].CreatedBy != nickID {
		return fmt.Errorf("ban list = %+v (%w), want only jamie-test banned by nick-test", bans.Bans, err)
	}
	if err := member(false); err != nil {
		return err
	}
	if _, err := s.cliAs(partNick, "admin", "member", "add", chGeneral, id(guestID)); err == nil {
		return errors.New("a banned principal was re-added")
	}
	if err := member(false); err != nil {
		return err
	}
	if err := confirm("/unban "+id(guestID), "lift the ban on principal "+id(guestID), "is unbanned or was"); err != nil {
		return err
	}
	out, err = s.cliAs(partNick, "admin", "ban", "list", "--json", chGeneral)
	if err != nil {
		return err
	}
	if err := decodeStrict([]byte(out), &bans); err != nil {
		return err
	}
	if len(bans.Bans) != 0 {
		return fmt.Errorf("after unban the list holds %+v", bans.Bans)
	}
	if err := member(false); err != nil {
		return fmt.Errorf("unban restored membership: %w", err)
	}
	if _, err := s.history(partGuest, chGeneral, 0); !channelNotFound(err) {
		return fmt.Errorf("unbanned but not re-added member's history: %s, want 404 channel_not_found", describe(err))
	}
	if err := s.admin("member", "add", chGeneral, id(guestID)); err != nil {
		return fmt.Errorf("explicit add after unban: %w", err)
	}
	if err := member(true); err != nil {
		return err
	}

	if err := s.p317MessageRemoval(t); err != nil {
		return err
	}
	return s.p317ScopedRemoval(guestID)
}

// p317MessageRemoval removes a held channel-wide message in the TUI and checks
// the live update, an idempotent CLI repeat, history, and a reconnecting replay.
func (s *suite) p317MessageRemoval(t *tui) error {
	const originalBody = "p317wide-original-secret"
	const controlBody = "p317wide-control"
	mark, err := s.lastID(chGeneral)
	if err != nil {
		return err
	}
	live, err := s.followMessages(partGuest, chGeneral, mark)
	if err != nil {
		return err
	}
	defer live.close()
	if err := s.say(chGeneral, originalBody); err != nil {
		return err
	}
	original, err := s.waitMessage(partGuest, chGeneral, originalBody)
	if err != nil {
		return err
	}
	if err := s.until("the live v2 socket to carry the original", 20*time.Second, func() (bool, error) {
		return live.has(original.ID), nil
	}); err != nil {
		return err
	}
	if err := t.waitFor("the message to remove", 20*time.Second, func(rows []string) bool {
		return showing(rows, "nick-test", originalBody)
	}); err != nil {
		return err
	}

	if err := t.sendLine("/remove-message " + id(original.ID)); err != nil {
		return err
	}
	if err := t.waitFor("message removal confirmation", 10*time.Second, func(rows []string) bool {
		return showing(rows, "remove message "+id(original.ID)+" in #general")
	}); err != nil {
		return err
	}
	if err := t.press("y"); err != nil {
		return err
	}
	var tomb schema.MessageV2
	if err := s.until("the live v2 socket to carry the same-ID tombstone", 20*time.Second, func() (bool, error) {
		frames := live.byID(original.ID)
		for _, m := range frames {
			if m.Removed {
				tomb = m
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	if err := removedMessage(tomb, original); err != nil {
		return fmt.Errorf("live tombstone: %w", err)
	}
	if err := t.waitFor("the TUI's held copy to become a tombstone", 20*time.Second, func(rows []string) bool {
		return showing(rows, "nick-test", schema.MessageRemovedBody) && !showing(rows, "nick-test", originalBody)
	}); err != nil {
		return err
	}

	out, err := s.cliAs(partNick, "admin", "message", "remove", "--yes", chGeneral, id(original.ID))
	if err != nil || !strings.Contains(out, "removed or was already removed") {
		return fmt.Errorf("idempotent message removal: %w (%q)", err, out)
	}
	if err := s.say(chGeneral, controlBody); err != nil {
		return err
	}
	control, err := s.waitMessage(partGuest, chGeneral, controlBody)
	if err != nil {
		return err
	}
	if err := s.until("the live socket after the repeated removal", 20*time.Second, func() (bool, error) {
		return live.has(control.ID), nil
	}); err != nil {
		return err
	}
	frames := live.byID(original.ID)
	if len(frames) != 2 || frames[0].Removed || !frames[1].Removed {
		return fmt.Errorf("the live socket carried %+v for message %d, want one original then one tombstone", frames, original.ID)
	}

	history, err := s.history(partGuest, chGeneral, 0)
	if err != nil {
		return err
	}
	held := messagesByID(history, original.ID)
	if len(held) != 1 {
		return fmt.Errorf("history holds %d copies of message %d, want one", len(held), original.ID)
	}
	if err := removedMessage(held[0], original); err != nil {
		return fmt.Errorf("history tombstone: %w", err)
	}

	replay, err := s.followMessages(partGuest, chGeneral, mark)
	if err != nil {
		return fmt.Errorf("reconnecting replay: %w", err)
	}
	defer replay.close()
	if err := s.until("the reconnecting replay to finish through the control", 20*time.Second, func() (bool, error) {
		return replay.has(control.ID), nil
	}); err != nil {
		return err
	}
	frames = replay.byID(original.ID)
	if len(frames) != 1 || !frames[0].Removed || strings.Contains(frames[0].Body, originalBody) {
		return fmt.Errorf("reconnecting replay carried %+v for message %d, want only its tombstone", frames, original.ID)
	}
	return nil
}

// p317ScopedRemoval checks that removing a whisper does not reveal its id,
// original, or removal event to a channel member outside its audience. The
// channel-wide control proves the outsider's sockets were live.
func (s *suite) p317ScopedRemoval(guestID int64) error {
	const originalBody = "p317whisper-original-secret"
	const controlBody = "p317whisper-control"
	mark, err := s.lastID(chGeneral)
	if err != nil {
		return err
	}
	insider, err := s.followMessages(partGuest, chGeneral, mark)
	if err != nil {
		return err
	}
	defer insider.close()
	outsider, err := s.followMessages(partBrowser, chGeneral, mark)
	if err != nil {
		return err
	}
	defer outsider.close()
	outsiderEvents, err := s.followEvents(partBrowser, chGeneral)
	if err != nil {
		return err
	}
	defer outsiderEvents.close()

	if err := s.say(chGeneral, originalBody, "--to", id(guestID)); err != nil {
		return err
	}
	original, err := s.waitMessage(partGuest, chGeneral, originalBody)
	if err != nil {
		return err
	}
	if err := s.say(chGeneral, controlBody); err != nil {
		return err
	}
	control, err := s.waitMessage(partBrowser, chGeneral, controlBody)
	if err != nil {
		return err
	}
	if err := s.until("the outsider's live socket to carry the control", 20*time.Second, func() (bool, error) {
		return outsider.has(control.ID), nil
	}); err != nil {
		return err
	}
	if outsider.has(original.ID) {
		return fmt.Errorf("the outsider's live socket carried scoped message %d", original.ID)
	}
	out, err := s.cliAs(partNick, "admin", "message", "remove", "--yes", chGeneral, id(original.ID))
	if err != nil || !strings.Contains(out, "removed or was already removed") {
		return fmt.Errorf("scoped message removal: %w (%q)", err, out)
	}
	if err := s.until("the whisper recipient's tombstone", 20*time.Second, func() (bool, error) {
		return slices.ContainsFunc(insider.byID(original.ID), func(m schema.MessageV2) bool { return m.Removed }), nil
	}); err != nil {
		return err
	}
	if outsider.has(original.ID) {
		return fmt.Errorf("the outsider received a tombstone for inaccessible message %d", original.ID)
	}
	if slices.ContainsFunc(outsiderEvents.removalsSeen(), func(r schema.MessageRemovedV1) bool { return r.Message.ID == original.ID }) {
		return fmt.Errorf("the outsider received a removal notification for inaccessible message %d", original.ID)
	}

	replay, err := s.followMessages(partBrowser, chGeneral, mark)
	if err != nil {
		return err
	}
	defer replay.close()
	if err := s.until("the outsider's replay to carry the control", 20*time.Second, func() (bool, error) {
		return replay.has(control.ID), nil
	}); err != nil {
		return err
	}
	if replay.has(original.ID) {
		return fmt.Errorf("the outsider's replay carried inaccessible message %d", original.ID)
	}
	return nil
}

func messagesByID(messages []schema.MessageV2, id int64) []schema.MessageV2 {
	var out []schema.MessageV2
	for _, m := range messages {
		if m.ID == id {
			out = append(out, m)
		}
	}
	return out
}

func removedMessage(got, original schema.MessageV2) error {
	if got.ID != original.ID || got.ChannelID != original.ChannelID || got.AuthorID != original.AuthorID {
		return fmt.Errorf("identity changed: got %+v, original %+v", got, original)
	}
	if !got.Removed || got.Body != schema.MessageRemovedBody || got.Payload != nil || got.EditedAt != nil {
		return fmt.Errorf("message %d is not a safe tombstone: %+v", got.ID, got)
	}
	if err := got.Validate(); err != nil {
		return fmt.Errorf("message %d tombstone is invalid: %w", got.ID, err)
	}
	return nil
}
