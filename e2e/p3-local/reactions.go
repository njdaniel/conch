package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// p316Reactions checks a real conchd, the browser participant's public API,
// the real TUI on a pseudo-terminal, and the scriptable reactor listing.
func (s *suite) p316Reactions() error {
	messages, err := s.history(partBrowser, chGeneral, 0)
	if err != nil || len(messages) == 0 {
		return fmt.Errorf("find a message for reactions: %w (%d messages)", err, len(messages))
	}
	messageID := messages[len(messages)-1].ID
	summaryPath := "/v2/channels/" + chGeneral + "/messages/" + strconv.FormatInt(messageID, 10) + "/reactions"
	path := summaryPath + "/" + url.PathEscape("👍")
	browser := s.as(partBrowser)
	var summary schema.ReactionSummaryV1
	if err := browser.call(http.MethodPut, path, nil, &summary); err != nil {
		return fmt.Errorf("browser set: %w", err)
	}
	if err := reactionCount(summary, 1, true); err != nil {
		return fmt.Errorf("browser's first summary: %w", err)
	}
	if err := s.tuiA.selectChannel(chGeneral); err != nil {
		return err
	}
	if err := s.tuiA.waitFor("the browser's reaction", 10*time.Second, func(rows []string) bool {
		return showing(rows, "👍 x1")
	}); err != nil {
		return err
	}
	if err := s.tuiA.press(keyCtrlR); err != nil {
		return err
	}
	if err := s.tuiA.typeText(":+1:"); err != nil {
		return err
	}
	if err := s.tuiA.press(keyEnter); err != nil {
		return err
	}
	if err := s.tuiA.waitFor("the terminal's own reaction and count", 10*time.Second, func(rows []string) bool {
		return showing(rows, "👍 x2 (you)")
	}); err != nil {
		return err
	}
	if err := browser.call(http.MethodGet, summaryPath, nil, &summary); err != nil {
		return fmt.Errorf("browser read after terminal set: %w", err)
	}
	if err := reactionCount(summary, 2, true); err != nil {
		return fmt.Errorf("browser and TUI disagree after terminal set: %w", err)
	}
	out, err := s.cli("reactions", "--text", chGeneral, strconv.FormatInt(messageID, 10), ":+1:")
	if err != nil {
		return err
	}
	for _, want := range []string{":thumbsup: 2 you", "nick-test", "browser-test"} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("reactor listing lacks %q:\n%s", want, out)
		}
	}
	if err := s.tuiA.press(keyCtrlU); err != nil {
		return err
	}
	if err := s.tuiA.typeText(":+1:"); err != nil {
		return err
	}
	if err := s.tuiA.press(keyEnter); err != nil {
		return err
	}
	if err := s.tuiA.waitFor("removal of the terminal's own reaction", 10*time.Second, func(rows []string) bool {
		return showing(rows, "👍 x1") && !showing(rows, "(you)")
	}); err != nil {
		return err
	}
	if err := browser.call(http.MethodGet, summaryPath, nil, &summary); err != nil {
		return fmt.Errorf("browser read after terminal removal: %w", err)
	}
	if err := reactionCount(summary, 1, true); err != nil {
		return fmt.Errorf("browser and TUI disagree after terminal removal: %w", err)
	}
	return nil
}

func reactionCount(summary schema.ReactionSummaryV1, want int, me bool) error {
	if len(summary.Reactions) != 1 || summary.Reactions[0].Emoji != "👍" || summary.Reactions[0].Count != want || summary.Reactions[0].Me != me {
		return fmt.Errorf("summary = %+v, want thumbs up x%d, me %v", summary.Reactions, want, me)
	}
	return nil
}
