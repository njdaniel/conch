package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// Notification preferences and the pending feed (issue #1713, Mobile M11-A).
// The routes act on the caller's own credential and read only the caller's
// own channels, so B's member has no way to name anything of A's: the rows
// are "own" rows (a success, and no marker of A in the body or the headers),
// and the feed's own check asks for everything, with text, and requires that
// every summary is in a channel of B's and that the body holds nothing of A's.

// allPreferences is the request that makes the feed as wide as it can be.
var allPreferences = schema.PutNotificationPreferencesRequestV1{
	Policy: schema.NotificationPolicyAll, Muted: []int64{}, Preview: true,
}

// aPushReg is A's push registration, by id (issue #1714).
func aPushReg(w *world) subject { return subject{kind: "push_registration", name: w.a.pushRegID} }

// pushPath is the path of the registration a subject names; a row asked about
// no subject (the route check) names an id that is not one.
func pushPath(s subject) string {
	if s.name == "" {
		return "/v1/push/registrations/none"
	}
	return "/v1/push/registrations/" + s.name
}

// pushHost is the public IP literal the rows register at: the address check
// needs no lookup for it, and nothing is ever sent there.
const pushHost = "https://93.184.216.34/"

// registerPush has api register a Web Push endpoint at pushHost plus path with
// fresh keys. With mark, the keys are markers of A's.
func (w *world) registerPush(a api, path string, mark bool) (schema.PushRegistrationV1, error) {
	var reg schema.PushRegistrationV1
	req, err := pushRequest(path, func(p256dh, auth string) {
		if mark {
			w.marks.secret("A's push p256dh", p256dh)
			w.marks.secret("A's push auth", auth)
		}
	})
	if err != nil {
		return reg, err
	}
	err = a.call(http.MethodPost, "/v1/push/registrations", req, &reg)
	return reg, err
}

// pushRequest is a Web Push registration request at pushHost plus path, with
// a fresh key pair and secret; keys sees them.
func pushRequest(path string, keys func(p256dh, auth string)) (schema.CreatePushRegistrationRequestV1, error) {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return schema.CreatePushRegistrationRequestV1{}, err
	}
	authRaw := make([]byte, schema.PushAuthBytes)
	if _, err := rand.Read(authRaw); err != nil {
		return schema.CreatePushRegistrationRequestV1{}, err
	}
	p256dh := base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
	auth := base64.RawURLEncoding.EncodeToString(authRaw)
	keys(p256dh, auth)
	return schema.CreatePushRegistrationRequestV1{
		Transport: schema.PushTransportWebPush, Platform: schema.PushPlatformWeb, App: "https://isolation.example",
		Endpoint: pushHost + path, P256DH: p256dh, Auth: auth,
	}, nil
}

// notificationMatrixRows are B's members' rows.
func notificationMatrixRows() []row {
	own := func(route, what string, p func(w *world, s subject) probe) row {
		return row{route: route, what: what, probe: p, own: true}
	}
	return []row{
		own("GET /v1/notifications/preferences", "reads her own sign-in's notification preferences",
			func(*world, subject) probe { return get("/v1/notifications/preferences") }),
		own("PUT /v1/notifications/preferences", "sets her own sign-in's notification preferences",
			func(*world, subject) probe { return put("/v1/notifications/preferences", allPreferences) }),
		{route: "GET /v1/notifications/pending", what: "reads her own pending notifications", own: true, after: checkPendingFeedOfB,
			probe: func(*world, subject) probe { return get("/v1/notifications/pending") }},

		// Push registrations (issue #1714). The config and the own list
		// answer about the caller alone; a registration is created for her
		// own credential and answered without its target; and A's
		// registration id is the 404 of any id that is not hers: byte for
		// byte the answer to one never issued (the ghost comparison).
		own("GET /v1/push/config", "reads which push transports are on",
			func(*world, subject) probe { return get("/v1/push/config") }),
		own("GET /v1/push/registrations/mine", "lists her own sign-in's push registrations",
			func(*world, subject) probe { return get("/v1/push/registrations/mine") }),
		{route: "POST /v1/push/registrations", what: "registers her own sign-in for push", own: true, inspect: checkOwnPushRegistration,
			probe: func(w *world, _ subject) probe {
				req, err := pushRequest(w.bName("pushep"), func(string, string) {})
				if err != nil {
					panic(err) // crypto/rand failed: nothing sensible can run
				}
				return post("/v1/push/registrations", req)
			}},
		{route: "PUT /v1/push/registrations/{id}", what: "replaces the target of A's push registration", pick: aPushReg,
			want: answer{http.StatusNotFound, "push_registration_not_found"},
			probe: func(w *world, s subject) probe {
				req, err := pushRequest(w.bName("pushep"), func(string, string) {})
				if err != nil {
					panic(err)
				}
				return put(pushPath(s), schema.UpdatePushRegistrationRequestV1(req))
			}},
		{route: "DELETE /v1/push/registrations/{id}", what: "ends A's push registration", pick: aPushReg,
			want:  answer{http.StatusNotFound, "push_registration_not_found"},
			probe: func(_ *world, s subject) probe { return del(pushPath(s)) }},
	}
}

// checkOwnPushRegistration requires that a registration's answer is a valid
// registration with no part of the target in it.
func checkOwnPushRegistration(_ *world, _ api, body []byte) error {
	var reg schema.PushRegistrationV1
	if err := json.Unmarshal(body, &reg); err != nil {
		return fmt.Errorf("the registration is not one: %w", err)
	}
	if err := reg.Validate(); err != nil {
		return err
	}
	for _, field := range []string{"endpoint", "p256dh", "auth", "token", "provider"} {
		if strings.Contains(string(body), `"`+field+`"`) {
			return fmt.Errorf("the registration answers with its %s", field)
		}
	}
	return nil
}

// checkPendingFeedOfB sets the prober's preferences to everything with text,
// drains the feed, and requires that it holds only B's: every summary in one
// of B's channels, none by one of A's people, and no marker of A in any body.
func checkPendingFeedOfB(w *world, a api) error {
	if err := a.call(http.MethodPut, "/v1/notifications/preferences", allPreferences, nil); err != nil {
		return err
	}
	after := int64(0)
	for range 10 {
		status, body, err := a.do(http.MethodGet, "/v1/notifications/pending?after="+id(after), nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("the pending feed answered %d %s", status, w.h.redact(string(body)))
		}
		if leaks := w.leaks(body, asked{}); len(leaks) > 0 {
			return fmt.Errorf("the pending feed holds %s", leaks)
		}
		var page schema.ListPendingNotificationsResponseV1
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("the pending feed is not a page: %w", err)
		}
		if err := page.Validate(); err != nil {
			return err
		}
		for _, s := range page.Summaries {
			if !slices.Contains(w.b.chanIDs, s.ChannelID) {
				return fmt.Errorf("a pending notification (message %d) is in channel %d, which is not one of B's", s.MessageID, s.ChannelID)
			}
			for _, p := range w.a.people {
				if s.AuthorID == p.id {
					return fmt.Errorf("a pending notification (message %d) is by %d, a person of A's", s.MessageID, s.AuthorID)
				}
			}
		}
		if !page.More {
			return nil
		}
		after = page.Cursor
	}
	return fmt.Errorf("the pending feed did not end in ten pages")
}

// checkPendingNames is the hide-members row of the feed (§5.5): with
// see_member_directory off, a summary names its author only where
// GET /v1/channels/{channel}/authors names that author to the same reader,
// and carries author_id either way. who has set preferences (hw's build), and
// the feed is asked as who.
func (w *world) checkPendingNames(hw *hideWorld, phase string, who *member, marks *markers, mustHave bool) error {
	status, body, err := who.api.do(http.MethodGet, "/v1/notifications/pending", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s: %s reads her pending notifications = %d %s, want 200", phase, who.name, status, w.h.redact(string(body)))
	}
	if err := hw.scan(marks, phase+": the pending feed of "+who.name, body); err != nil {
		return err
	}
	var page schema.ListPendingNotificationsResponseV1
	if err := json.Unmarshal(body, &page); err != nil {
		return err
	}
	if err := page.Validate(); err != nil {
		return err
	}
	if mustHave && len(page.Summaries) == 0 {
		return fmt.Errorf("%s: the pending feed of %s is empty: the row has nothing to check", phase, who.name)
	}
	for _, s := range page.Summaries {
		var authors schema.ChannelAuthorsResponseV1
		path := fmt.Sprintf("/v1/channels/%s/authors?ids=%d", url.PathEscape(s.ChannelName), s.AuthorID)
		if err := who.api.call(http.MethodGet, path, nil, &authors); err != nil {
			return err
		}
		want := ""
		if len(authors.Authors) == 1 {
			want = authors.Authors[0].Name
		}
		if s.AuthorID <= 0 || s.AuthorName != want {
			return fmt.Errorf("%s: %s's summary of message %d names its author %q (id %d); the authors route says %q",
				phase, who.name, s.MessageID, s.AuthorName, s.AuthorID, want)
		}
	}
	return nil
}
