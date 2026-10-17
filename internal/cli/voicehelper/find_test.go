package voicehelper

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestFind covers discovery (design note §10): CONCH_VOICE_HELPER, then
// beside conch, then PATH; the first candidate that exists is the one, and
// one that fails the trust checks stops the search.
func TestFind(t *testing.T) {
	type layout struct {
		configured string
		exe        string // the running conch
		path       string // PATH
		chdir      string
	}
	write := func(t *testing.T, path string, mode os.FileMode) string {
		t.Helper()
		if err := os.WriteFile(path, []byte("helper\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mkdir := func(t *testing.T, path string, mode os.FileMode) string {
		t.Helper()
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil { // #nosec G302 -- the directory modes under test
			t.Fatal(err)
		}
		return path
	}
	link := func(t *testing.T, target, at string) string {
		t.Helper()
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	// planted is a conch-voice in a directory anyone can write to, a link to
	// a root-owned program: what another local user could leave there.
	planted := func(t *testing.T, root string) string {
		shared := mkdir(t, filepath.Join(root, "shared"), 0o777)
		return link(t, rootProgram(t), filepath.Join(shared, HelperName))
	}
	tests := []struct {
		name   string
		setup  func(t *testing.T, root string) (layout, string)
		reason string
		inMsg  string
	}{
		{
			name: "CONCH_VOICE_HELPER, used exactly",
			setup: func(t *testing.T, root string) (layout, string) {
				p := write(t, filepath.Join(root, "custom-helper"), 0o700)
				return layout{configured: p}, p
			},
		},
		{
			name: "CONCH_VOICE_HELPER through a link: the file it names",
			setup: func(t *testing.T, root string) (layout, string) {
				p := write(t, filepath.Join(root, "real"), 0o755)
				link := filepath.Join(root, "link")
				if err := os.Symlink(p, link); err != nil {
					t.Fatal(err)
				}
				return layout{configured: link}, p
			},
		},
		{
			name: "a relative CONCH_VOICE_HELPER is refused, never resolved",
			setup: func(t *testing.T, root string) (layout, string) {
				write(t, filepath.Join(root, HelperName), 0o700)
				return layout{configured: HelperName, chdir: root}, ""
			},
			reason: ReasonHelperUntrusted, inMsg: HelperEnv,
		},
		{
			name: "a missing CONCH_VOICE_HELPER does not fall through",
			setup: func(t *testing.T, root string) (layout, string) {
				write(t, filepath.Join(root, HelperName), 0o700)
				return layout{configured: filepath.Join(root, "missing"), exe: filepath.Join(root, "conch")}, ""
			},
			reason: ReasonHelperNotFound, inMsg: HelperEnv,
		},
		{
			name: "white space is not set",
			setup: func(t *testing.T, root string) (layout, string) {
				p := write(t, filepath.Join(root, HelperName), 0o700)
				return layout{configured: " \t", exe: filepath.Join(root, "conch")}, p
			},
		},
		{
			name: "group-writable",
			setup: func(t *testing.T, root string) (layout, string) {
				return layout{configured: write(t, filepath.Join(root, "h"), 0o770)}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "writable by others",
			setup: func(t *testing.T, root string) (layout, string) {
				return layout{configured: write(t, filepath.Join(root, "h"), 0o707)}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "not executable",
			setup: func(t *testing.T, root string) (layout, string) {
				return layout{configured: write(t, filepath.Join(root, "h"), 0o600)}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a directory",
			setup: func(t *testing.T, root string) (layout, string) {
				dir := filepath.Join(root, "h")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				return layout{configured: dir}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a link to nothing",
			setup: func(t *testing.T, root string) (layout, string) {
				link := filepath.Join(root, "h")
				if err := os.Symlink(filepath.Join(root, "nothing"), link); err != nil {
					t.Fatal(err)
				}
				return layout{configured: link}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "in a directory others can write to",
			setup: func(t *testing.T, root string) (layout, string) {
				dir := filepath.Join(root, "shared")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o777); err != nil { // #nosec G302 -- the unsafe directory under test
					t.Fatal(err)
				}
				return layout{configured: write(t, filepath.Join(dir, "h"), 0o700)}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a link planted in a writable directory, through CONCH_VOICE_HELPER",
			setup: func(t *testing.T, root string) (layout, string) {
				return layout{configured: planted(t, root)}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a link planted in a writable directory, beside conch",
			setup: func(t *testing.T, root string) (layout, string) {
				p := planted(t, root)
				return layout{exe: filepath.Join(filepath.Dir(p), "conch"), path: filepath.Join(root, "nothing-here")}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a link planted in a writable directory, through PATH",
			setup: func(t *testing.T, root string) (layout, string) {
				p := planted(t, root)
				return layout{exe: filepath.Join(root, "empty", "conch"), path: filepath.Dir(p)}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a chain whose middle link sits in a writable directory",
			setup: func(t *testing.T, root string) (layout, string) {
				shared := mkdir(t, filepath.Join(root, "shared"), 0o777)
				real := write(t, filepath.Join(root, "real"), 0o700)
				middle := link(t, real, filepath.Join(shared, "middle"))
				return layout{configured: link(t, middle, filepath.Join(root, HelperName))}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a link to a directory others can write to",
			setup: func(t *testing.T, root string) (layout, string) {
				shared := mkdir(t, filepath.Join(root, "shared"), 0o777)
				write(t, filepath.Join(shared, HelperName), 0o700)
				via := link(t, shared, filepath.Join(root, "via"))
				return layout{configured: filepath.Join(via, HelperName)}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a parent others can write to",
			setup: func(t *testing.T, root string) (layout, string) {
				open := mkdir(t, filepath.Join(root, "open"), 0o777)
				inner := mkdir(t, filepath.Join(open, "inner"), 0o700)
				return layout{configured: write(t, filepath.Join(inner, HelperName), 0o700)}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a link loop",
			setup: func(t *testing.T, root string) (layout, string) {
				link(t, filepath.Join(root, "b"), filepath.Join(root, "a"))
				link(t, filepath.Join(root, "a"), filepath.Join(root, "b"))
				return layout{configured: filepath.Join(root, "a")}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "a 0755 directory of yours",
			setup: func(t *testing.T, root string) (layout, string) {
				bin := mkdir(t, filepath.Join(root, "bin"), 0o755)
				p := write(t, filepath.Join(bin, HelperName), 0o755)
				return layout{configured: p}, p
			},
		},
		{
			name: "a root-owned program through a safe link",
			setup: func(t *testing.T, root string) (layout, string) {
				target := rootProgram(t)
				return layout{configured: link(t, target, filepath.Join(root, HelperName))}, target
			},
		},
		{
			name: "a relative link, through a parent that is a link",
			setup: func(t *testing.T, root string) (layout, string) {
				bin := mkdir(t, filepath.Join(root, "bin"), 0o700)
				real := write(t, filepath.Join(bin, "conch-voice-1"), 0o700)
				link(t, "conch-voice-1", filepath.Join(bin, HelperName))
				via := link(t, "bin", filepath.Join(root, "via"))
				return layout{configured: filepath.Join(via, ".", HelperName)}, real
			},
		},
		{
			name: "beside conch",
			setup: func(t *testing.T, root string) (layout, string) {
				p := write(t, filepath.Join(root, HelperName), 0o755)
				return layout{exe: filepath.Join(root, "conch"), path: filepath.Join(root, "nowhere")}, p
			},
		},
		{
			name: "an untrusted one beside conch does not fall through to PATH",
			setup: func(t *testing.T, root string) (layout, string) {
				write(t, filepath.Join(root, HelperName), 0o775)
				bin := filepath.Join(root, "bin")
				if err := os.Mkdir(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(bin, HelperName), 0o700)
				return layout{exe: filepath.Join(root, "conch"), path: bin}, ""
			},
			reason: ReasonHelperUntrusted,
		},
		{
			name: "PATH",
			setup: func(t *testing.T, root string) (layout, string) {
				bin := filepath.Join(root, "bin")
				if err := os.Mkdir(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				p := write(t, filepath.Join(bin, HelperName), 0o700)
				return layout{exe: filepath.Join(root, "empty", "conch"), path: filepath.Join(root, "nothing-here") + string(os.PathListSeparator) + bin}, p
			},
		},
		{
			name: "PATH through '.' is not found",
			setup: func(t *testing.T, root string) (layout, string) {
				write(t, filepath.Join(root, HelperName), 0o700)
				return layout{exe: filepath.Join(root, "empty", "conch"), path: ".", chdir: root}, ""
			},
			reason: ReasonHelperNotFound,
		},
		{
			name: "PATH through an empty entry is not found",
			setup: func(t *testing.T, root string) (layout, string) {
				write(t, filepath.Join(root, HelperName), 0o700)
				return layout{exe: filepath.Join(root, "empty", "conch"), path: string(os.PathListSeparator), chdir: root}, ""
			},
			reason: ReasonHelperNotFound,
		},
		{
			name: "nowhere",
			setup: func(t *testing.T, root string) (layout, string) {
				return layout{exe: filepath.Join(root, "conch"), path: filepath.Join(root, "nothing-here")}, ""
			},
			reason: ReasonHelperNotFound, inMsg: HelperEnv,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := privateDir(t)
			if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
				t.Fatal(err)
			}
			l, want := tt.setup(t, root)
			t.Setenv("PATH", l.path)
			if l.chdir != "" {
				t.Chdir(l.chdir)
			}
			f := finder{
				executable: func() (string, error) {
					if l.exe == "" {
						return "", errors.New("no executable")
					}
					return l.exe, nil
				},
				lookPath: exec.LookPath,
			}
			got, err := f.find(l.configured)
			if tt.reason != "" {
				if reasonOf(err) != tt.reason || got != "" {
					t.Fatalf("find = %q, %v; want %s", got, err, tt.reason)
				}
				if !strings.Contains(err.Error(), tt.inMsg) {
					t.Errorf("%v does not name %s", err, tt.inMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("find: %v", err)
			}
			resolved, err := filepath.EvalSymlinks(want)
			if err != nil {
				t.Fatal(err)
			}
			if got != resolved {
				t.Fatalf("find = %q, want %q", got, resolved)
			}
		})
	}
}

// rootProgram is a root-owned program on a path the trust checks accept, the
// way a packaged conch-voice in /usr/bin would be.
func rootProgram(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/env", "/bin/env"} {
		got, err := trusted(p)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(got); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == 0 {
				return got
			}
		}
	}
	t.Skip("no root-owned program on a safe path on this machine (/usr/bin/env)")
	return ""
}

func TestHelperPathFromEnv(t *testing.T) {
	for _, v := range []string{"", "/opt/conch/conch-voice", "relative"} {
		t.Setenv(HelperEnv, v)
		if got := HelperPathFromEnv(); got != v {
			t.Errorf("HelperPathFromEnv() = %q, want %q exactly", got, v)
		}
	}
}

func TestOwnerTrusted(t *testing.T) {
	tests := []struct {
		uid  uint32
		euid int
		want bool
	}{
		{uid: 1000, euid: 1000, want: true},
		{uid: 0, euid: 1000, want: true},
		{uid: 0, euid: 0, want: true},
		{uid: 1001, euid: 1000, want: false},
		{uid: 1000, euid: 0, want: false},
		{uid: 1000, euid: -1, want: false},
	}
	for _, tt := range tests {
		if got := ownerTrusted(tt.uid, tt.euid); got != tt.want {
			t.Errorf("ownerTrusted(%d, %d) = %v, want %v", tt.uid, tt.euid, got, tt.want)
		}
	}
}
