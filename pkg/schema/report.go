package schema

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Problem reports (issue #446, private alpha A13): a tester reports a bug from
// inside the product, and the operator turns it into an issue after reading
// it. The repository is public, so a report never reaches GitHub on its own:
// conchd only stores it, and an operator files it by hand
// (ops/report-to-issue.sh).
//
// What a client sends is the bug template's fields (what you did, expected,
// what happened, how often), the client's environment and its recent
// diagnostic events. The diagnostics are typed, not free text: every field of
// an event is a number, a closed vocabulary, or a short token checked by a
// pattern, so a client cannot put a token, a ticket, an invite code, a room
// name, a channel name or message text in one by accident. Only the three
// free-text answers are the person's own words.
//
// The server adds the rest when it stores the report: its version, the
// reporter, the time, the voice health, and the reporter's recent request ids
// and socket close reasons from its own memory.
//
// These are new types; nothing published changes. They are REST bodies,
// versioned by the V1 type suffix, and carry no schema-name field.

const (
	// MaxProblemReportBytes bounds the body of a report request.
	MaxProblemReportBytes = 32 << 10
	// MaxProblemReportText is the most characters (runes) one free-text
	// answer may hold.
	MaxProblemReportText = 4000
	// MaxDiagnosticEvents is the most events a report carries: the size of
	// a client's diagnostic ring.
	MaxDiagnosticEvents = 100
	// MaxServerTraceEntries is how many of the reporter's recent requests,
	// and separately of their socket closes, the server attaches.
	MaxServerTraceEntries = 20
	// MaxProblemReportIssueURL bounds the issue address a filed report
	// carries.
	MaxProblemReportIssueURL = 512
	// RequestIDLength is the length of a request id: 16 hex characters.
	RequestIDLength = 16
	// RequestIDHeader is the response header that carries the request id.
	RequestIDHeader = "X-Request-Id"
)

// ErrorCodeProblemReportNotFound: no report has that id (HTTP 404).
const ErrorCodeProblemReportNotFound = "problem_report_not_found"

// ProblemReportOften is the bug template's "How often does it happen?". The
// vocabulary is closed.
type ProblemReportOften string

const (
	ProblemReportOftenEveryTime ProblemReportOften = "every_time"
	ProblemReportOftenSometimes ProblemReportOften = "sometimes"
	ProblemReportOftenOnce      ProblemReportOften = "once"
)

// Valid reports whether o is a recognized answer.
func (o ProblemReportOften) Valid() bool {
	switch o {
	case ProblemReportOftenEveryTime, ProblemReportOftenSometimes, ProblemReportOftenOnce:
		return true
	default:
		return false
	}
}

// Label is the answer in the bug template's words.
func (o ProblemReportOften) Label() string {
	switch o {
	case ProblemReportOftenEveryTime:
		return "Every time"
	case ProblemReportOftenSometimes:
		return "Sometimes"
	case ProblemReportOftenOnce:
		return "Once so far"
	default:
		return string(o)
	}
}

// ProblemReportState is where a report is: open until an operator has filed
// it as an issue.
type ProblemReportState string

const (
	ProblemReportStateOpen  ProblemReportState = "open"
	ProblemReportStateFiled ProblemReportState = "filed"
)

// Valid reports whether s is a recognized state.
func (s ProblemReportState) Valid() bool {
	switch s {
	case ProblemReportStateOpen, ProblemReportStateFiled:
		return true
	default:
		return false
	}
}

// DiagnosticEventKind is what a client's diagnostic event records.
type DiagnosticEventKind string

const (
	// DiagnosticSocketOpen: a socket opened. Socket says which.
	DiagnosticSocketOpen DiagnosticEventKind = "socket_open"
	// DiagnosticSocketClose: a socket closed, with Code its close code.
	DiagnosticSocketClose DiagnosticEventKind = "socket_close"
	// DiagnosticReconnect: the client tries a socket again; Attempt counts
	// from 1.
	DiagnosticReconnect DiagnosticEventKind = "reconnect"
	// DiagnosticHTTPFailure: a request failed. Code is the HTTP status (0
	// when there was no answer), Error the server's error code, RequestID
	// the X-Request-Id of the answer.
	DiagnosticHTTPFailure DiagnosticEventKind = "http_failure"
	// DiagnosticVoiceState: the voice connection changed state.
	DiagnosticVoiceState DiagnosticEventKind = "voice_state"
	// DiagnosticDeviceError: a media device failed. Name is the error's
	// name (NotAllowedError, NotFoundError, ...).
	DiagnosticDeviceError DiagnosticEventKind = "device_error"
)

// Valid reports whether k is a recognized kind.
func (k DiagnosticEventKind) Valid() bool {
	switch k {
	case DiagnosticSocketOpen, DiagnosticSocketClose, DiagnosticReconnect,
		DiagnosticHTTPFailure, DiagnosticVoiceState, DiagnosticDeviceError:
		return true
	default:
		return false
	}
}

// DiagnosticSocket names one of a client's sockets by what it carries, never
// by its address (which holds a channel name and a ticket).
type DiagnosticSocket string

const (
	DiagnosticSocketMessages DiagnosticSocket = "messages"
	DiagnosticSocketEvents   DiagnosticSocket = "events"
	DiagnosticSocketVoice    DiagnosticSocket = "voice"
)

// Valid reports whether s is a recognized socket.
func (s DiagnosticSocket) Valid() bool {
	switch s {
	case DiagnosticSocketMessages, DiagnosticSocketEvents, DiagnosticSocketVoice:
		return true
	default:
		return false
	}
}

// DiagnosticVoiceStateName is the state of a client's voice connection.
type DiagnosticVoiceStateName string

const (
	DiagnosticVoiceIdle         DiagnosticVoiceStateName = "idle"
	DiagnosticVoiceConnecting   DiagnosticVoiceStateName = "connecting"
	DiagnosticVoiceConnected    DiagnosticVoiceStateName = "connected"
	DiagnosticVoiceReconnecting DiagnosticVoiceStateName = "reconnecting"
	DiagnosticVoiceUnavailable  DiagnosticVoiceStateName = "unavailable"
	DiagnosticVoiceStopped      DiagnosticVoiceStateName = "stopped"
)

// Valid reports whether s is a recognized voice state.
func (s DiagnosticVoiceStateName) Valid() bool {
	switch s {
	case DiagnosticVoiceIdle, DiagnosticVoiceConnecting, DiagnosticVoiceConnected,
		DiagnosticVoiceReconnecting, DiagnosticVoiceUnavailable, DiagnosticVoiceStopped:
		return true
	default:
		return false
	}
}

// SocketCloseReason is why the server closed, or saw the end of, a socket. It
// is what conchd's connection log line and a report's server trace say.
type SocketCloseReason string

const (
	// SocketCloseClient: the client closed it or went away.
	SocketCloseClient SocketCloseReason = "client"
	// SocketCloseServerShutdown: conchd is stopping.
	SocketCloseServerShutdown SocketCloseReason = "server_shutdown"
	// SocketCloseKeepaliveTimeout: a ping went unanswered.
	SocketCloseKeepaliveTimeout SocketCloseReason = "keepalive_timeout"
	// SocketCloseRevoked: the credential was revoked or expired, or its
	// principal disabled.
	SocketCloseRevoked SocketCloseReason = "revoked"
	// SocketCloseRemoved: the principal was removed from the channel, or an
	// agent's manifest no longer lets it read there.
	SocketCloseRemoved SocketCloseReason = "removed"
	// SocketCloseSlowConsumer: the reader fell too far behind.
	SocketCloseSlowConsumer SocketCloseReason = "slow_consumer"
	// SocketCloseSessionEnded: the session an events socket kept alive
	// ended.
	SocketCloseSessionEnded SocketCloseReason = "session_ended"
	// SocketCloseError: a write, a read of the store or a check failed.
	SocketCloseError SocketCloseReason = "error"
)

// Valid reports whether r is a recognized reason.
func (r SocketCloseReason) Valid() bool {
	switch r {
	case SocketCloseClient, SocketCloseServerShutdown, SocketCloseKeepaliveTimeout, SocketCloseRevoked,
		SocketCloseRemoved, SocketCloseSlowConsumer, SocketCloseSessionEnded, SocketCloseError:
		return true
	default:
		return false
	}
}

// The patterns of the short fields. Each is narrow enough that a name, a
// sentence or a credential does not fit, or (a version, a platform) is the
// client's own description of itself.
var (
	reportTokenRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	reportVersionRe   = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)
	reportScreenRe    = regexp.MustCompile(`^[0-9]{1,5}x[0-9]{1,5}$`)
	reportDeviceErrRe = regexp.MustCompile(`^[A-Z][A-Za-z]{0,58}Error$`)
	requestIDRe       = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ValidRequestID reports whether id is a well-formed request id.
func ValidRequestID(id string) bool { return requestIDRe.MatchString(id) }

// ProblemReportClientV1 is the reporting client's environment.
type ProblemReportClientV1 struct {
	// Name is the client: "conch-web", "conch".
	Name string `json:"name"`
	// Version is the client's build version.
	Version string `json:"version"`
	// UserAgent is a browser's user agent; empty for a terminal client.
	UserAgent string `json:"user_agent,omitempty"`
	// Platform is the operating system and architecture as the client
	// knows them.
	Platform string `json:"platform"`
	// Screen is the viewport, "<width>x<height>"; empty when unknown.
	Screen string `json:"screen,omitempty"`
	// Voice and Share are the client's voice and screen-share state when
	// it reported, one word each; empty when the client has neither.
	Voice string `json:"voice,omitempty"`
	Share string `json:"share,omitempty"`
}

// Validate reports whether the environment is well-formed.
func (c ProblemReportClientV1) Validate() error {
	if !reportTokenRe.MatchString(strings.ReplaceAll(c.Name, "-", "_")) {
		return errors.New("schema: report client name must be a short lowercase word")
	}
	if !reportVersionRe.MatchString(c.Version) {
		return errors.New("schema: report client version must be 1 to 64 visible ASCII characters")
	}
	if err := shortLine("user_agent", c.UserAgent, 512); err != nil {
		return err
	}
	if c.Platform == "" {
		return errors.New("schema: report client platform is required")
	}
	if err := shortLine("platform", c.Platform, 128); err != nil {
		return err
	}
	if c.Screen != "" && !reportScreenRe.MatchString(c.Screen) {
		return errors.New("schema: report client screen must be <width>x<height>")
	}
	for _, f := range []struct{ name, value string }{{"voice", c.Voice}, {"share", c.Share}} {
		if f.value != "" && !reportTokenRe.MatchString(f.value) {
			return fmt.Errorf("schema: report client %s must be one lowercase word", f.name)
		}
	}
	return nil
}

// shortLine checks a one-line descriptive field: printable, at most max
// bytes.
func shortLine(name, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("schema: report client %s must be at most %d bytes", name, max)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("schema: report client %s must be one line of printable text", name)
		}
	}
	return nil
}

// DiagnosticEventV1 is one entry of a client's diagnostic ring. Which fields
// it has depends on Kind; Validate refuses a field the kind does not take.
type DiagnosticEventV1 struct {
	At   Timestamp           `json:"at"`
	Kind DiagnosticEventKind `json:"kind"`
	// Socket is the socket of socket_open, socket_close and reconnect.
	Socket DiagnosticSocket `json:"socket,omitempty"`
	// Code is a socket_close's close code, or an http_failure's status (0
	// for no answer).
	Code int `json:"code,omitempty"`
	// Error is an http_failure's error code from the server's refusal, or
	// "network" for no answer.
	Error string `json:"error,omitempty"`
	// RequestID is an http_failure's X-Request-Id.
	RequestID string `json:"request_id,omitempty"`
	// Attempt is a reconnect's attempt number, from 1.
	Attempt int `json:"attempt,omitempty"`
	// State is a voice_state's new state.
	State DiagnosticVoiceStateName `json:"state,omitempty"`
	// Name is a device_error's error name.
	Name string `json:"name,omitempty"`
}

// Validate reports whether the event is well-formed for its kind.
func (e DiagnosticEventV1) Validate() error {
	if e.At.IsZero() {
		return errors.New("schema: diagnostic event at is required")
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("schema: diagnostic event kind must be one of: %s", strings.Join(Vocabularies["DiagnosticEventKind"], ", "))
	}
	socket := e.Kind == DiagnosticSocketOpen || e.Kind == DiagnosticSocketClose || e.Kind == DiagnosticReconnect
	switch {
	case socket != (e.Socket != ""):
		return fmt.Errorf("schema: diagnostic event %s: socket is for socket events only", e.Kind)
	case e.Socket != "" && !e.Socket.Valid():
		return fmt.Errorf("schema: diagnostic event socket must be one of: %s", strings.Join(Vocabularies["DiagnosticSocket"], ", "))
	case e.Code != 0 && e.Kind != DiagnosticSocketClose && e.Kind != DiagnosticHTTPFailure:
		return fmt.Errorf("schema: diagnostic event %s takes no code", e.Kind)
	case e.Code < 0 || e.Code > 4999:
		return errors.New("schema: diagnostic event code must be between 0 and 4999")
	case (e.Error != "" || e.RequestID != "") && e.Kind != DiagnosticHTTPFailure:
		return fmt.Errorf("schema: diagnostic event %s takes no error or request id", e.Kind)
	case e.Error != "" && !reportTokenRe.MatchString(e.Error):
		return errors.New("schema: diagnostic event error must be an error code")
	case e.RequestID != "" && !ValidRequestID(e.RequestID):
		return errors.New("schema: diagnostic event request_id must be 16 hex characters")
	case (e.Attempt != 0) != (e.Kind == DiagnosticReconnect):
		return errors.New("schema: diagnostic event attempt is for reconnect, which needs one")
	case e.Attempt < 0:
		return errors.New("schema: diagnostic event attempt must be positive")
	case (e.State != "") != (e.Kind == DiagnosticVoiceState):
		return errors.New("schema: diagnostic event state is for voice_state, which needs one")
	case e.State != "" && !e.State.Valid():
		return fmt.Errorf("schema: diagnostic event state must be one of: %s", strings.Join(Vocabularies["DiagnosticVoiceStateName"], ", "))
	case (e.Name != "") != (e.Kind == DiagnosticDeviceError):
		return errors.New("schema: diagnostic event name is for device_error, which needs one")
	case e.Name != "" && !reportDeviceErrRe.MatchString(e.Name):
		return errors.New("schema: diagnostic event name must be an error name such as NotAllowedError")
	}
	return nil
}

// CreateProblemReportRequestV1 is the body of POST /v1/reports.
type CreateProblemReportRequestV1 struct {
	// Did, Expected and Happened are the bug template's three questions, in
	// the person's own words.
	Did      string                `json:"did"`
	Expected string                `json:"expected"`
	Happened string                `json:"happened"`
	Often    ProblemReportOften    `json:"often"`
	Client   ProblemReportClientV1 `json:"client"`
	// Diagnostics is the client's recent events, oldest first; empty for a
	// client that keeps none.
	Diagnostics []DiagnosticEventV1 `json:"diagnostics"`
}

// Validate reports whether the request is well-formed. Its messages name the
// field and the rule, never the value.
func (r CreateProblemReportRequestV1) Validate() error {
	for _, f := range []struct{ name, value string }{{"did", r.Did}, {"expected", r.Expected}, {"happened", r.Happened}} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("schema: report %s is required", f.name)
		}
		if !utf8.ValidString(f.value) || utf8.RuneCountInString(f.value) > MaxProblemReportText {
			return fmt.Errorf("schema: report %s must be at most %d characters", f.name, MaxProblemReportText)
		}
	}
	if !r.Often.Valid() {
		return fmt.Errorf("schema: report often must be one of: %s", strings.Join(Vocabularies["ProblemReportOften"], ", "))
	}
	if err := r.Client.Validate(); err != nil {
		return err
	}
	if len(r.Diagnostics) > MaxDiagnosticEvents {
		return fmt.Errorf("schema: a report carries at most %d diagnostic events", MaxDiagnosticEvents)
	}
	for i, e := range r.Diagnostics {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("diagnostics[%d]: %w", i, err)
		}
	}
	return nil
}

// CreateProblemReportResponseV1 is the 201 answer to a report: its id, which a
// tester can quote.
type CreateProblemReportResponseV1 struct {
	ID        int64     `json:"id"`
	CreatedAt Timestamp `json:"created_at"`
}

// ServerRequestTraceV1 is one of the reporter's recent requests as conchd saw
// it. Route is the route pattern ("GET /v2/channels/{channel}/messages"),
// never the path, which would name the channel.
type ServerRequestTraceV1 struct {
	At        Timestamp `json:"at"`
	RequestID string    `json:"request_id"`
	Route     string    `json:"route"`
	Status    int       `json:"status"`
}

// ServerSocketCloseV1 is one of the reporter's recent socket closes.
type ServerSocketCloseV1 struct {
	At              Timestamp         `json:"at"`
	RequestID       string            `json:"request_id"`
	Route           string            `json:"route"`
	Reason          SocketCloseReason `json:"reason"`
	DurationSeconds int64             `json:"duration_seconds"`
	Frames          int64             `json:"frames"`
}

// ProblemReportServerV1 is what conchd knew of the reporter when the report
// arrived, from memory: lost on a restart, and empty for a principal it has
// not seen since.
type ProblemReportServerV1 struct {
	Requests     []ServerRequestTraceV1 `json:"requests"`
	SocketCloses []ServerSocketCloseV1  `json:"socket_closes"`
}

// ProblemReportV1 is a stored report, as an operator reads it.
type ProblemReportV1 struct {
	ID          int64              `json:"id"`
	State       ProblemReportState `json:"state"`
	CreatedAt   Timestamp          `json:"created_at"`
	PrincipalID int64              `json:"principal_id"`
	// ServerVersion is conchd's build when the report arrived.
	ServerVersion string `json:"server_version"`
	// Voice is the voice health when the report arrived.
	Voice       HealthVoice           `json:"voice"`
	Did         string                `json:"did"`
	Expected    string                `json:"expected"`
	Happened    string                `json:"happened"`
	Often       ProblemReportOften    `json:"often"`
	Client      ProblemReportClientV1 `json:"client"`
	Diagnostics []DiagnosticEventV1   `json:"diagnostics"`
	Server      ProblemReportServerV1 `json:"server"`
	// Issue is the address of the issue the report was filed as; FiledAt
	// when. Both are absent while the report is open.
	Issue   string     `json:"issue,omitempty"`
	FiledAt *Timestamp `json:"filed_at,omitempty"`
	// Redact is what an issue body made from this report must not name:
	// every channel and every principal on the server, as the one answer
	// GET /v1/reports/{id} gives an operator has them. It is never stored with
	// the report and absent from every other answer.
	Redact *ProblemReportRedactionV1 `json:"redact,omitempty"`
}

// ProblemReportRedactionV1 is the names a public issue body must leave out
// (issue #446). Channels holds channel and net names, Principals every
// principal's name, each list sorted. It is operator-only data an operator
// can already learn from other routes, gathered here so that a report is
// redacted against the whole server and not only the channels the operator
// happens to be in.
type ProblemReportRedactionV1 struct {
	Channels   []string `json:"channels"`
	Principals []string `json:"principals"`
}

// ProblemReportSummaryV1 is a report as a list shows it: no free text.
type ProblemReportSummaryV1 struct {
	ID            int64              `json:"id"`
	State         ProblemReportState `json:"state"`
	CreatedAt     Timestamp          `json:"created_at"`
	PrincipalID   int64              `json:"principal_id"`
	ClientName    string             `json:"client_name"`
	ClientVersion string             `json:"client_version"`
	ServerVersion string             `json:"server_version"`
	Often         ProblemReportOften `json:"often"`
	Issue         string             `json:"issue,omitempty"`
}

// ListProblemReportsResponseV1 is the body of GET /v1/reports, newest first.
type ListProblemReportsResponseV1 struct {
	Reports []ProblemReportSummaryV1 `json:"reports"`
}

// UpdateProblemReportRequestV1 is the body of PUT /v1/reports/{id}: an
// operator marks a report filed, with the issue it became.
type UpdateProblemReportRequestV1 struct {
	State ProblemReportState `json:"state"`
	Issue string             `json:"issue"`
}

// Validate reports whether the request is well-formed: the state is filed
// (the one change a report takes) and the issue is an http(s) address.
func (r UpdateProblemReportRequestV1) Validate() error {
	if r.State != ProblemReportStateFiled {
		return fmt.Errorf("schema: a report can only be marked %q", ProblemReportStateFiled)
	}
	if len(r.Issue) == 0 || len(r.Issue) > MaxProblemReportIssueURL {
		return fmt.Errorf("schema: report issue must be 1 to %d bytes", MaxProblemReportIssueURL)
	}
	u, err := url.Parse(r.Issue)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("schema: report issue must be an http or https address")
	}
	return nil
}
