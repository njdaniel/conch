package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

const (
	// upstreamTimeout bounds one call to conchd.
	upstreamTimeout = 15 * time.Second
	// maxUpstreamBody bounds what is read back from conchd. Its lists are
	// paged and its documents small; a longer answer is not conchd's.
	maxUpstreamBody = 4 << 20
)

// upstream is the relay's REST client for conchd. It sends the relay's
// credential in the Authorization header and nowhere else, and it does not
// follow a redirect: a conchd that redirects would otherwise send the
// credential to wherever it points.
type upstream struct {
	base  string
	token string
	http  *http.Client
}

func newUpstream(server, token string) *upstream {
	return &upstream{
		base:  strings.TrimRight(server, "/"),
		token: token,
		http: &http.Client{
			Timeout:       upstreamTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// reply is what conchd answered: the parts the relay passes on.
type reply struct {
	status     int
	retryAfter string
	body       []byte
}

// do sends one request and returns conchd's answer whatever its status. An
// error is a call that got no answer. The credential is not in any error.
func (u *upstream) do(ctx context.Context, method, path string, query url.Values, body []byte) (reply, error) {
	target := u.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader) // #nosec G704 -- the server the operator configured
	if err != nil {
		return reply{}, errors.New("build request")
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.http.Do(req) // #nosec G704 -- as above
	if err != nil {
		// A *url.Error quotes the URL; the URL holds no credential, but the
		// transport's own words are all the caller needs.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return reply{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody+1))
	if err != nil {
		return reply{}, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if len(data) > maxUpstreamBody {
		return reply{}, fmt.Errorf("%s %s: response over %d bytes", method, path, maxUpstreamBody)
	}
	return reply{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: data}, nil
}

// refusal is a call that conchd answered with a status outside 2xx.
type refusal struct {
	Status int
	Code   string
}

func (r *refusal) Error() string {
	if r.Code != "" {
		return fmt.Sprintf("conchd answered %d %s", r.Status, r.Code)
	}
	return fmt.Sprintf("conchd answered %d", r.Status)
}

// call sends in as JSON (nil for no body) and decodes a 2xx answer into out
// (nil for none). Any other status is a *refusal carrying conchd's error code.
func (u *upstream) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	rep, err := u.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if rep.status < 200 || rep.status > 299 {
		ref := &refusal{Status: rep.status}
		var e schema.Error
		if json.Unmarshal(rep.body, &e) == nil {
			ref.Code = e.Code
		}
		return ref
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rep.body, out)
}

func (u *upstream) registerSession(ctx context.Context, req schema.CreateSessionRequestV1) (schema.SessionV1, error) {
	var session schema.SessionV1
	err := u.call(ctx, http.MethodPost, "/v1/sessions", nil, req, &session)
	return session, err
}

func (u *upstream) heartbeat(ctx context.Context, id int64) (schema.SessionV1, error) {
	var session schema.SessionV1
	err := u.call(ctx, http.MethodPost, fmt.Sprintf("/v1/sessions/%d/heartbeat", id), nil, nil, &session)
	return session, err
}

func (u *upstream) endSession(ctx context.Context, id int64) error {
	return u.call(ctx, http.MethodDelete, fmt.Sprintf("/v1/sessions/%d", id), nil, nil, nil)
}

func (u *upstream) listInterventions(ctx context.Context, session int64, query url.Values) (schema.ListInterventionsResponseV1, error) {
	var list schema.ListInterventionsResponseV1
	err := u.call(ctx, http.MethodGet, fmt.Sprintf("/v1/sessions/%d/interventions", session), query, nil, &list)
	return list, err
}

func (u *upstream) ack(ctx context.Context, id int64, req schema.AckInterventionRequestV1) error {
	return u.call(ctx, http.MethodPost, fmt.Sprintf("/v1/interventions/%d/ack", id), nil, req, nil)
}
