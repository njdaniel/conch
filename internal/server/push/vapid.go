package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// KeyFileName is the name, in the data directory, of the file holding the
// server's VAPID private key (RFC 8292): a PEM "PRIVATE KEY" block of a
// PKCS#8 P-256 key.
const KeyFileName = "push-vapid-key.pem"

// maxKeyFileBytes bounds what is read of a key file. A P-256 key in PEM is
// under 250 bytes; anything much larger is not one.
const maxKeyFileBytes = 8 << 10

// VAPIDKey is the server's application server key pair (RFC 8292 §3.2). A Web
// Push subscription is made under its public half, so the key is for as long as
// the registrations made under it: losing or replacing it ends every one of
// them (store.ReconcilePushVAPIDKey).
type VAPIDKey struct {
	private *ecdsa.PrivateKey
	public  []byte
}

// LoadOrCreateVAPIDKey returns the VAPID key in dataDir/KeyFileName, creating
// the file if it does not exist. It creates with O_EXCL and mode 0600, so it
// never overwrites a key, and two conchd started at once cannot each make one:
// the one that loses the race loads the winner's. If a write fails the partial
// file is removed, so the next start does not find half a key.
//
// It refuses, with an error that names the file and never any of its contents,
// a file that cannot be read, is not a regular file, is readable or writable
// by group or others (a private key anyone on the host can read is not a
// secret), is not one PEM block holding a PKCS#8 key, or is a key on another
// curve than P-256.
func LoadOrCreateVAPIDKey(dataDir string) (*VAPIDKey, error) {
	path := filepath.Join(dataDir, KeyFileName)
	key, err := loadVAPIDKey(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := createVAPIDKeyFile(path); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return nil, err
			}
			// Lost a race with another start: use the file it made.
		}
		key, err = loadVAPIDKey(path)
	}
	if err != nil {
		return nil, err
	}
	return key, nil
}

func loadVAPIDKey(path string) (*VAPIDKey, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator's data directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("push: VAPID key file %s cannot be opened: %w", path, errors.Unwrap(err))
	}
	defer func() { _ = f.Close() }()
	// The checks are on the open file, so the file checked is the file read.
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("push: VAPID key file %s cannot be examined", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("push: VAPID key file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("push: VAPID key file %s is accessible to group or others (mode %04o); run chmod 600 on it, or delete it to make a new key, which ends every Web Push registration", path, perm)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("push: VAPID key file %s cannot be read", path)
	}
	if len(raw) > maxKeyFileBytes {
		return nil, fmt.Errorf("push: VAPID key file %s is too large to be a P-256 key", path)
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 {
		return nil, fmt.Errorf("push: VAPID key file %s is not a PKCS#8 PEM private key", path)
	}
	for _, b := range rest {
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return nil, fmt.Errorf("push: VAPID key file %s has data after its key", path)
		}
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("push: VAPID key file %s does not hold a PKCS#8 private key", path)
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("push: VAPID key file %s is not a P-256 key", path)
	}
	pub, err := priv.PublicKey.ECDH()
	if err != nil {
		return nil, fmt.Errorf("push: VAPID key file %s is not a P-256 key", path)
	}
	return &VAPIDKey{private: priv, public: pub.Bytes()}, nil
}

func createVAPIDKeyFile(path string) (err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("push: generate VAPID key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("push: encode VAPID key: %w", err)
	}
	// O_EXCL: this never replaces a key, whatever the file is. 0600 is the
	// mode asked for; the umask can only narrow it.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator's data directory
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return err
		}
		return fmt.Errorf("push: VAPID key file %s cannot be created: %w", path, errors.Unwrap(err))
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		return fmt.Errorf("push: VAPID key file %s cannot be written", path)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("push: VAPID key file %s cannot be written", path)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("push: VAPID key file %s cannot be written", path)
	}
	return nil
}

// PublicKey is the public key as an app passes it when it subscribes (RFC 8292
// §3.2): unpadded base64url of the 65-byte uncompressed P-256 point. It is
// PushConfigV1.VAPIDPublicKey.
func (k *VAPIDKey) PublicKey() string {
	return base64.RawURLEncoding.EncodeToString(k.public)
}

// Fingerprint identifies the key without being it: the SHA-256 of the public
// point, in hex. The store keeps it to notice that the key was replaced.
func (k *VAPIDKey) Fingerprint() string {
	sum := sha256.Sum256(k.public)
	return hex.EncodeToString(sum[:])
}

// Private is the private key, for signing the VAPID JWT of a send (#1715). It
// is never logged, served or stored anywhere but its file.
func (k *VAPIDKey) Private() *ecdsa.PrivateKey { return k.private }
