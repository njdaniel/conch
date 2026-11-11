package push

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func pemOf(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestVAPIDKeyCreateAndReuse(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateVAPIDKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, KeyFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	// The public key is what PushConfigV1 serves, and it validates.
	cfg := schema.PushConfigV1{Transports: []schema.PushTransport{schema.PushTransportWebPush}, VAPIDPublicKey: first.PublicKey()}
	if err := cfg.Validate(); err != nil {
		t.Errorf("public key does not validate: %v", err)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(first.PublicKey()); err != nil || len(raw) != schema.PushP256DHBytes || raw[0] != 4 {
		t.Errorf("public key is not an uncompressed point: %v", err)
	}
	before, _ := os.ReadFile(path) // #nosec G304 -- a test file
	second, err := LoadOrCreateVAPIDKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path) // #nosec G304 -- a test file
	if string(before) != string(after) || second.PublicKey() != first.PublicKey() || second.Fingerprint() != first.Fingerprint() {
		t.Error("a second start did not reuse the key")
	}
	if first.Private() == nil || first.Fingerprint() == "" {
		t.Error("no private key or fingerprint")
	}
}

func TestVAPIDKeyRefusals(t *testing.T) {
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good := pemOf(t, p256)
	tests := []struct {
		name    string
		content []byte
		mode    os.FileMode
		wantErr string // "" means it loads
	}{
		{"a P-256 key", good, 0o600, ""},
		{"read-only to its owner", good, 0o400, ""},
		{"group readable", good, 0o640, "group or others"},
		{"world readable", good, 0o644, "group or others"},
		{"group writable", good, 0o620, "group or others"},
		{"P-384", pemOf(t, p384), 0o600, "not a P-256 key"},
		{"Ed25519", pemOf(t, ed), 0o600, "not a P-256 key"},
		{"not PEM", []byte("this is not a key"), 0o600, "PKCS#8 PEM"},
		{"empty", nil, 0o600, "PKCS#8 PEM"},
		{"a certificate block", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}), 0o600, "PKCS#8 PEM"},
		{"garbage in the block", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}), 0o600, "PKCS#8"},
		{"a second block", append(append([]byte{}, good...), good...), 0o600, "after its key"},
		{"too large", []byte(strings.Repeat("A", maxKeyFileBytes+10)), 0o600, "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, KeyFileName)
			if err := os.WriteFile(path, tt.content, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			key, err := LoadOrCreateVAPIDKey(dir)
			if tt.wantErr == "" {
				if err != nil || key == nil {
					t.Fatalf("load = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
			// It names the file and shows none of its contents.
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error does not name the file: %v", err)
			}
			if len(tt.content) > 0 && strings.Contains(err.Error(), strings.TrimSpace(string(tt.content[:min(40, len(tt.content))]))) {
				t.Errorf("error holds the file's content: %v", err)
			}
			// A refused file is left as it was.
			if got, _ := os.ReadFile(path); string(got) != string(tt.content) { // #nosec G304 -- a test file
				t.Error("a refusal changed the file")
			}
		})
	}

	t.Run("a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, KeyFileName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateVAPIDKey(dir); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("an unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads everything")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, KeyFileName)
		if err := os.WriteFile(path, good, 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateVAPIDKey(dir); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("a data directory that cannot be written", func(t *testing.T) {
		if _, err := LoadOrCreateVAPIDKey(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Error("created a key in a directory that does not exist")
		}
	})
}
