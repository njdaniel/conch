package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The admin door's dispatcher (issue #1326, slice S3 #1330,
// docs/design/ai-administrators.md §3.4): a catalog action runs as an
// in-process request for the catalog's route, served by the server's own mux,
// so the route class, the recent-authentication gate, the handler and the
// store's re-checks all run unchanged and the caller gets the answer an
// operator would get from that route.

// adminDispatchKey is the context key of the dispatch mark.
type adminDispatchKey struct{}

// adminDispatch is the dispatch mark: a context value no network request can
// carry. It names the one route it admits (pattern), so a wrapper that admits
// a dispatched agent does so on the catalog's route and nowhere else.
type adminDispatch struct {
	action     schema.AdminAction
	pattern    string
	permission schema.Permission
	approvalID int64
}

// withAdminDispatch marks ctx as dispatched by the door for d. It also marks
// the store's context (store.WithAdminDispatch), the one place an agent's
// grant is authority.
func withAdminDispatch(ctx context.Context, d adminDispatch) context.Context {
	return context.WithValue(store.WithAdminDispatch(ctx), adminDispatchKey{}, d)
}

// dispatchedHere reports whether r is the door's dispatch of the route r is
// being served on.
func dispatchedHere(r *http.Request) (adminDispatch, bool) {
	d, ok := r.Context().Value(adminDispatchKey{}).(adminDispatch)
	return d, ok && r.Pattern != "" && d.pattern == r.Pattern
}

// dispatchRecorder is the in-memory response writer a dispatched route
// answers into.
type dispatchRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newDispatchRecorder() *dispatchRecorder { return &dispatchRecorder{header: http.Header{}} }

func (d *dispatchRecorder) Header() http.Header { return d.header }

func (d *dispatchRecorder) WriteHeader(status int) {
	if d.status == 0 {
		d.status = status
	}
}

// Flush is a no-op: the dispatched answer is read whole. The audit export
// flushes as it streams.
func (d *dispatchRecorder) Flush() {}

func (d *dispatchRecorder) Write(b []byte) (int, error) {
	if d.status == 0 {
		d.status = http.StatusOK
	}
	return d.body.Write(b)
}

// dispatched is what a dispatched route answered.
type dispatched struct {
	status int
	// body is the route's body as JSON, or null when it answered none.
	body json.RawMessage
	// code is the error code of a refusal (status >= 400), "" otherwise.
	code string
}

// adminRequest is one built dispatch: the catalog's method and path with the
// target's ids filled, and the body after the catalog's clamps.
type adminRequest struct {
	method string
	path   string
	query  string
	body   []byte
}

// route is the display form of the request, "POST /v1/principals/7/disable".
func (a adminRequest) route() string {
	p := a.path
	if a.query != "" {
		p += "?" + a.query
	}
	return a.method + " " + p
}

// dispatch serves req through the mux as caller, under the mark for entry.
// The request inherits ctx's caller, credential and request credential, which
// the door's own route authenticated.
func (s *Server) dispatch(ctx context.Context, entry adminCatalogEntry, approvalID int64, req adminRequest) (dispatched, error) {
	target := req.path
	if req.query != "" {
		target += "?" + req.query
	}
	var body *bytes.Reader
	if req.body != nil {
		body = bytes.NewReader(req.body)
	} else {
		body = bytes.NewReader(nil)
	}
	dctx := withAdminDispatch(context.WithoutCancel(ctx), adminDispatch{
		action:     entry.action,
		pattern:    entry.pattern,
		permission: entry.permission,
		approvalID: approvalID,
	})
	hr, err := http.NewRequestWithContext(dctx, req.method, target, body)
	if err != nil {
		return dispatched{}, fmt.Errorf("admin dispatch: build request: %w", err)
	}
	if req.body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	// The mux must route the built request to the catalog's route and no
	// other: a path that matched elsewhere is a catalog bug, refused.
	if _, pattern := s.mux.Handler(hr); pattern != entry.pattern {
		return dispatched{}, fmt.Errorf("admin dispatch: %s routes to %q, not the catalog's %q", req.route(), pattern, entry.pattern)
	}
	rec := newDispatchRecorder()
	if aborted := serveRecovering(s.mux, rec, hr); aborted {
		// A streaming route abandons its answer by panicking with
		// http.ErrAbortHandler; the dispatch reports a failure, not a part.
		return dispatched{status: http.StatusInternalServerError, code: errInternal.Code, body: json.RawMessage("null")}, nil
	}
	out := dispatched{status: rec.status, body: json.RawMessage("null")}
	if out.status == 0 {
		out.status = http.StatusOK
	}
	if b := bytes.TrimSpace(rec.body.Bytes()); len(b) > 0 {
		if json.Valid(b) {
			out.body = append(json.RawMessage(nil), b...)
		} else if arr, ok := ndjsonArray(b); ok {
			// The audit export answers NDJSON: the result is its lines as
			// one JSON array.
			out.body = arr
		} else {
			// Every catalog route answers JSON; anything else is passed on
			// as a JSON string rather than broken JSON.
			quoted, _ := json.Marshal(string(b))
			out.body = quoted
		}
	}
	if out.status >= 400 {
		var e schema.Error
		if json.Unmarshal(out.body, &e) == nil {
			out.code = e.Code
		}
		if out.code == "" {
			out.code = strings.ToLower(strings.ReplaceAll(http.StatusText(out.status), " ", "_"))
		}
	}
	return out, nil
}

// serveRecovering serves r and reports whether the handler aborted with
// http.ErrAbortHandler. Any other panic is not the door's to swallow.
func serveRecovering(h http.Handler, w http.ResponseWriter, r *http.Request) (aborted bool) {
	defer func() {
		if v := recover(); v != nil {
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				aborted = true
				return
			}
			panic(v)
		}
	}()
	h.ServeHTTP(w, r)
	return false
}

// ndjsonArray turns newline-delimited JSON into one JSON array, when every
// line is a JSON value.
func ndjsonArray(b []byte) (json.RawMessage, bool) {
	var out bytes.Buffer
	out.WriteByte('[')
	n := 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil, false
		}
		if n > 0 {
			out.WriteByte(',')
		}
		out.Write(line)
		n++
	}
	out.WriteByte(']')
	return out.Bytes(), true
}
