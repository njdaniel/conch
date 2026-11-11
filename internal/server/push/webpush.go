package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

// Web Push message encryption: RFC 8291 over the "aes128gcm" content coding
// of RFC 8188, with the standard library alone (crypto/ecdh, crypto/hkdf,
// crypto/aes, crypto/cipher). One wake is one record: a fresh 16-byte salt and
// a fresh ephemeral P-256 key are made for every message, so no key and no
// nonce is ever used twice.

const (
	// authSecretLen is the length of a subscription's auth secret (RFC 8291
	// section 3.2).
	authSecretLen = 16
	// saltLen is the length of the RFC 8188 salt.
	saltLen = 16
	// uncompressedPointLen is an uncompressed P-256 point: 0x04 || X || Y.
	uncompressedPointLen = 65
	// recordSize is the RFC 8188 record size written in the header. It
	// is 4096, the smallest maximum every push service accepts for a body;
	// a wake's one record is far shorter.
	recordSize = 4096
	// lastRecordDelimiter ends the last (here the only) record's plaintext
	// (RFC 8188 section 2): 0x02, with no padding after it, because every
	// wake has the same length already.
	lastRecordDelimiter = 0x02
)

// errEncrypt is the whole of what an encryption failure says: it never echoes
// a key, however the failure came about.
var errEncrypt = errors.New("push: cannot encrypt the wake for this subscription")

// Encrypt returns the body of a Web Push request that carries plaintext to
// the subscription whose public key is p256dh (the 65-byte uncompressed point
// a browser gives) and whose auth secret is auth (16 bytes), as RFC 8291
// section 3 specifies: the aes128gcm content coding, one record, a fresh salt
// and a fresh ephemeral key from crypto/rand. It refuses a key that is not a
// valid P-256 point (RFC 8291 section 7: a point not on the curve could leak
// the private key) and an auth secret that is not 16 bytes.
func Encrypt(plaintext, p256dh, auth []byte) ([]byte, error) {
	return encryptWith(rand.Reader, plaintext, p256dh, auth)
}

// encryptWith is Encrypt with the source of randomness for the ephemeral key
// and the salt made a parameter, so a test can reproduce a published vector.
func encryptWith(random io.Reader, plaintext, p256dh, auth []byte) ([]byte, error) {
	asPriv, err := ecdh.P256().GenerateKey(random)
	if err != nil {
		return nil, errEncrypt
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(random, salt); err != nil {
		return nil, errEncrypt
	}
	return encryptFixed(plaintext, p256dh, auth, asPriv, salt)
}

// encryptFixed is the encryption with every random input given: the
// application server's ephemeral private key and the salt. Only encryptWith
// calls it in production; the RFC 8291 section 5 vector calls it directly
// with that example's fixed values.
func encryptFixed(plaintext, p256dh, auth []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(auth) != authSecretLen || len(salt) != saltLen || len(p256dh) != uncompressedPointLen {
		return nil, errEncrypt
	}
	uaPub, err := ecdh.P256().NewPublicKey(p256dh) // validates the point
	if err != nil {
		return nil, errEncrypt
	}
	secret, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, errEncrypt
	}
	asPub := asPriv.PublicKey().Bytes()

	// RFC 8291 section 3.4: combine the ECDH secret with the auth secret.
	prkKey, err := hkdf.Extract(sha256.New, secret, auth)
	if err != nil {
		return nil, errEncrypt
	}
	keyInfo := make([]byte, 0, len("WebPush: info\x00")+2*uncompressedPointLen)
	keyInfo = append(keyInfo, "WebPush: info\x00"...)
	keyInfo = append(keyInfo, p256dh...)
	keyInfo = append(keyInfo, asPub...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, errEncrypt
	}

	// RFC 8188 section 2.2 and 2.3: the content encryption key and the nonce.
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, errEncrypt
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, errEncrypt
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, errEncrypt
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, errEncrypt
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errEncrypt
	}

	// Header (RFC 8188 section 2.1): salt, record size, key id length, key
	// id, which for Web Push is the sender's public key.
	out := make([]byte, 0, saltLen+4+1+len(asPub)+len(plaintext)+1+aead.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, uncompressedPointLen) // the key id length: the 65-byte public key
	out = append(out, asPub...)

	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, lastRecordDelimiter)
	return aead.Seal(out, nonce, record, nil), nil
}
