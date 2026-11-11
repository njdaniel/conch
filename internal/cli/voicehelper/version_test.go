package voicehelper

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

const (
	v1 = schema.VoiceControlSchemaV1
	v2 = schema.VoiceControlSchemaV2
)

// TestVersionNegotiation: the owner chooses v2 when the hello offers it and v1
// otherwise, writes its start in that version, and every command after it (the
// fake refuses a line of the other version, which fails the command), and the
// status says which version it is and, in v2, the playback and talk mode.
func TestVersionNegotiation(t *testing.T) {
	tests := []struct {
		name         string
		versions     string
		extra        []string
		wantProtocol string
		wantTalk     schema.VoiceControlTalkMode
	}{
		{name: "v1 only", versions: v1, wantProtocol: v1},
		{name: "v1 and v2", versions: v1 + "," + v2, wantProtocol: v2, wantTalk: schema.VoiceControlTalkModePushToTalk},
		{name: "v2 listed first", versions: v2 + "," + v1, wantProtocol: v2, wantTalk: schema.VoiceControlTalkModePushToTalk},
		{name: "v2 only", versions: v2, wantProtocol: v2, wantTalk: schema.VoiceControlTalkModePushToTalk},
		{name: "v2 with the talk mode open", versions: v1 + "," + v2, extra: []string{"-talk-open"}, wantProtocol: v2, wantTalk: schema.VoiceControlTalkModeOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dumpFile := filepath.Join(t.TempDir(), "dump")
			args := append([]string{"-dump", dumpFile, "-versions", tt.versions}, tt.extra...)
			s := newSupervisor(t, storedProfile(t), Timings{}, args...)
			ctx := ctxFor(t)
			if err := s.Join(ctx, testChannel); err != nil {
				t.Fatalf("join: %v", err)
			}
			// Each of these is written in the session's version, or the fake
			// refuses it.
			for i, step := range []func() error{
				func() error { return s.SetMute(ctx, true) },
				func() error { return s.SetDeafen(ctx, true) },
				func() error { return s.Refresh(ctx) },
			} {
				if err := step(); err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
			}
			st := s.Status()
			if st.Protocol != tt.wantProtocol || st.Phase != PhaseConnected || !st.Muted || !st.Deafened {
				t.Errorf("status %+v, want protocol %s, connected, muted and deafened", st, tt.wantProtocol)
			}
			wantPlayback := schema.VoiceControlPlayback("")
			if tt.wantProtocol == v2 {
				wantPlayback = schema.VoiceControlPlaybackOn
			}
			if st.TalkMode != tt.wantTalk || st.Playback != wantPlayback {
				t.Errorf("talk mode %q, playback %q, want %q, %q", st.TalkMode, st.Playback, tt.wantTalk, wantPlayback)
			}
			start := readDump(t, dumpFile).one("start")
			if got := schemaOf(t, start); got != tt.wantProtocol {
				t.Errorf("the start is in %s, want %s", got, tt.wantProtocol)
			}
			if err := s.Leave(ctx); err != nil {
				t.Fatalf("leave: %v", err)
			}
			waitReaped(t, s)
			assertNoMismatch(t, s)
			if st := s.Status(); st.Phase != PhaseIdle {
				t.Errorf("after leave: %+v", st)
			}
		})
	}
}

func schemaOf(t *testing.T, line string) string {
	t.Helper()
	i := strings.Index(line, `"schema":"`)
	if i < 0 {
		t.Fatalf("no schema in %q", line)
	}
	rest := line[i+len(`"schema":"`):]
	return rest[:strings.Index(rest, `"`)]
}

// TestVoiceRemovedV2: a moderator's voice kick, which a v2 helper reports as
// failed with voice_removed, shows that reason, is not offered a retry, and
// starts nothing: no second helper and no new start.
func TestVoiceRemovedV2(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), Timings{}, "-versions", v1+","+v2, "-fail-with", string(schema.VoiceControlReasonVoiceRemoved))
	ctx := ctxFor(t)
	// The failure may already be known when Join returns.
	if err := s.Join(ctx, testChannel); err != nil && reasonOf(err) != "voice_removed" {
		t.Fatalf("join: %v", err)
	}
	waitFor(t, s, "voice_removed", func(st Status) bool {
		return st.Reason == string(schema.VoiceControlReasonVoiceRemoved) && st.Phase == PhaseFailed
	})
	waitReaped(t, s)
	st := s.Status()
	if st.Phase != PhaseFailed || st.Reason != "voice_removed" || st.Retryable || st.Protocol != v2 || st.Capture != schema.VoiceControlCaptureOff {
		t.Fatalf("status %+v, want failed with voice_removed, not retryable, in v2", st)
	}
	if err := s.Retry(ctx); !errors.Is(err, ErrNoRetry) {
		t.Errorf("retry after voice_removed: %v, want ErrNoRetry", err)
	}
	// Nothing starts again by itself.
	time.Sleep(300 * time.Millisecond)
	if now := s.Status(); now.Gen != 1 || s.current() != nil || now.Reason != "voice_removed" {
		t.Errorf("a helper was started again by itself: %+v", now)
	}
	assertNoMismatch(t, s)
}

// TestVoiceRemovedIsNotRetryable: the reason is not among those a new attempt
// could fix, so no view offers Retry for it.
func TestVoiceRemovedIsNotRetryable(t *testing.T) {
	for reason, want := range map[string]bool{
		string(schema.VoiceControlReasonVoiceRemoved):  false,
		string(schema.VoiceControlReasonInternalError): true,
	} {
		if got := Retryable(reason); got != want {
			t.Errorf("Retryable(%q) = %v, want %v", reason, got, want)
		}
	}
}

// TestTrackerChoosesVersion: the choice is v2 when offered, else v1, and
// nothing when neither is; after it, a line of the other version is a bad
// event, as is a v2 line before any choice.
func TestTrackerChoosesVersion(t *testing.T) {
	hello := func(versions ...string) string {
		var q []string
		for _, v := range versions {
			q = append(q, `"`+v+`"`)
		}
		return `{"schema":"` + v1 + `","kind":"hello","seq":1,"versions":[` + strings.Join(q, ",") + `],"helper_version":"t"}`
	}
	state2 := `{"schema":"` + v2 + `","kind":"state","seq":2,"phase":"failed","reason":"voice_removed","capture":"off","playback":"off",` +
		`"transmitting":false,"muted":false,"deafened":false,"blocked":["not_connected"],"talk_mode":"push_to_talk","ptt":"unknown"}`
	ack2 := `{"schema":"` + v2 + `","kind":"ack","seq":2,"id":1,"ok":true}`
	ack1 := `{"schema":"` + v1 + `","kind":"ack","seq":2,"id":1,"ok":true}`
	tests := []struct {
		name     string
		versions []string
		want     string
		ok       bool
		line     string // read after the start is written
		wantErr  bool
	}{
		{name: "both", versions: []string{v1, v2}, want: v2, ok: true, line: ack2},
		{name: "v1 only", versions: []string{v1}, want: v1, ok: true, line: ack1},
		{name: "v2 only", versions: []string{v2}, want: v2, ok: true, line: ack2},
		{name: "neither", versions: []string{"conch.voice_control.v3"}},
		{name: "v1 line in a v2 session", versions: []string{v1, v2}, want: v2, ok: true, line: ack1, wantErr: true},
		{name: "v2 line in a v1 session", versions: []string{v1}, want: v1, ok: true, line: ack2, wantErr: true},
		{name: "voice_removed in v1", versions: []string{v1}, want: v1, ok: true, line: state2, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &tracker{}
			if _, err := tr.read([]byte(hello(tt.versions...)), false); err != nil {
				t.Fatalf("hello: %v", err)
			}
			got, ok := tr.choose()
			if got != tt.want || ok != tt.ok {
				t.Fatalf("choose = %q, %v, want %q, %v", got, ok, tt.want, tt.ok)
			}
			if !ok {
				return
			}
			tr.written(1, schema.VoiceControlCommandStart)
			_, err := tr.read([]byte(tt.line), false)
			if (err != nil) != tt.wantErr {
				t.Errorf("read: %v, want error: %v", err, tt.wantErr)
			}
		})
	}
	t.Run("a v2 state keeps playback and talk mode", func(t *testing.T) {
		tr := &tracker{}
		if _, err := tr.read([]byte(hello(v1, v2)), false); err != nil {
			t.Fatal(err)
		}
		tr.choose()
		tr.written(1, schema.VoiceControlCommandStart)
		rd, err := tr.read([]byte(state2), false)
		if err != nil || rd.v2 == nil || rd.ev.State == nil {
			t.Fatalf("read: %+v, %v", rd, err)
		}
		st := fromStateV2(1, "c", "t", rd.v2)
		if st.Phase != PhaseFailed || st.Reason != "voice_removed" || st.Retryable || st.Playback != schema.VoiceControlPlaybackOff || st.TalkMode != schema.VoiceControlTalkModePushToTalk {
			t.Errorf("status %+v", st)
		}
	})
}
