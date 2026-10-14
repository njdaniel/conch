//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil { //nolint:gosec // the test needs a chosen mode
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { //nolint:gosec // the test needs a chosen mode
		t.Fatal(err)
	}
}

// TestCredentialsWriteLandsInCheckedDirectory: CONCH_CONFIG_DIR with a symlink
// followed by ".." names a different directory to the kernel than to
// filepath.Clean. The directory that is checked must be the one written to.
func TestCredentialsWriteLandsInCheckedDirectory(t *testing.T) {
	isolateConfig(t)
	base := t.TempDir()
	mkdirMode(t, filepath.Join(base, "deep", "real"), 0o700)
	if err := os.Symlink(filepath.Join(base, "deep", "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	cleaned := filepath.Join(base, "shared") // what Clean makes of link/../shared
	resolved := filepath.Join(base, "deep", "shared")
	t.Setenv(configDirEnv, base+"/link/../shared")

	t.Run("a 0755 target is refused and nothing is written", func(t *testing.T) {
		mkdirMode(t, cleaned, 0o700)
		mkdirMode(t, resolved, 0o755)
		err := SaveToken("http://a:1", "tok")
		if err == nil || !strings.Contains(err.Error(), configDirEnv) {
			t.Fatalf("SaveToken = %v, want a refusal naming %s", err, configDirEnv)
		}
		for _, dir := range []string{cleaned, resolved} {
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("%s holds %d entries, want none", dir, len(entries))
			}
		}
	})
	t.Run("a private target gets the file, the cleaned one does not", func(t *testing.T) {
		mkdirMode(t, resolved, 0o700)
		if err := SaveToken("http://a:1", "tok"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(resolved, credentialsFileName)); err != nil {
			t.Errorf("the file is not in the checked directory: %v", err)
		}
		if _, err := os.Stat(filepath.Join(cleaned, credentialsFileName)); !os.IsNotExist(err) {
			t.Errorf("the file landed in the cleaned path's directory (%v)", err)
		}
		if tok, err := LoadToken("http://a:1"); err != nil || tok != "tok" {
			t.Errorf("LoadToken = %q, %v; the reader and the writer must agree", tok, err)
		}
	})
}

// TestCredentialsFIFOFileDoesNotHang: a FIFO named credentials.json must be
// refused promptly, not waited on.
func TestCredentialsFIFOFileDoesNotHang(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)
	if err := syscall.Mkfifo(filepath.Join(dir, credentialsFileName), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := LoadToken("http://a:1")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("LoadToken = %v, want a refusal: not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadToken hung on a FIFO")
	}
}

// TestCredentialsSymlinkFileRefused: a symlink at credentials.json is not
// something conch wrote (a rename replaces the leaf), so it is refused, and
// nothing is written through it.
func TestCredentialsSymlinkFileRefused(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, credentialsFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken("http://a:1"); err == nil {
		t.Error("LoadToken followed a symlink at the leaf")
	}
	if err := SaveToken("http://a:1", "tok"); err == nil {
		t.Error("SaveToken accepted a symlink at the leaf")
	}
	if got, _ := os.ReadFile(target); string(got) != "{}" { //nolint:gosec // test-local path
		t.Errorf("the symlink's target was written: %q", got)
	}
}

// TestCredentialsErrorsAreOneLine: CONCH_CONFIG_DIR may hold a newline, and
// errors repeat the path; none may span two lines.
func TestCredentialsErrorsAreOneLine(t *testing.T) {
	isolateConfig(t)
	dir := filepath.Join(t.TempDir(), "evil\nname\x1b[31m")
	mkdirMode(t, dir, 0o700)
	t.Setenv(configDirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, credentialsFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadToken("http://a:1")
	if err == nil {
		t.Fatal("LoadToken accepted a malformed file")
	}
	if strings.ContainsAny(err.Error(), "\n\x1b") || !strings.Contains(err.Error(), `\n`) {
		t.Errorf("error = %q, want one line with the control characters escaped", err.Error())
	}
}
