package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/njdaniel/conch/pkg/schema"
)

// The marker scan: what group A made, and the check that none of it is in
// anything group B was sent.
//
// Text. Every string A writes (channel and member names, message words, file
// names and contents, the net, the run's task, the approval, the report) is
// made of A's run tag and hex (world.aName), never of words: so every run of
// six or more of its characters is A's alone, and a body holding any of them
// holds a piece of A's text. That catches a search snippet cut short, a
// highlight that splits a word, a name truncated to fit. The tag itself is
// one of those runs.
//
// Secrets. A's credentials, invite codes, voice tokens and room names are
// matched whole: they share prefixes with B's own (every JWT starts alike),
// so a piece of one says nothing.
//
// Ids. A bare number cannot be searched for: B's own ids are small numbers
// too, and B's message 7 is not A's file 7. So a body is read as JSON and
// each number is given the kind its key says it is: "channel_id" is a
// channel, "author_id" a principal, and "id" is whatever the object holding
// it is (an element of "channels" is a channel). A number of a kind that is
// A's is a leak, unless it is the very object (kind and id) the request
// named. A key that looks like an id (it ends in _id, _ids or _by) and that
// this file cannot classify fails the scan: a new field must be given a kind
// before the check can pass, rather than be passed over.
//
// Whatever the request itself named (A's channel name, say) is set aside
// before the body is scanned: a refusal may quote what it refused.

// A private register (community model §8, issue #981): A's public channel,
// its name and what is written there carry their own tag (world.uName) and
// are not registered, as a public channel is what any member may find. A's
// private channels, everything written in them, their voice rooms and their
// members stay markers; so, for now, do A's people's names and ids, which no
// route shows another member until the member directory (#983) unmarks them.
//
// The same scan has a second register, the plaza's private markers
// (guests.go): what was in the plaza before the guests' visits began, and
// what was said there after that no guest may see. A body is clean when it
// holds a marker of neither (world.leaks).

// window is the shortest run of A's text that counts as a piece of it.
const window = 6

// marker is one thing A made, and what it is, for the message when it leaks.
type marker struct{ what, value string }

// markers is the register of A's strings and ids.
type markers struct {
	mu      sync.Mutex
	text    []marker // every window of A's text
	secrets []marker
	ids     map[string]map[int64]string // kind -> id -> what
}

func newMarkers() *markers { return &markers{ids: map[string]map[int64]string{}} }

// str registers text A made: every run of window characters of it.
func (m *markers) str(what, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(value) < window {
		m.text = append(m.text, marker{what, value})
		return
	}
	for i := 0; i+window <= len(value); i++ {
		m.text = append(m.text, marker{what, value[i : i+window]})
	}
}

// secret registers a credential, code, token or room name of A's, matched
// whole.
func (m *markers) secret(what, value string) {
	if value == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets = append(m.secrets, marker{what, value})
}

// id registers an id of one of A's objects.
func (m *markers) id(kind, what string, id int64) {
	if id <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ids[kind] == nil {
		m.ids[kind] = map[int64]string{}
	}
	m.ids[kind][id] = what
}

// count says how much is registered, for the run's output.
func (m *markers) count() (text, secrets, ids int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, byID := range m.ids {
		ids += len(byID)
	}
	return len(m.text), len(m.secrets), ids
}

// kindID is an id with its kind.
type kindID struct {
	kind string
	id   int64
}

// asked is what a request named: those strings, and those objects (kind and
// id), may come back.
type asked struct {
	strs []string
	ids  []kindID
}

// has reports whether the request named this object.
func (a asked) has(kind string, id int64) bool { return slices.Contains(a.ids, kindID{kind, id}) }

// leaks returns every marker in body, each described once. A body that is
// not JSON is scanned for strings only.
func (m *markers) leaks(body []byte, a asked) []string { return m.scan(body, a, true) }

// scan is leaks; asJSON false scans text only.
func (m *markers) scan(body []byte, a asked, asJSON bool) []string {
	for _, s := range a.strs {
		if s != "" {
			body = bytes.ReplaceAll(body, []byte(s), nil)
			body = bytes.ReplaceAll(body, []byte(jsonEscaped(s)), nil)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	// A's strings share their tag, so a piece of one is often a piece of
	// several: they are named together, once.
	var pieces []string
	for _, mk := range m.text {
		if bytes.Contains(body, []byte(mk.value)) {
			pieces = append(pieces, mk.what)
		}
	}
	if len(pieces) > 0 {
		sort.Strings(pieces)
		out = append(out, "a piece of A's text ("+strings.Join(slices.Compact(pieces), ", ")+")")
	}
	for _, mk := range m.secrets {
		if bytes.Contains(body, []byte(mk.value)) || bytes.Contains(body, []byte(jsonEscaped(mk.value))) {
			out = append(out, mk.what)
		}
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if asJSON && dec.Decode(&doc) == nil {
		m.walk(doc, "", "", a, &out)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// leaks is markers.leaks over A's markers and, once the plaza exists, its
// private ones.
func (w *world) leaks(body []byte, a asked) []string {
	out := w.marks.leaks(body, a)
	if w.priv != nil {
		out = append(out, w.priv.leaks(body, a)...)
	}
	return out
}

// headerLeaks is markers.headerLeaks over both registers.
func (w *world) headerLeaks(h http.Header, a asked) []string {
	out := w.marks.headerLeaks(h, a)
	if w.priv != nil {
		out = append(out, w.priv.headerLeaks(h, a)...)
	}
	return out
}

// headerLeaks scans response headers as one text, all but the request id.
// conchd sets that id on every response, from random hex, so now and then it
// holds A's run tag by chance (six hex characters, a piece of every A name).
// Only a value of exactly that form (schema.ValidRequestID) is passed over:
// anything else in the header is scanned, so A's text put there is still found.
func (m *markers) headerLeaks(h http.Header, a asked) []string {
	var b strings.Builder
	for k, vs := range h {
		if http.CanonicalHeaderKey(k) == http.CanonicalHeaderKey(schema.RequestIDHeader) {
			vs = slices.DeleteFunc(slices.Clone(vs), schema.ValidRequestID)
		}
		b.WriteString(k + ": " + strings.Join(vs, ", ") + "\n")
	}
	return m.scan([]byte(b.String()), a, false)
}

// jsonEscaped is how encoding/json writes s inside a string: a body may
// escape < > & as < and so on.
func jsonEscaped(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// walk visits every value of a decoded JSON document. container is the key
// that holds the current object or array ("channels"), key the key of the
// current value.
func (m *markers) walk(v any, container, key string, a asked, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			m.walk(child, key, k, a, out)
		}
	case []any:
		for _, child := range t {
			// An element of a list is held by the list's key: an object in
			// "channels" is a channel, a number in "file_ids" a file.
			m.walk(child, key, key, a, out)
		}
	case json.Number:
		m.number(t.String(), container, key, a, out)
	case string:
		if idLike(key) && digitsOnly.MatchString(t) {
			m.number(t, container, key, a, out)
		}
	}
}

// number judges one number found under key.
func (m *markers) number(text, container, key string, a asked, out *[]string) {
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return
	}
	kind := idKind(container, key)
	if kind == "" {
		if idLike(key) {
			*out = append(*out, fmt.Sprintf("an id under %q that the scan cannot classify (give the key a kind in markers.go)", key))
		}
		return
	}
	if kind == "-" || a.has(kind, n) {
		return
	}
	if what, ok := m.ids[kind][n]; ok {
		*out = append(*out, fmt.Sprintf("%s (%s id %d, under %q)", what, kind, n, key))
	}
}

var digitsOnly = regexp.MustCompile(`^[0-9]+$`)

// idLike reports whether a key names an id: it ends in _id, _ids or _by.
func idLike(key string) bool {
	return strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids") || strings.HasSuffix(key, "_by")
}

// idKeys are the keys that name an id of a known kind wherever they are. "-"
// is an id of no group's object (a request id), passed over on purpose.
var idKeys = map[string]string{
	"channel_id": "channel", "channel_ids": "channel",
	"principal_id": "principal", "principal_ids": "principal", "author_id": "principal", "created_by": "principal",
	"requested_by": "principal", "requester_id": "principal", "invited_by": "principal", "sharer_id": "principal",
	"decided_by": "principal", "resolved_by": "principal", "added_by": "principal", "hidden_by": "principal", "disabled_by": "principal",
	"message_id": "message", "thread_root_id": "message", "root_id": "message", "last_read_id": "message", "reply_to": "message",
	"file_id": "file", "file_ids": "file",
	"session_id": "session", "run_id": "run", "intervention_id": "intervention",
	"approval_id": "approval", "invite_id": "invite", "report_id": "report", "net_id": "net",
	"credential_id": "credential", "sign_in_link_id": "sign_in_link",
	"request_id": "-",
}

// holders are the keys of objects (or lists of objects) whose "id" is of a
// known kind.
var holders = map[string]string{
	"channel": "channel", "channels": "channel",
	"principal": "principal", "principals": "principal", "members": "principal", "member": "principal",
	"joiners": "principal", "participants": "principal",
	"message": "message", "messages": "message", "root": "message",
	"file": "file", "files": "file",
	"session": "session", "sessions": "session",
	"run": "run", "runs": "run",
	"intervention": "intervention", "interventions": "intervention",
	"approval": "approval", "approvals": "approval",
	"invite": "invite", "invites": "invite",
	"report": "report", "reports": "report",
	"net": "net", "nets": "net",
	"credential": "credential", "credentials": "credential",
	"sign_in_link": "sign_in_link", "sign_in_links": "sign_in_link",
}

// idKind is the kind of id a number under key, inside an object held by
// container, is; "" when the keys do not say.
func idKind(container, key string) string {
	if k, ok := idKeys[key]; ok {
		return k
	}
	if key == "id" {
		return holders[container]
	}
	return ""
}

// digits finds a run of digits in an error message.
var digits = regexp.MustCompile(`[0-9]+`)

// numbersBeyond returns the numbers in text other than the ids asked for.
func numbersBeyond(text string, a asked) []string {
	var out []string
	for _, d := range digits.FindAllString(text, -1) {
		n, err := strconv.ParseInt(d, 10, 64)
		if err == nil && slices.ContainsFunc(a.ids, func(k kindID) bool { return k.id == n }) {
			continue
		}
		out = append(out, d)
	}
	return out
}
