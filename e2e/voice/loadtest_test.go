package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeJWT is a token shaped as lk signs one, naming participant sub. Its
// signature is not checked by anything here.
func fakeJWT(sub string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(`{"sub":"`+sub+`","video":{"room":"r"}}`)) + ".c2ln"
}

func TestParseLoadtestFlags(t *testing.T) {
	base := []string{"-server", "https://conch.example.org/", "-prefix", "loadtest-a1", "-channel", "movie", "-viewers", "29", "-ready", "/tmp/r"}
	with := func(extra ...string) []string { return append(append([]string(nil), base...), extra...) }
	tests := []struct {
		name    string
		args    []string
		wantErr string
		check   func(t *testing.T, c loadtestConfig)
	}{
		{name: "a run", args: base, check: func(t *testing.T, c loadtestConfig) {
			if c.server != "https://conch.example.org" || c.viewers != 29 || c.lifetime != 4*time.Hour || c.fakeSharer {
				t.Errorf("config = %+v", c)
			}
		}},
		{name: "fake sharer with lk", args: with("-fake-sharer", "-lk", "/x/lk"), check: func(t *testing.T, c loadtestConfig) {
			if !c.fakeSharer || c.lkBin != "/x/lk" {
				t.Errorf("config = %+v", c)
			}
		}},
		{name: "clean-up needs no channel", args: []string{"-server", "http://127.0.0.1:1", "-prefix", "loadtest-a1", "-cleanup-only"}},
		{name: "fake sharer without lk", args: with("-fake-sharer"), wantErr: "-fake-sharer needs -lk"},
		{name: "no server", args: []string{"-prefix", "loadtest-a1", "-cleanup-only"}, wantErr: "-server"},
		{name: "server not http", args: []string{"-server", "ws://h", "-prefix", "loadtest-a1", "-cleanup-only"}, wantErr: "-server"},
		{name: "prefix that is not a load test's", args: []string{"-server", "http://h", "-prefix", "ali", "-cleanup-only"}, wantErr: "-prefix"},
		{name: "prefix with capitals", args: []string{"-server", "http://h", "-prefix", "loadtest-Alice", "-cleanup-only"}, wantErr: "-prefix"},
		{name: "prefix that is only loadtest-", args: []string{"-server", "http://h", "-prefix", "loadtest-", "-cleanup-only"}, wantErr: "-prefix"},
		{name: "no channel", args: []string{"-server", "http://h", "-prefix", "loadtest-a1", "-viewers", "2", "-ready", "/r"}, wantErr: "-channel"},
		{name: "no viewers", args: with("-viewers", "0"), wantErr: "-viewers"},
		{name: "the most viewers", args: with("-viewers", "499")},
		{name: "more than one hide's worth", args: with("-viewers", "500"), wantErr: "-viewers"},
		{name: "a token as an argument", args: with("-token", "x"), wantErr: "flag provided but not defined"},
		{name: "a stray argument", args: with("extra"), wantErr: "unexpected argument"},
		{name: "a short credential lifetime", args: with("-credential-lifetime", "30s"), wantErr: "-credential-lifetime"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseLoadtestFlags(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestLKIdentity(t *testing.T) {
	tests := []struct {
		name, token, want string
		wantErr           bool
	}{
		{name: "lk's token", token: fakeJWT("lt_sub_3"), want: "lt_sub_3"},
		{name: "no token", token: "", wantErr: true},
		{name: "two parts", token: "a.b", wantErr: true},
		{name: "payload not base64", token: "a.!!!.c", wantErr: true},
		{name: "no sub", token: "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"k"}`)) + ".c", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lkIdentity(tt.token)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("lkIdentity = %q, %v; want %q, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestLivekitUpstream(t *testing.T) {
	tests := []struct {
		raw, host string
		secure    bool
		wantErr   bool
	}{
		{raw: "ws://127.0.0.1:7880", host: "127.0.0.1:7880"},
		{raw: "wss://lk.example.org", host: "lk.example.org", secure: true},
		{raw: "wss://lk.example.org/", host: "lk.example.org", secure: true},
		{raw: "https://lk.example.org:7443", host: "lk.example.org:7443", secure: true},
		{raw: "wss://example.org/livekit", wantErr: true},
		{raw: "ftp://lk.example.org", wantErr: true},
		{raw: "lk.example.org", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			host, secure, err := livekitUpstream(tt.raw)
			if (err != nil) != tt.wantErr || host != tt.host || secure != tt.secure {
				t.Fatalf("livekitUpstream = %q, %v, %v; want %q, %v, error %v", host, secure, err, tt.host, tt.secure, tt.wantErr)
			}
		})
	}
}

func TestClientToken(t *testing.T) {
	tests := []struct {
		name, url, auth, want string
	}{
		{name: "query", url: "/rtc?access_token=abc&sdk=go", want: "abc"},
		{name: "header", url: "/rtc", auth: "Bearer xyz", want: "xyz"},
		{name: "query first", url: "/rtc?access_token=abc", auth: "Bearer xyz", want: "abc"},
		{name: "none", url: "/rtc", want: ""},
		{name: "not bearer", url: "/rtc", auth: "Basic xyz", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			if got := clientToken(req); got != tt.want {
				t.Fatalf("clientToken = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLoadtestAssign: each of lk's participants gets a viewer of its own, the
// same one when it connects again, and one participant too many is refused.
func TestLoadtestAssign(t *testing.T) {
	lt := &loadtest{viewers: []*person{{name: "v1"}, {name: "v2"}}, byLK: map[string]*person{}}
	steps := []struct {
		lk, want string
		wantErr  bool
	}{
		{lk: "sub_0", want: "v1"},
		{lk: "sub_1", want: "v2"},
		{lk: "sub_0", want: "v1"},
		{lk: "sub_2", wantErr: true},
		{lk: "sub_1", want: "v2"},
	}
	for i, s := range steps {
		v, err := lt.assign(s.lk)
		if s.wantErr {
			if err == nil {
				t.Fatalf("step %d: %s was given %s, want a refusal", i, s.lk, v.name)
			}
			continue
		}
		if err != nil || v.name != s.want {
			t.Fatalf("step %d: assign(%s) = %v, %v; want %s", i, s.lk, v, err, s.want)
		}
	}
}

// TestMintingRelay: through one relay, each connection reaches LiveKit with
// the token minted for the token its client presented, in the query and in
// the Authorization header, and a refused mint never reaches LiveKit.
func TestMintingRelay(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Query().Get("access_token")+"|"+r.Header.Get("Authorization"))
		mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer upstream.Close()

	mint := func(_ context.Context, lkToken string) (string, error) {
		id, err := lkIdentity(lkToken)
		if err != nil {
			return "", err
		}
		if id == "refused" {
			return "", errors.New("no viewer left")
		}
		return "conchd-" + id, nil
	}
	rl, err := serveRelay(&relay{upstream: strings.TrimPrefix(upstream.URL, "http://"), mint: mint})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range []string{"a", "b"} {
		hdr := http.Header{"Authorization": {"Bearer " + fakeJWT(id)}}
		c, _, err := websocket.Dial(ctx, rl.url()+"/rtc?access_token="+fakeJWT(id), &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			t.Fatalf("dial as %s: %v", id, err)
		}
		_, _, _ = c.Read(ctx)
		_ = c.CloseNow()
	}
	_, resp, err := websocket.Dial(ctx, rl.url()+"/rtc?access_token="+fakeJWT("refused"), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a refused mint: err %v, response %v; want 503", err, resp)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"conchd-a|Bearer conchd-a", "conchd-b|Bearer conchd-b"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("LiveKit saw %q, want %q", seen, want)
	}
}
