package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fileServer is a conchd stand-in that serves one file's metadata and bytes,
// both under the test's control, so the client can be given a hostile name, a
// wrong hash or a body of the wrong length.
func fileServer(t *testing.T, meta schema.FileV1, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/files/7/meta":
			_ = json.NewEncoder(w).Encode(meta)
		case "/v2/files/7":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"file_not_found","message":"file not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// inTempDir runs the test in a fresh working directory that sits one level
// below a parent the test can inspect for anything written outside it.
func inTempDir(t *testing.T) (work, parent string) {
	t.Helper()
	parent = t.TempDir()
	work = filepath.Join(parent, "work")
	if err := os.Mkdir(work, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	t.Setenv("CONCH_TOKEN", "test-token")
	return work, parent
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func goodMeta(name string, body []byte) schema.FileV1 {
	return schema.FileV1{Schema: schema.FileSchemaV1, ID: 7, ChannelID: 1, UploaderID: 2, Filename: name,
		MediaType: "text/plain", Size: int64(len(body)), SHA256: sha(body), CreatedAt: schema.NewTimestamp(time.Unix(1, 0))}
}

// A server chooses the stored name. Whatever it sends, a download without -o
// writes inside the current directory or refuses, and nothing appears anywhere
// else.
func TestDownloadHostileStoredNames(t *testing.T) {
	body := []byte("payload")
	tests := []struct {
		name     string
		filename string
		wantFile string // written in the current directory, or "" for a refusal
	}{
		{"plain name", "report.txt", "report.txt"},
		{"parent traversal", "../../x", ""},
		{"one level up", "../x", ""},
		{"absolute path", "/etc/passwd", ""},
		{"absolute path to a temp file", filepath.Join(os.TempDir(), "conch-escape"), ""},
		{"subdirectory", "sub/x", ""},
		{"backslash", `..\..\x`, ""},
		{"drive letter", `C:\x`, ""},
		{"NUL", "a\x00b", ""},
		{"newline", "a\nb", ""},
		{"escape sequence", "a\x1b[31mb", ""},
		{"dot", ".", ""},
		{"dotdot", "..", ""},
		{"empty", "", ""},
		{"256 bytes", strings.Repeat("a", 256), ""},
		// JSON turns the invalid byte into U+FFFD on the way, so what the
		// client sees is a valid name that stays inside the directory.
		{"invalid UTF-8", "a\xffb", "a�b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work, parent := inTempDir(t)
			srv := fileServer(t, goodMeta(tt.filename, body), body)
			var out, errOut bytes.Buffer
			err := Run(context.Background(), []string{"download", "--server", srv.URL, "7"}, &out, &errOut, "test")
			if tt.wantFile == "" {
				if err == nil {
					t.Fatalf("download of a file named %q succeeded", tt.filename)
				}
				if got := listNames(t, work); len(got) != 0 {
					t.Errorf("files in the working directory after a refusal: %v", got)
				}
			} else {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				got, rerr := os.ReadFile(filepath.Join(work, tt.wantFile)) // #nosec G304 -- a test path
				if rerr != nil || !bytes.Equal(got, body) {
					t.Errorf("%s = %q, %v", tt.wantFile, got, rerr)
				}
			}
			if got := listNames(t, parent); len(got) != 1 || got[0] != "work" {
				t.Errorf("something was created beside the working directory: %v", got)
			}
			if _, serr := os.Stat(filepath.Join(os.TempDir(), "conch-escape")); serr == nil {
				t.Error("a file was created at an absolute path the server named")
			}
		})
	}
}

func TestDownloadBehaviour(t *testing.T) {
	body := []byte("the bytes")
	t.Run("-o writes there and the name is not needed", func(t *testing.T) {
		work, _ := inTempDir(t)
		srv := fileServer(t, goodMeta("../../evil", body), body)
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), []string{"download", "--server", srv.URL, "-o", "mine.bin", "7"}, &out, &errOut, "test"); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(work, "mine.bin")); !bytes.Equal(got, body) { // #nosec G304 -- a test path
			t.Errorf("mine.bin = %q", got)
		}
		if strings.TrimSpace(out.String()) != "mine.bin" {
			t.Errorf("stdout = %q", out.String())
		}
	})
	t.Run("an existing file is never overwritten", func(t *testing.T) {
		work, _ := inTempDir(t)
		srv := fileServer(t, goodMeta("report.txt", body), body)
		for _, args := range [][]string{{"7"}, {"-o", "report.txt", "7"}} {
			if err := os.WriteFile(filepath.Join(work, "report.txt"), []byte("precious"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			err := Run(context.Background(), append([]string{"download", "--server", srv.URL}, args...), &out, &errOut, "test")
			if err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Errorf("args %v: err = %v", args, err)
			}
			if got, _ := os.ReadFile(filepath.Join(work, "report.txt")); string(got) != "precious" { // #nosec G304 -- a test path
				t.Errorf("args %v: the existing file now holds %q", args, got)
			}
		}
	})
	t.Run("a symbolic link in the way is not followed", func(t *testing.T) {
		work, parent := inTempDir(t)
		target := filepath.Join(parent, "target")
		if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(work, "report.txt")); err != nil {
			t.Skip("symbolic links are not available: ", err)
		}
		srv := fileServer(t, goodMeta("report.txt", body), body)
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), []string{"download", "--server", srv.URL, "7"}, &out, &errOut, "test"); err == nil {
			t.Fatal("download through a symbolic link succeeded")
		}
		if got, _ := os.ReadFile(target); string(got) != "precious" { // #nosec G304 -- a test path
			t.Errorf("the link's target now holds %q", got)
		}
	})
	tests := []struct {
		name string
		meta func() schema.FileV1
		body []byte
		want string
	}{
		{"hash mismatch", func() schema.FileV1 { m := goodMeta("a.txt", body); m.SHA256 = sha([]byte("other")); return m }, body, "sha256"},
		{"body longer than the size", func() schema.FileV1 { return goodMeta("a.txt", body) }, append(append([]byte{}, body...), "extra"...), "more bytes"},
		{"body shorter than the size", func() schema.FileV1 { return goodMeta("a.txt", body) }, body[:3], "3 bytes of"},
		{"negative size", func() schema.FileV1 { m := goodMeta("a.txt", body); m.Size = -1; return m }, body, "unusable metadata"},
		{"malformed hash", func() schema.FileV1 { m := goodMeta("a.txt", body); m.SHA256 = "zz"; return m }, body, "unusable metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work, _ := inTempDir(t)
			srv := fileServer(t, tt.meta(), tt.body)
			var out, errOut bytes.Buffer
			err := Run(context.Background(), []string{"download", "--server", srv.URL, "7"}, &out, &errOut, "test")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if got := listNames(t, work); len(got) != 0 {
				t.Errorf("a failed download left %v", got)
			}
		})
	}
	t.Run("an unknown file is the server's refusal", func(t *testing.T) {
		work, _ := inTempDir(t)
		srv := fileServer(t, goodMeta("a.txt", body), body)
		var out, errOut bytes.Buffer
		err := Run(context.Background(), []string{"download", "--server", srv.URL, "8"}, &out, &errOut, "test")
		if err == nil || !strings.Contains(err.Error(), "file_not_found") {
			t.Errorf("err = %v", err)
		}
		if got := listNames(t, work); len(got) != 0 {
			t.Errorf("left %v", got)
		}
	})
	for _, args := range [][]string{{}, {"abc"}, {"0"}, {"-1"}, {"1", "2"}, {"-o", "", "1"}} {
		inTempDir(t)
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), append([]string{"download", "--server", "http://127.0.0.1:1"}, args...), &out, &errOut, "test"); err == nil {
			t.Errorf("args %v were accepted", args)
		}
	}
}

func TestUploadArguments(t *testing.T) {
	inTempDir(t)
	if err := os.WriteFile("bad\nname", []byte("x"), 0o600); err != nil {
		t.Skip("cannot create a file with a newline in its name: ", err)
	}
	if err := os.WriteFile("ok.txt", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("dir", 0o750); err != nil {
		t.Fatal(err)
	}
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	for _, args := range [][]string{
		{}, {"ops"}, {"ops", "missing.txt"}, {"ops", "dir"}, {"ops", "bad\nname"}, {"ops", "ok.txt", "extra"},
	} {
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), append([]string{"upload", "--server", srv.URL}, args...), &out, &errOut, "test"); err == nil {
			t.Errorf("args %v were accepted", args)
		}
	}
	if called {
		t.Error("a refused upload still called the server")
	}
}

func TestHelpMentionsFiles(t *testing.T) {
	var out bytes.Buffer
	Usage(&out)
	for _, want := range []string{"conch upload", "conch download", "never overwrites"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help does not mention %q", want)
		}
	}
}
