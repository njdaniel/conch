package livekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable is returned, wrapped around the cause, for every way a room
// call can fail to get a usable answer from LiveKit: a refused connection, a
// timeout, a non-2xx status, an unreadable body. Callers map it to
// voice_unavailable. It never carries a token, the secret, or the request.
var ErrUnavailable = errors.New("livekit: unavailable")

const (
	// defaultTimeout applies when the caller's context has no deadline.
	defaultTimeout = 2 * time.Second
	// maxResponseBytes bounds how much of a response is read.
	maxResponseBytes = 1 << 20
	// maxErrorBodyBytes bounds how much of an error body is parsed for its
	// Twirp code. LiveKit's error bodies are a few hundred bytes.
	maxErrorBodyBytes = 4 << 10
	rpcPrefix         = "/twirp/livekit.RoomService/"
)

// Client calls LiveKit's room API and signs tokens. It is safe for
// concurrent use. Build one with New.
type Client struct {
	cfg     Config
	http    *http.Client
	timeout time.Duration
	now     func() time.Time
}

// New returns a Client for a configured Config. It does not contact LiveKit.
func New(cfg Config) (*Client, error) {
	if !cfg.Configured() {
		return nil, errors.New("livekit: not configured")
	}
	// Room calls carry a bearer token and normally go to a LiveKit on the same
	// host. An HTTP_PROXY in conchd's environment must not receive them.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Transport: transport,
			// Never follow a redirect: it could carry our bearer token
			// somewhere else, and LiveKit does not redirect API calls.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timeout: defaultTimeout,
		now:     time.Now,
	}, nil
}

// String describes the client without its secret. The secret could not be
// printed even without this method (see Secret); this keeps the output tidy.
func (c *Client) String() string { return "livekit.Client{" + c.cfg.String() + "}" }

// GoString is String, for %#v.
func (c *Client) GoString() string { return c.String() }

// LogValue implements slog.LogValuer.
func (c *Client) LogValue() slog.Value { return c.cfg.LogValue() }

// Room is a LiveKit room as the sweep needs it.
type Room struct {
	Name            string
	NumParticipants int
}

// Participant is one connection in a room.
type Participant struct {
	Identity string
	// JoinedAt is zero when LiveKit did not report it.
	JoinedAt time.Time
	// MicrophonePublished is true when a microphone track is published.
	MicrophonePublished bool
	// MicrophoneMuted is true when every published microphone track is muted.
	// It is false when no microphone track is published.
	MicrophoneMuted bool
	// ScreenSharePublished is true when a track with the screen-share source
	// is published, and ScreenShareAudioPublished when one with the
	// screen-share audio source is (issue #443). Muted or not: a published
	// share is a share.
	ScreenSharePublished      bool
	ScreenShareAudioPublished bool
	// ShareSourcesGranted is true when the participant's current permission
	// lets it publish either screen-share source: it may publish, and its
	// list of sources names one of them or is empty (LiveKit reads an empty
	// list as every source; measured on 1.13.7, a token naming no sources
	// is listed with can_publish_sources: []). It is false when LiveKit did
	// not report a permission at all.
	ShareSourcesGranted bool
	// CanPublish is the participant's current permission to publish at all,
	// as LiveKit reports it. It is false when LiveKit reported no permission,
	// so a caller that restores a grant from it never widens one.
	CanPublish bool
	// CanSubscribe is the participant's current permission to receive, as
	// LiveKit reports it; false when LiveKit reported no permission, so a
	// caller that restores a grant from it never widens one.
	CanSubscribe bool
}

// Sharing reports whether the participant has either screen-share source
// published.
func (p Participant) Sharing() bool { return p.ScreenSharePublished || p.ScreenShareAudioPublished }

// Transmitting reports whether the participant has a microphone track
// published and unmuted, which is what push-to-talk looks like.
func (p Participant) Transmitting() bool { return p.MicrophonePublished && !p.MicrophoneMuted }

// CreateRoom creates the named room. LiveKit treats an existing name as
// success and leaves the room alone, so callers may call it before every
// session.
func (c *Client) CreateRoom(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("livekit: CreateRoom needs a room name")
	}
	// LiveKit answers with the room. Requiring the name back means a 200 from
	// something that is not LiveKit (a proxy's error page, a wrong address)
	// is not taken for a room that exists.
	var room struct {
		Name string `json:"name"`
	}
	if err := c.call(ctx, "CreateRoom", adminGrant{RoomCreate: true}, map[string]any{"name": name}, &room); err != nil {
		return err
	}
	if room.Name != name {
		return fmt.Errorf("%w: CreateRoom: the answer does not name the room", ErrUnavailable)
	}
	return nil
}

// ListRooms returns every room LiveKit currently has, with its participant
// count. The count lags by a few seconds (measured on 1.13.7: zero for about
// two seconds after a join), so treat a listed room as possibly occupied and
// ask ListParticipants for the truth. A room LiveKit closed for being empty is
// not listed.
func (c *Client) ListRooms(ctx context.Context) ([]Room, error) {
	var resp struct {
		Rooms []struct {
			Name            string  `json:"name"`
			NumParticipants flexInt `json:"numParticipants"`
			SnakeNum        flexInt `json:"num_participants"`
		} `json:"rooms"`
	}
	if err := c.call(ctx, "ListRooms", adminGrant{RoomList: true}, map[string]any{}, &resp); err != nil {
		return nil, err
	}
	rooms := make([]Room, 0, len(resp.Rooms))
	for _, r := range resp.Rooms {
		rooms = append(rooms, Room{Name: r.Name, NumParticipants: int(max(r.NumParticipants, r.SnakeNum))})
	}
	return rooms, nil
}

// ListParticipants returns the participants of room. A room LiveKit does not
// have is an empty list, not an error (measured on 1.13.7).
func (c *Client) ListParticipants(ctx context.Context, room string) ([]Participant, error) {
	var resp struct {
		Participants []struct {
			Identity     string  `json:"identity"`
			JoinedAt     flexInt `json:"joinedAt"`
			SnakeJoined  flexInt `json:"joined_at"`
			JoinedAtMs   flexInt `json:"joinedAtMs"`
			SnakeJoinedM flexInt `json:"joined_at_ms"`
			Tracks       []struct {
				Type   flexEnum `json:"type"`
				Source flexEnum `json:"source"`
				Muted  bool     `json:"muted"`
			} `json:"tracks"`
			Permission *participantPermission `json:"permission"`
		} `json:"participants"`
	}
	// An admin grant with no room named is wider than this call needs, so an
	// empty room never reaches the signer.
	if room == "" {
		return nil, errors.New("livekit: ListParticipants needs a room name")
	}
	grant := adminGrant{RoomAdmin: true, Room: room}
	if err := c.call(ctx, "ListParticipants", grant, map[string]any{"room": room}, &resp); err != nil {
		return nil, err
	}
	out := make([]Participant, 0, len(resp.Participants))
	for _, p := range resp.Participants {
		part := Participant{Identity: p.Identity}
		switch ms, s := max(p.JoinedAtMs, p.SnakeJoinedM), max(p.JoinedAt, p.SnakeJoined); {
		case ms > 0:
			part.JoinedAt = time.UnixMilli(int64(ms)).UTC()
		case s > 0:
			part.JoinedAt = time.Unix(int64(s), 0).UTC()
		}
		mics, muted := 0, 0
		for _, t := range p.Tracks {
			// A screen share is recognised by its source alone, whatever
			// its type: a source that is not allowed is refused by LiveKit
			// whatever the type says, so a track of an unexpected type
			// under a share source is still a share.
			switch {
			case t.Source.is(sourceScreenShare, 3):
				part.ScreenSharePublished = true
				continue
			case t.Source.is(sourceScreenShareAudio, 4):
				part.ScreenShareAudioPublished = true
				continue
			}
			// protojson drops default values: an AUDIO type (0) is usually
			// absent, so only an explicit non-audio type disqualifies a track.
			if !t.Source.is(sourceMicrophone, 2) || (!t.Type.absent() && !t.Type.is("AUDIO", 0)) {
				continue
			}
			mics++
			if t.Muted {
				muted++
			}
		}
		part.MicrophonePublished = mics > 0
		part.MicrophoneMuted = mics > 0 && muted == mics
		part.ShareSourcesGranted = p.Permission.grantsShare()
		part.CanPublish = p.Permission != nil && (p.Permission.CanPublish || p.Permission.SnakeCanPublish)
		part.CanSubscribe = p.Permission != nil && (p.Permission.CanSubscribe || p.Permission.SnakeSubscribe)
		out = append(out, part)
	}
	return out, nil
}

// RemoveParticipant disconnects identity from room. It does not stop the
// holder of an unexpired token from rejoining.
//
// Removing someone who is not in the room, or from a room LiveKit no longer
// has, succeeds: the outcome the caller wants already holds, and reporting it
// as ErrUnavailable would make a participant leaving just before their removal
// look like a LiveKit outage. LiveKit says so with a 404 whose body is the
// Twirp error {"code":"not_found"} (measured on 1.13.7). Only that exact answer
// counts. A 404 with any other body is a wrong route or a proxy in the way
// (LiveKit answers "bad_route" for an unknown method, and plain text for an
// unknown path), and treating it as done would record a removal that never
// happened.
func (c *Client) RemoveParticipant(ctx context.Context, room, identity string) error {
	_, err := c.Evict(ctx, room, identity)
	return err
}

// Evict is RemoveParticipant that also reports whether LiveKit had the
// participant: true when it disconnected someone, false when there was nobody
// by that identity to disconnect (the not_found answer described above). A
// caller that records removals needs the difference.
func (c *Client) Evict(ctx context.Context, room, identity string) (removed bool, err error) {
	if room == "" || identity == "" {
		return false, errors.New("livekit: RemoveParticipant needs a room name and an identity")
	}
	grant := adminGrant{RoomAdmin: true, Room: room}
	err = c.call(ctx, "RemoveParticipant", grant, map[string]any{"room": room, "identity": identity}, nil)
	var status httpStatusError
	if errors.As(err, &status) && status.status == http.StatusNotFound && status.twirpCode == "not_found" {
		return false, nil
	}
	return err == nil, err
}

// DeleteRoom deletes the named room and disconnects everyone in it. Deleting
// a room LiveKit no longer has succeeds: LiveKit closes empty rooms and loses
// all of them on a restart, and the outcome the caller wants already holds. It
// says so with the Twirp answer {"code":"not_found"}; only that exact answer
// counts (see RemoveParticipant). LiveKit's DeleteRoom needs the room-create
// permission, so the signed grant is the one CreateRoom uses.
func (c *Client) DeleteRoom(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("livekit: DeleteRoom needs a room name")
	}
	err := c.call(ctx, "DeleteRoom", adminGrant{RoomCreate: true}, map[string]any{"room": name}, nil)
	var status httpStatusError
	if errors.As(err, &status) && status.status == http.StatusNotFound && status.twirpCode == "not_found" {
		return nil
	}
	return err
}

// LiveKit's names for the track sources conchd grants, as its room API
// writes them (the TrackSource enum). Join tokens use the lower-case form
// (token.go).
const (
	sourceMicrophone       = "MICROPHONE"
	sourceScreenShare      = "SCREEN_SHARE"
	sourceScreenShareAudio = "SCREEN_SHARE_AUDIO"
)

// Permission is what UpdateParticipant sets a connected participant's
// permission to. LiveKit replaces the whole permission with what it is sent,
// so this names only what conchd decides and the request spells out the
// rest: subscribe, never publish data, nothing hidden or administrative.
// With ScreenShare false it is exactly what a join token grants (JoinToken),
// and LiveKit treats setting it on a participant that already has it as no
// change at all.
type Permission struct {
	// CanPublish is the join token's publishing grant: may publish a
	// microphone. False is a listen-only participant.
	CanPublish bool
	// ScreenShare adds the screen-share sources to the microphone: a screen
	// and its sound. It means nothing without CanPublish.
	ScreenShare bool

	// explicit, with sources, is the per-source form (SourcePermission): the
	// list of sources is exactly sources, and CanPublish and ScreenShare are
	// not consulted for it. The bundled form above is what every other
	// Permission is, and its requests are unchanged.
	explicit    bool
	noSubscribe bool
	listed      []Source
}

// subscribes is the can_subscribe a request for p carries: always true,
// except for a per-source permission built to leave subscribing off.
func (p Permission) subscribes() bool { return !p.noSubscribe }

// Source is a track source a permission may name, as LiveKit's room API
// writes it.
type Source string

// The sources a per-source permission may name.
const (
	SourceScreenShare      Source = sourceScreenShare
	SourceScreenShareAudio Source = sourceScreenShareAudio
)

// SourcePermission is the per-source form of Permission, for a participant
// that is not a person's microphone connection (a companion, issue #932):
// canSubscribe and canPublish as given, and exactly the sources named, in
// order. LiveKit replaces a permission whole, so the caller passes the
// participant's own canSubscribe: a companion issued publish-only must not
// gain the right to receive by being granted or stripped of the screen.
//
// No sources with canPublish false is a participant that publishes nothing.
// No sources with canPublish true would read as every source to LiveKit, and
// a source other than the two exported is not a thing conchd grants:
// UpdateParticipant refuses both before any request is made.
func SourcePermission(canSubscribe, canPublish bool, sources ...Source) Permission {
	return Permission{CanPublish: canPublish, explicit: true, noSubscribe: !canSubscribe, listed: append([]Source{}, sources...)}
}

// check refuses a per-source permission that would mean more than it says.
func (p Permission) check() error {
	if !p.explicit {
		return nil
	}
	if p.CanPublish && len(p.listed) == 0 {
		return errors.New("livekit: a permission that may publish must name its sources: none means every source")
	}
	for _, s := range p.listed {
		if s != SourceScreenShare && s != SourceScreenShareAudio {
			return errors.New("livekit: a per-source permission may name only the screen-share sources")
		}
	}
	return nil
}

// sources lists the sources p allows, in LiveKit's room-API names. The list is
// never nil, so that an empty one is written as [] and not as null.
func (p Permission) sources() []string {
	if p.explicit {
		out := make([]string, 0, len(p.listed))
		for _, s := range p.listed {
			out = append(out, string(s))
		}
		return out
	}
	if p.ScreenShare {
		return []string{sourceMicrophone, sourceScreenShare, sourceScreenShareAudio}
	}
	return []string{sourceMicrophone}
}

// participantPermission is the permission ListParticipants reports for a
// participant, in either of the JSON spellings protojson may use.
type participantPermission struct {
	CanPublish      bool       `json:"canPublish"`
	SnakeCanPublish bool       `json:"can_publish"`
	CanSubscribe    bool       `json:"canSubscribe"`
	SnakeSubscribe  bool       `json:"can_subscribe"`
	Sources         []flexEnum `json:"canPublishSources"`
	SnakeSources    []flexEnum `json:"can_publish_sources"`
}

// grantsShare reports whether the permission lets its holder publish either
// screen-share source. A nil permission (LiveKit reported none) grants
// nothing that can be seen; the tracks are then the only evidence.
func (pp *participantPermission) grantsShare() bool {
	if pp == nil || (!pp.CanPublish && !pp.SnakeCanPublish) {
		return false
	}
	sources := append(append([]flexEnum(nil), pp.Sources...), pp.SnakeSources...)
	// LiveKit reads an empty list as "any source" (protocol auth.VideoGrant
	// GetCanPublishSource), so an empty list grants the share.
	if len(sources) == 0 {
		return true
	}
	for _, s := range sources {
		if s.is(sourceScreenShare, 3) || s.is(sourceScreenShareAudio, 4) {
			return true
		}
	}
	return false
}

// UpdateParticipant sets the permission of identity's connection to room,
// which is how a screen share is granted and withdrawn (issue #443). LiveKit
// applies it at once: withdrawing a source unpublishes every track of that
// source, and every subscriber is told it is gone, before the call returns
// (measured on 1.13.7). It also sends the connection a fresh token of its own
// carrying the new permission, so a client that reconnects on the token it
// was last sent keeps exactly what it has now.
//
// found is false, with a nil error, when LiveKit has nobody by that identity
// in the room: the not_found Twirp answer (measured on 1.13.7: 404,
// {"code":"not_found","msg":"twirp error unknown: participant does not
// exist"}). Only that exact answer counts, as for RemoveParticipant. A 200
// must name the identity back, so an answer from something that is not
// LiveKit is not taken for a change that was made.
func (c *Client) UpdateParticipant(ctx context.Context, room, identity string, p Permission) (found bool, err error) {
	if room == "" || identity == "" {
		return false, errors.New("livekit: UpdateParticipant needs a room name and an identity")
	}
	if err := p.check(); err != nil {
		return false, err
	}
	// Every field of the permission is written out: LiveKit replaces the
	// permission as a whole, and protojson reads an absent bool as false,
	// which for subscribing is not what the join token granted.
	req := map[string]any{
		"room":     room,
		"identity": identity,
		"permission": map[string]any{
			"can_subscribe":            p.subscribes(),
			"can_publish":              p.CanPublish,
			"can_publish_data":         false,
			"can_publish_sources":      p.sources(),
			"hidden":                   false,
			"recorder":                 false,
			"can_update_metadata":      false,
			"agent":                    false,
			"can_subscribe_metrics":    false,
			"can_manage_agent_session": false,
		},
	}
	var answer struct {
		Identity string `json:"identity"`
	}
	grant := adminGrant{RoomAdmin: true, Room: room}
	err = c.call(ctx, "UpdateParticipant", grant, req, &answer)
	var status httpStatusError
	if errors.As(err, &status) && status.status == http.StatusNotFound && status.twirpCode == "not_found" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if answer.Identity != identity {
		return false, fmt.Errorf("%w: UpdateParticipant: the answer does not name the participant", ErrUnavailable)
	}
	return true, nil
}

// httpStatusError is a non-2xx answer from LiveKit. It is always wrapped in
// ErrUnavailable; it exists so a caller inside this package can tell LiveKit's
// "not found" from an outage. twirpCode is the "code" of a Twirp JSON error
// body, or empty. It is never put in the error text: the body is not ours.
type httpStatusError struct {
	status    int
	twirpCode string
}

func (e httpStatusError) Error() string { return "HTTP " + strconv.Itoa(e.status) }

// call signs a fresh admin token carrying grant, POSTs req to the method, and
// decodes the JSON answer into out (when non-nil). Every failure is wrapped
// in ErrUnavailable. Errors name the method and the HTTP status only.
func (c *Client) call(ctx context.Context, method string, grant adminGrant, req, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	token, err := c.adminToken(grant)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: %s: encode request", ErrUnavailable, method)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL+rpcPrefix+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %s: build request", ErrUnavailable, method)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(hr) // #nosec G704 -- the API address is operator configuration
	if err != nil {
		// A *url.Error repeats the request URL; the inner error is the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		status := httpStatusError{status: resp.StatusCode}
		if raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes)); err == nil {
			var twirp struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(raw, &twirp) == nil {
				status.twirpCode = twirp.Code
			}
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, method, status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: %s: read response: %w", ErrUnavailable, method, unwrapURL(err))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %s: malformed response: %w", ErrUnavailable, method, err)
	}
	return nil
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// flexInt decodes a JSON number or a numeric string. protojson writes 64-bit
// integers (join times) as strings and 32-bit ones as numbers.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return errors.New("not an integer")
	}
	*f = flexInt(n)
	return nil
}

// flexEnum decodes a protobuf enum written either as its name or its number.
type flexEnum struct {
	set  bool
	name string
	num  int64
}

func (f *flexEnum) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	f.set = true
	if strings.HasPrefix(s, `"`) {
		var name string
		if err := json.Unmarshal(b, &name); err != nil {
			return err
		}
		f.name, f.num = strings.ToUpper(name), -1
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return errors.New("not an enum")
	}
	f.num = n
	return nil
}

func (f flexEnum) absent() bool { return !f.set }

func (f flexEnum) is(name string, num int64) bool {
	return f.set && (f.name == name || (f.name == "" && f.num == num))
}
