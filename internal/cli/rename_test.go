package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// rename runs `conch rename <args> --server <url>` with token as CONCH_TOKEN.
func (e *adminEnv) rename(t *testing.T, token string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("CONCH_TOKEN", token)
	return runCLI(t, "", append(append([]string{"rename"}, args...), "--server", e.url)...)
}

// conch rename changes the caller's own name, against a real conchd: the
// member is principal 2.
func TestRenameSelf(t *testing.T) {
	e := newAdminEnv(t)
	stdout, stderr, err := e.rename(t, e.memberToken, "Mallory Quinn")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if want := "renamed principal 2 to Mallory Quinn\n"; stdout != want || stderr != "" {
		t.Errorf("stdout %q, stderr %q; want %q", stdout, stderr, want)
	}
	who, _, err := runCLI(t, "", "whoami", "--server", e.url)
	if err != nil || !strings.Contains(who, "name: Mallory Quinn\n") {
		t.Errorf("whoami after the rename = %q, %v", who, err)
	}

	// --json prints the response document.
	stdout, _, err = e.rename(t, e.memberToken, "--json", "mallory")
	if err != nil {
		t.Fatalf("rename --json: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	var resp schema.RenamePrincipalResponseV1
	if err := dec.Decode(&resp); err != nil || resp.Principal.ID != 2 || resp.Principal.Name != "mallory" {
		t.Errorf("--json = %q (%+v, %v)", stdout, resp.Principal, err)
	}
}

// A name that clashes with the operator's, by case or look-alike letters, is
// refused by the server with its code, on one line.
func TestRenameClash(t *testing.T) {
	e := newAdminEnv(t)
	for _, name := range []string{"ROOT", "r\u043e\u043et"} {
		stdout, _, err := e.rename(t, e.memberToken, name)
		if err == nil || !strings.Contains(err.Error(), "principal_exists") {
			t.Errorf("rename to %q = %v, want principal_exists", name, err)
		}
		if stdout != "" || strings.Contains(ErrorLine(err), "\n") {
			t.Errorf("stdout %q, error %q", stdout, err)
		}
	}
}

// Usage mistakes and names the server would refuse cost no request.
func TestRenameRefusedLocally(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(ts.Close)
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "conch_"+strings.Repeat("A", 43))
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no name", nil, "usage: conch rename"},
		{"two names", []string{"ann", "lee"}, "usage: conch rename"},
		{"blank", []string{"  "}, "must not be empty"},
		{"control character", []string{"a\x1bb"}, "control characters"},
		{"bidi override", []string{"a\u202eb"}, "bidi controls"},
		{"surrounding space", []string{" ann"}, "whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := runCLI(t, "", append(append([]string{"rename"}, tt.args...), "--server", ts.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q", stdout)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests reached the server", n)
	}
}

func TestUsageListsRename(t *testing.T) {
	var out strings.Builder
	Usage(&out)
	for _, want := range []string{"conch rename [--server <url>] [--json] <name>", "conch admin principal rename"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help lacks %q", want)
		}
	}
}
