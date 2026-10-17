package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProblemReportGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"create-problem-report-request-v1.json", func() any { return new(CreateProblemReportRequestV1) }},
		{"create-problem-report-request-v1-terminal.json", func() any { return new(CreateProblemReportRequestV1) }},
		{"create-problem-report-response-v1.json", func() any { return new(CreateProblemReportResponseV1) }},
		{"problem-report-v1.json", func() any { return new(ProblemReportV1) }},
		{"problem-report-v1-filed.json", func() any { return new(ProblemReportV1) }},
		{"problem-report-v1-redact.json", func() any { return new(ProblemReportV1) }},
		{"list-problem-reports-response-v1.json", func() any { return new(ListProblemReportsResponseV1) }},
		{"list-problem-reports-response-v1-empty.json", func() any { return new(ListProblemReportsResponseV1) }},
		{"update-problem-report-request-v1.json", func() any { return new(UpdateProblemReportRequestV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// The request fixtures are valid requests, so a change to Validate that
// refuses what a client sends today fails here.
func TestProblemReportRequestFixturesValidate(t *testing.T) {
	for _, file := range []string{"create-problem-report-request-v1.json", "create-problem-report-request-v1-terminal.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name from this test
		if err != nil {
			t.Fatal(err)
		}
		var req CreateProblemReportRequestV1
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if err := req.Validate(); err != nil {
			t.Errorf("%s: %v", file, err)
		}
	}
	var up UpdateProblemReportRequestV1
	raw, err := os.ReadFile(filepath.Join("testdata", "update-problem-report-request-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &up); err != nil {
		t.Fatal(err)
	}
	if err := up.Validate(); err != nil {
		t.Errorf("update fixture: %v", err)
	}
}

func validReport() CreateProblemReportRequestV1 {
	return CreateProblemReportRequestV1{
		Did: "did", Expected: "expected", Happened: "happened", Often: ProblemReportOftenOnce,
		Client:      ProblemReportClientV1{Name: "conch-web", Version: "v1", Platform: "Linux"},
		Diagnostics: []DiagnosticEventV1{},
	}
}

func TestCreateProblemReportRequestValidate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	tests := []struct {
		name   string
		change func(r *CreateProblemReportRequestV1)
		ok     bool
	}{
		{"valid", func(*CreateProblemReportRequestV1) {}, true},
		{"blank did", func(r *CreateProblemReportRequestV1) { r.Did = "  \n" }, false},
		{"no expected", func(r *CreateProblemReportRequestV1) { r.Expected = "" }, false},
		{"no happened", func(r *CreateProblemReportRequestV1) { r.Happened = "" }, false},
		{"happened at the limit", func(r *CreateProblemReportRequestV1) { r.Happened = strings.Repeat("é", MaxProblemReportText) }, true},
		{"happened too long", func(r *CreateProblemReportRequestV1) { r.Happened = strings.Repeat("a", MaxProblemReportText+1) }, false},
		{"unknown often", func(r *CreateProblemReportRequestV1) { r.Often = "daily" }, false},
		{"no client name", func(r *CreateProblemReportRequestV1) { r.Client.Name = "" }, false},
		{"client name with a space", func(r *CreateProblemReportRequestV1) { r.Client.Name = "my client" }, false},
		{"version with a space", func(r *CreateProblemReportRequestV1) { r.Client.Version = "v 1" }, false},
		{"no platform", func(r *CreateProblemReportRequestV1) { r.Client.Platform = "" }, false},
		{"platform on two lines", func(r *CreateProblemReportRequestV1) { r.Client.Platform = "a\nb" }, false},
		{"screen", func(r *CreateProblemReportRequestV1) { r.Client.Screen = "390x844" }, true},
		{"bad screen", func(r *CreateProblemReportRequestV1) { r.Client.Screen = "big" }, false},
		{"voice word", func(r *CreateProblemReportRequestV1) { r.Client.Voice = "connected" }, true},
		{"voice sentence", func(r *CreateProblemReportRequestV1) { r.Client.Voice = "in #general" }, false},
		{"share sentence", func(r *CreateProblemReportRequestV1) { r.Client.Share = "Sharing with Alice" }, false},
		{"too many events", func(r *CreateProblemReportRequestV1) {
			r.Diagnostics = make([]DiagnosticEventV1, MaxDiagnosticEvents+1)
			for i := range r.Diagnostics {
				r.Diagnostics[i] = DiagnosticEventV1{At: at, Kind: DiagnosticVoiceState, State: DiagnosticVoiceIdle}
			}
		}, false},
		{"a bad event", func(r *CreateProblemReportRequestV1) {
			r.Diagnostics = []DiagnosticEventV1{{At: at, Kind: "message", Name: "hello"}}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validReport()
			tt.change(&r)
			if err := r.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// Each diagnostic field takes only what its kind records, in a shape a name,
// a sentence or a credential does not fit.
func TestDiagnosticEventValidate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	tests := []struct {
		name string
		e    DiagnosticEventV1
		ok   bool
	}{
		{"socket open", DiagnosticEventV1{At: at, Kind: DiagnosticSocketOpen, Socket: DiagnosticSocketVoice}, true},
		{"socket close", DiagnosticEventV1{At: at, Kind: DiagnosticSocketClose, Socket: DiagnosticSocketEvents, Code: 1008}, true},
		{"reconnect", DiagnosticEventV1{At: at, Kind: DiagnosticReconnect, Socket: DiagnosticSocketMessages, Attempt: 3}, true},
		{"http failure", DiagnosticEventV1{At: at, Kind: DiagnosticHTTPFailure, Code: 429, Error: "rate_limited", RequestID: "0123456789abcdef"}, true},
		{"no answer", DiagnosticEventV1{At: at, Kind: DiagnosticHTTPFailure, Error: "network"}, true},
		{"voice state", DiagnosticEventV1{At: at, Kind: DiagnosticVoiceState, State: DiagnosticVoiceConnected}, true},
		{"device error", DiagnosticEventV1{At: at, Kind: DiagnosticDeviceError, Name: "NotAllowedError"}, true},

		{"no time", DiagnosticEventV1{Kind: DiagnosticSocketOpen, Socket: DiagnosticSocketVoice}, false},
		{"unknown kind", DiagnosticEventV1{At: at, Kind: "message"}, false},
		{"socket without a socket", DiagnosticEventV1{At: at, Kind: DiagnosticSocketOpen}, false},
		{"socket by address", DiagnosticEventV1{At: at, Kind: DiagnosticSocketOpen, Socket: "/v2/ws?channel=general"}, false},
		{"socket on an http failure", DiagnosticEventV1{At: at, Kind: DiagnosticHTTPFailure, Socket: DiagnosticSocketVoice}, false},
		{"code on a voice state", DiagnosticEventV1{At: at, Kind: DiagnosticVoiceState, State: DiagnosticVoiceIdle, Code: 1}, false},
		{"code out of range", DiagnosticEventV1{At: at, Kind: DiagnosticSocketClose, Socket: DiagnosticSocketVoice, Code: 70000}, false},
		{"error as text", DiagnosticEventV1{At: at, Kind: DiagnosticHTTPFailure, Error: "channel general not found"}, false},
		{"error is a token", DiagnosticEventV1{At: at, Kind: DiagnosticHTTPFailure, Error: "conch_AbCdEf0123"}, false},
		{"error on a socket", DiagnosticEventV1{At: at, Kind: DiagnosticSocketClose, Socket: DiagnosticSocketVoice, Error: "x"}, false},
		{"request id too long", DiagnosticEventV1{At: at, Kind: DiagnosticHTTPFailure, RequestID: "0123456789abcdef0"}, false},
		{"request id not hex", DiagnosticEventV1{At: at, Kind: DiagnosticHTTPFailure, RequestID: "conch_inv_aaaaaa"}, false},
		{"reconnect without attempt", DiagnosticEventV1{At: at, Kind: DiagnosticReconnect, Socket: DiagnosticSocketVoice}, false},
		{"attempt elsewhere", DiagnosticEventV1{At: at, Kind: DiagnosticSocketOpen, Socket: DiagnosticSocketVoice, Attempt: 1}, false},
		{"voice state free text", DiagnosticEventV1{At: at, Kind: DiagnosticVoiceState, State: "in room conch-general"}, false},
		{"voice state missing", DiagnosticEventV1{At: at, Kind: DiagnosticVoiceState}, false},
		{"device name sentence", DiagnosticEventV1{At: at, Kind: DiagnosticDeviceError, Name: "Permission denied by Alice"}, false},
		{"device name not an error", DiagnosticEventV1{At: at, Kind: DiagnosticDeviceError, Name: "General"}, false},
		{"name elsewhere", DiagnosticEventV1{At: at, Kind: DiagnosticVoiceState, State: DiagnosticVoiceIdle, Name: "NotAllowedError"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.e.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestUpdateProblemReportRequestValidate(t *testing.T) {
	tests := []struct {
		name string
		r    UpdateProblemReportRequestV1
		ok   bool
	}{
		{"filed", UpdateProblemReportRequestV1{State: ProblemReportStateFiled, Issue: "https://github.com/o/r/issues/1"}, true},
		{"open", UpdateProblemReportRequestV1{State: ProblemReportStateOpen, Issue: "https://github.com/o/r/issues/1"}, false},
		{"no issue", UpdateProblemReportRequestV1{State: ProblemReportStateFiled}, false},
		{"not a url", UpdateProblemReportRequestV1{State: ProblemReportStateFiled, Issue: "issue 1"}, false},
		{"javascript", UpdateProblemReportRequestV1{State: ProblemReportStateFiled, Issue: "javascript:alert(1)"}, false},
		{"too long", UpdateProblemReportRequestV1{State: ProblemReportStateFiled, Issue: "https://x/" + strings.Repeat("a", MaxProblemReportIssueURL)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.r.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestRequestIDShape(t *testing.T) {
	for id, want := range map[string]bool{
		"0123456789abcdef":  true,
		"0123456789ABCDEF":  false,
		"0123456789abcde":   false,
		"0123456789abcdef0": false,
		"":                  false,
	} {
		if got := ValidRequestID(id); got != want {
			t.Errorf("ValidRequestID(%q) = %v, want %v", id, got, want)
		}
	}
	if RequestIDLength != 16 {
		t.Errorf("RequestIDLength = %d", RequestIDLength)
	}
}

func TestProblemReportOftenLabels(t *testing.T) {
	for _, o := range []ProblemReportOften{ProblemReportOftenEveryTime, ProblemReportOftenSometimes, ProblemReportOftenOnce} {
		if o.Label() == string(o) {
			t.Errorf("%s has no label", o)
		}
	}
}

// Health carries voice beside the fields it always had.
func TestHealthVoice(t *testing.T) {
	raw, err := json.Marshal(Health{Status: HealthOK, Version: "v1", DB: HealthOK, Voice: HealthVoiceUnavailable})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"status":"ok","version":"v1","db":"ok","voice":"unavailable"}`; string(raw) != want {
		t.Errorf("health = %s, want %s", raw, want)
	}
}
