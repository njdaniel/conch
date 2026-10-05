// lkctl: throwaway spike (#87). Mints LiveKit access tokens and calls the
// RoomService Twirp API using only the Go standard library.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// mint builds an HS256 JWT in the shape LiveKit expects: iss = API key,
// sub = participant identity, and a "video" grant object.
func mint(key, secret, identity string, grant map[string]any, ttl time.Duration) string {
	now := time.Now()
	head, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	claims := map[string]any{"iss": key, "nbf": now.Unix() - 5, "exp": now.Add(ttl).Unix(), "video": grant}
	if identity != "" {
		claims["sub"] = identity
	}
	body, _ := json.Marshal(claims)
	signing := b64(head) + "." + b64(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}

func twirp(host, token, method string, req any) ([]byte, int, error) {
	body, _ := json.Marshal(req)
	url := strings.TrimRight(host, "/") + "/twirp/livekit.RoomService/" + method
	r, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lkctl token|participants|subscribe|rooms [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	key := fs.String("key", "devkey", "API key")
	secret := fs.String("secret", "secret", "API secret")
	host := fs.String("host", "http://127.0.0.1:7880", "LiveKit HTTP address")
	room := fs.String("room", "", "room name")
	identity := fs.String("identity", "", "participant identity")
	canPub := fs.Bool("pub", true, "canPublish")
	canSub := fs.Bool("sub", true, "canSubscribe")
	tracks := fs.String("tracks", "", "comma-separated track sids")
	on := fs.Bool("on", true, "subscribe (true) or unsubscribe (false)")
	_ = fs.Parse(os.Args[2:])

	admin := func() string {
		return mint(*key, *secret, "", map[string]any{"roomAdmin": true, "roomList": true, "room": *room}, time.Minute)
	}
	var out []byte
	var code int
	var err error
	switch os.Args[1] {
	case "token":
		fmt.Println(mint(*key, *secret, *identity, map[string]any{
			"room": *room, "roomJoin": true, "canPublish": *canPub, "canSubscribe": *canSub, "canPublishData": false,
		}, 10*time.Minute))
		return
	case "rooms":
		out, code, err = twirp(*host, admin(), "ListRooms", map[string]any{})
	case "participants":
		out, code, err = twirp(*host, admin(), "ListParticipants", map[string]any{"room": *room})
	case "subscribe":
		out, code, err = twirp(*host, admin(), "UpdateSubscriptions", map[string]any{
			"room": *room, "identity": *identity, "track_sids": strings.Split(*tracks, ","), "subscribe": *on,
		})
	case "kick":
		out, code, err = twirp(*host, admin(), "RemoveParticipant", map[string]any{"room": *room, "identity": *identity})
	case "perm":
		out, code, err = twirp(*host, admin(), "UpdateParticipant", map[string]any{"room": *room, "identity": *identity,
			"permission": map[string]any{"can_subscribe": *canSub, "can_publish": *canPub}})
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%d %s\n", code, out)
}
