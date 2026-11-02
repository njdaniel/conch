package main

import (
	"bytes"
	"fmt"
	"net/http"

	"github.com/njdaniel/conch/pkg/schema"
)

// The per-person voice switch (issue #984, design §3.6, §8): PUT
// /v1/principals/{id}/voice-permission. B's member and the guest are refused
// by the matrix rows (matrix.go, guestrows.go), byte-identical for a person of
// A's and for a ghost. Here the operator's side: a hidden principal and an id
// nobody holds are one 404, the same bytes, so the route does not tell the
// operator which ids ever existed.
func (w *world) voicePermissionHiddenIsGhost(hidden int64) error {
	body := schema.VoicePermissionRequestV1{VoiceAllowed: false}
	status, got, err := w.op.do(http.MethodPut, fmt.Sprintf("/v1/principals/%d/voice-permission", hidden), body)
	if err != nil {
		return err
	}
	if _, err := checkAnswer(result{status: status, body: got}, answer{http.StatusNotFound, "principal_not_found"}); err != nil {
		return fmt.Errorf("the voice switch of a hidden principal: %w", err)
	}
	gstatus, ghost, err := w.op.do(http.MethodPut, fmt.Sprintf("/v1/principals/%d/voice-permission", w.ghost().id), body)
	if err != nil {
		return err
	}
	if gstatus != status || !bytes.Equal(got, ghost) {
		return fmt.Errorf("the voice switch of a hidden principal answers %d %s, a ghost id %d %s", status, got, gstatus, ghost)
	}
	return nil
}
