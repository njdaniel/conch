package push

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("bad base64url in test: %v", err)
	}
	return b
}

// The worked example of RFC 8291 section 5 and appendix A.
const (
	rfcPlaintext   = "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24"
	rfcASPrivate   = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcASPublic    = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	rfcUAPrivate   = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcUAPublic    = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcAuth        = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcSalt        = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcECDHSecret  = "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs" // #nosec G101 -- a published RFC 8291 example value
	rfcPRKKey      = "Snr3JMxaHVDXHWJn5wdC52WjpCtd2EIEGBykDcZW32k"
	rfcIKM         = "S4lYMb_L0FxCeq0WhDx813KgSYqU26kOyzWUdsXYyrg"
	rfcPRK         = "09_eUZGrsvxChDCGRCdkLiDXrReGOEVeSCdCcPBSJSc"
	rfcCEK         = "oIhVW04MRdy2XN9CiKLxTg"
	rfcNonce       = "4h_95klXJ5E_qnoN"
	rfcCiphertext  = "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ"
	rfcRequestBody = `DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml
mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT
pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN`
)

// TestRFC8291Vector reproduces the worked example of RFC 8291 section 5,
// byte for byte, from its fixed keys and salt through the test seam
// (encryptFixed), and checks each intermediate value of appendix A on the way.
func TestRFC8291Vector(t *testing.T) {
	plaintext := b64(t, rfcPlaintext)
	if string(plaintext) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("plaintext = %q", plaintext)
	}
	asPriv, err := ecdh.P256().NewPrivateKey(b64(t, rfcASPrivate))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(asPriv.PublicKey().Bytes(), b64(t, rfcASPublic)) {
		t.Fatal("the sender's public key is not the example's")
	}
	uaPub := b64(t, rfcUAPublic)
	auth := b64(t, rfcAuth)
	salt := b64(t, rfcSalt)

	t.Run("intermediate values", func(t *testing.T) {
		uaPubKey, err := ecdh.P256().NewPublicKey(uaPub)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := asPriv.ECDH(uaPubKey)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(secret, b64(t, rfcECDHSecret)) {
			t.Error("ecdh_secret differs")
		}
		prkKey, _ := hkdf.Extract(sha256.New, secret, auth)
		if !bytes.Equal(prkKey, b64(t, rfcPRKKey)) {
			t.Error("PRK_key differs")
		}
		keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPriv.PublicKey().Bytes()...)
		ikm, _ := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
		if !bytes.Equal(ikm, b64(t, rfcIKM)) {
			t.Error("IKM differs")
		}
		prk, _ := hkdf.Extract(sha256.New, ikm, salt)
		if !bytes.Equal(prk, b64(t, rfcPRK)) {
			t.Error("PRK differs")
		}
		cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
		if !bytes.Equal(cek, b64(t, rfcCEK)) {
			t.Error("CEK differs")
		}
		nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
		if !bytes.Equal(nonce, b64(t, rfcNonce)) {
			t.Error("NONCE differs")
		}
	})

	t.Run("the request body", func(t *testing.T) {
		got, err := encryptFixed(plaintext, uaPub, auth, asPriv, salt)
		if err != nil {
			t.Fatal(err)
		}
		if want := b64(t, rfcRequestBody); !bytes.Equal(got, want) {
			t.Errorf("body differs:\n got %s\nwant %s", base64.RawURLEncoding.EncodeToString(got), base64.RawURLEncoding.EncodeToString(want))
		}
		// The example's printed "Content-Length: 145" does not match its own
		// body (86 header octets and 58 of ciphertext are 144); the body is
		// what is reproduced.
		if len(got) != 144 {
			t.Errorf("len = %d, want 144", len(got))
		}
		header := got[:86]
		if !bytes.Equal(got[86:], b64(t, rfcCiphertext)) {
			t.Error("ciphertext differs from appendix A")
		}
		if !bytes.Equal(header[:16], salt) || binary.BigEndian.Uint32(header[16:20]) != 4096 || header[20] != 65 {
			t.Error("header fields differ")
		}
	})

	t.Run("the seam is the only way to fix the randomness", func(t *testing.T) {
		// Encrypt, which production uses, takes no fixed input: two calls
		// never agree.
		a, err := Encrypt(plaintext, uaPub, auth)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Encrypt(plaintext, uaPub, auth)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(a[:16], b[:16]) {
			t.Error("two messages share a salt")
		}
		if bytes.Equal(a[21:86], b[21:86]) {
			t.Error("two messages share an ephemeral key")
		}
	})
}

// decryptForTest is the subscriber's side of RFC 8291, written apart from the
// encryption so a round trip proves each against the other and the vector.
func decryptForTest(body []byte, uaPriv *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	if len(body) < 86+17 {
		return nil, errors.New("short")
	}
	salt, idlen := body[:16], int(body[20])
	if idlen != 65 || binary.BigEndian.Uint32(body[16:20]) < 18 {
		return nil, errors.New("header")
	}
	asPub, err := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	if err != nil {
		return nil, err
	}
	secret, err := uaPriv.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	prkKey, _ := hkdf.Extract(sha256.New, secret, auth)
	info := append(append([]byte("WebPush: info\x00"), uaPriv.PublicKey().Bytes()...), asPub.Bytes()...)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, string(info), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	aead, _ := cipher.NewGCM(block)
	rec, err := aead.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	i := len(rec) - 1
	for i >= 0 && rec[i] == 0 {
		i--
	}
	if i < 0 || rec[i] != 0x02 {
		return nil, errors.New("delimiter")
	}
	return rec[:i], nil
}

// TestEncryptRoundTrip: what Encrypt makes the subscriber's key and auth
// decrypt, a wrong auth or key does not, and a changed byte anywhere fails the
// tag.
func TestEncryptRoundTrip(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := bytes.Repeat([]byte{7}, 16)
	plaintext := []byte(`{"schema":"conch.push_wake.v1","registration":"abc"}`)
	body, err := Encrypt(plaintext, ua.PublicKey().Bytes(), auth)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		b := bytes.Clone(body)
		b[i] ^= 1
		return b
	}
	tests := []struct {
		name string
		body []byte
		key  *ecdh.PrivateKey
		auth []byte
		ok   bool
	}{
		{"right key and auth", body, ua, auth, true},
		{"wrong auth", body, ua, bytes.Repeat([]byte{8}, 16), false},
		{"wrong key", body, other, auth, false},
		{"tampered tag", flip(len(body) - 1), ua, auth, false},
		{"tampered ciphertext", flip(len(body) - 20), ua, auth, false},
		{"tampered salt", flip(0), ua, auth, false},
		{"tampered sender key id", flip(30), ua, auth, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decryptForTest(tt.body, tt.key, tt.auth)
			if tt.ok {
				if err != nil || !bytes.Equal(got, plaintext) {
					t.Fatalf("decrypt = %q, %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("decrypted to %q", got)
			}
		})
	}
}

// TestEncryptRefusesBadInput: keys and secrets that cannot be used are refused
// with an error that names none of them.
func TestEncryptRefusesBadInput(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	pub := ua.PublicKey().Bytes()
	offCurve := bytes.Clone(pub)
	offCurve[64] ^= 1
	tests := []struct {
		name       string
		p256dh, au []byte
	}{
		{"point off the curve", offCurve, make([]byte, 16)},
		{"short key", pub[:64], make([]byte, 16)},
		{"compressed key", append([]byte{2}, pub[1:33]...), make([]byte, 16)},
		{"empty key", nil, make([]byte, 16)},
		{"short auth", pub, make([]byte, 15)},
		{"long auth", pub, make([]byte, 17)},
		{"no auth", pub, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Encrypt([]byte("x"), tt.p256dh, tt.au)
			if err == nil {
				t.Fatal("encrypted")
			}
			if !errors.Is(err, errEncrypt) {
				t.Errorf("error = %v; want the fixed error", err)
			}
		})
	}
}
