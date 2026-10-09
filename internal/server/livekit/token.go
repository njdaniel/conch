package livekit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// JoinParams describes one join token. The caller supplies the identity: this
// package knows nothing about principals.
type JoinParams struct {
	// Identity becomes the token's "sub" and the participant identity.
	Identity string
	// Room is the one room the token admits its holder to.
	Room string
	// CanPublish allows publishing a microphone track. Without it the holder
	// can only listen.
	CanPublish bool
	// Lifetime sets "exp". LiveKit checks a token only when a connection
	// starts, and it allows 60 seconds of leeway (measured on 1.13.7: accepted
	// 55 s past exp, refused at 65 s), so a token can start a connection for
	// Lifetime plus about a minute.
	Lifetime time.Duration
	// BackdateNotBefore moves "nbf" into the past. LiveKit's 60 seconds of
	// leeway applies to "nbf" too, so this is only needed for a clock more
	// than a minute behind ours; zero is the normal value.
	BackdateNotBefore time.Duration
}

// joinGrant is LiveKit's "video" grant for a participant. LiveKit treats an
// absent canPublish/canSubscribe/canPublishData as true, so these are never
// omitted: a false must be written out.
type joinGrant struct {
	RoomJoin          bool     `json:"roomJoin"`
	Room              string   `json:"room"`
	CanSubscribe      bool     `json:"canSubscribe"`
	CanPublish        bool     `json:"canPublish"`
	CanPublishData    bool     `json:"canPublishData"`
	CanPublishSources []string `json:"canPublishSources"`
}

// adminGrant is the "video" grant for a room-API call. Each call sets only
// what it needs; everything else stays off.
type adminGrant struct {
	RoomCreate bool   `json:"roomCreate,omitempty"`
	RoomList   bool   `json:"roomList,omitempty"`
	RoomAdmin  bool   `json:"roomAdmin,omitempty"`
	Room       string `json:"room,omitempty"`
}

type claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub,omitempty"`
	NotBefore int64  `json:"nbf"`
	Expires   int64  `json:"exp"`
	Video     any    `json:"video"`
}

// JoinToken signs a join token for p with the config's key pair. The token is
// a credential: callers hand it to the client and must not log it.
func (c *Client) JoinToken(p JoinParams) (string, error) {
	if p.Identity == "" || p.Room == "" {
		return "", errors.New("livekit: join token needs an identity and a room")
	}
	if p.Lifetime <= 0 {
		return "", errors.New("livekit: join token lifetime must be positive")
	}
	if p.BackdateNotBefore < 0 {
		return "", errors.New("livekit: join token backdate must not be negative")
	}
	now := c.now()
	return c.sign(claims{
		Issuer:    c.cfg.APIKey,
		Subject:   p.Identity,
		NotBefore: now.Add(-p.BackdateNotBefore).Unix(),
		Expires:   now.Add(p.Lifetime).Unix(),
		Video: joinGrant{
			RoomJoin:          true,
			Room:              p.Room,
			CanSubscribe:      true,
			CanPublish:        p.CanPublish,
			CanPublishData:    false,
			CanPublishSources: []string{"microphone"},
		},
	})
}

// adminTokenLife is how long a room-API token lives. A fresh one is signed
// per call, so this only has to outlast the call.
const adminTokenLife = time.Minute

// adminToken signs a one-minute token carrying only grant.
func (c *Client) adminToken(grant adminGrant) (string, error) {
	now := c.now()
	return c.sign(claims{
		Issuer:    c.cfg.APIKey,
		NotBefore: now.Add(-adminBackdate).Unix(),
		Expires:   now.Add(adminTokenLife).Unix(),
		Video:     grant,
	})
}

// adminBackdate tolerates a LiveKit clock behind ours, on top of the 60
// seconds of leeway LiveKit already allows.
const adminBackdate = 30 * time.Second

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func (c *Client) sign(cl claims) (string, error) {
	body, err := json.Marshal(cl)
	if err != nil {
		return "", errors.New("livekit: encode token claims")
	}
	signing := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(c.cfg.APISecret))
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
