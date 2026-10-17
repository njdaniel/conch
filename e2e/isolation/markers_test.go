package main

import (
	"net/http"
	"strings"
	"testing"
)

func testMarkers() *markers {
	m := newMarkers()
	m.str("A's channel name", "alpha-3fa2c1-9be1c2d4")
	m.str("A's message", "alpha-3fa2c1-07aa51e0")
	m.secret("an A credential", "conch_tok_abcdefghijklmnop")
	m.id("channel", "A's channel camp", 3)
	m.id("principal", "A's member ann", 7)
	m.id("file", "A's file", 4)
	m.id("invite", "A's open link", 2)
	return m
}

// TestMarkerScan: the scanner finds each kind of leak, and lets through what
// the request named and numbers whose kind is not A's.
func TestMarkerScan(t *testing.T) {
	m := testMarkers()
	for _, tc := range []struct {
		name  string
		body  string
		asked asked
		leaks []string // a part of each leak's description, in order
	}{
		{"a refusal that leaks nothing", `{"code":"channel_not_found","message":"channel not found"}`, asked{}, nil},
		{"a refusal that quotes the channel it was asked for", `{"code":"channel_not_found","message":"no channel alpha-3fa2c1-9be1c2d4"}`,
			asked{strs: []string{"alpha-3fa2c1-9be1c2d4"}}, nil},
		{"a refusal that names the channel nobody asked for", `{"code":"forbidden","message":"not a member of alpha-3fa2c1-9be1c2d4"}`,
			asked{}, []string{"A's channel name"}},
		{"A's message in a list", `{"messages":[{"id":9,"body":"hello alpha-3fa2c1-07aa51e0"}]}`, asked{}, []string{"A's message"}},
		{"a search snippet cut short", `{"snippet":"…3fa2c1-07a…"}`, asked{}, []string{"A's message"}},
		{"a highlight that splits the word", `{"snippet":"<b>alpha-3f</b>a2c1-07aa51e0"}`, asked{}, []string{"A's message"}},
		{"five characters are not a piece", `{"snippet":"07aa5"}`, asked{}, nil},
		{"an A credential", `{"token":"conch_tok_abcdefghijklmnop"}`, asked{}, []string{"an A credential"}},
		{"A's channel id in a list of channels", `{"channels":[{"id":5,"name":"b"},{"id":3,"name":"x"}]}`, asked{},
			[]string{"channel id 3"}},
		{"A's member as an author", `{"messages":[{"id":3,"author_id":7}]}`, asked{}, []string{"principal id 7"}},
		{"A's member as a numeric string", `{"messages":[{"id":3,"author_id":"7"}]}`, asked{}, []string{"principal id 7"}},
		{"A's file attached to a message", `{"payload":{"data":{"file_ids":[4]}}}`, asked{}, []string{"file id 4"}},
		{"A's invite id in a list of invites", `{"invites":[{"id":2,"created_by":11}]}`, asked{}, []string{"invite id 2"}},
		{"the object the request named", `{"invites":[{"id":2}]}`, asked{ids: []kindID{{"invite", 2}}}, nil},
		{"the same number as another kind than the one named", `{"channels":[{"id":2}],"invites":[{"id":2}]}`, asked{ids: []kindID{{"invite", 2}}}, nil},
		{"A's object of another kind than the one named", `{"files":[{"id":4}]}`, asked{ids: []kindID{{"invite", 4}}}, []string{"file id 4"}},
		{"the same numbers as other kinds", `{"messages":[{"id":3,"author_id":4,"channel_id":7}],"invites":[{"id":7}]}`, asked{}, nil},
		{"a number whose kind the key does not say", `{"count":3,"retry_after_seconds":7}`, asked{}, nil},
		{"an id under a key the scan does not know", `{"widget_id":12}`, asked{}, []string{`"widget_id"`}},
		{"an id string under a key the scan does not know", `{"granted_by":"12"}`, asked{}, []string{`"granted_by"`}},
		{"a request id is no group's", `{"request_id":"12"}`, asked{}, nil},
		{"not JSON at all", `alpha-3fa2c1-9be1c2d4`, asked{}, []string{"A's channel name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := m.leaks([]byte(tc.body), tc.asked)
			if len(got) != len(tc.leaks) {
				t.Fatalf("leaks = %q, want %d matching %q", got, len(tc.leaks), tc.leaks)
			}
			for i, want := range tc.leaks {
				if !strings.Contains(got[i], want) {
					t.Errorf("leak %d = %q, want it to say %q", i, got[i], want)
				}
			}
		})
	}
}

// TestHeaderScan: response headers are scanned as text.
func TestHeaderScan(t *testing.T) {
	m := testMarkers()
	if got := m.headerLeaks(http.Header{"Content-Type": {"application/json"}}, asked{}); len(got) != 0 {
		t.Errorf("clean headers: leaks %q", got)
	}
	if got := m.headerLeaks(http.Header{"Content-Disposition": {`attachment; filename="alpha-3fa2c1-9be1c2d4.txt"`}}, asked{}); len(got) == 0 {
		t.Error("a header with A's name: no leak found")
	}
}

// TestNumbersBeyond: an error message may hold the id it was asked about
// and no other number.
func TestNumbersBeyond(t *testing.T) {
	for _, tc := range []struct {
		text  string
		asked asked
		want  int
	}{
		{"run not found", asked{}, 0},
		{"run 42 not found", asked{ids: []kindID{{"run", 42}}}, 0},
		{"run 42 is in channel 3", asked{ids: []kindID{{"run", 42}}}, 1},
		{"channel has 2 members", asked{}, 1},
	} {
		if got := numbersBeyond(tc.text, tc.asked); len(got) != tc.want {
			t.Errorf("numbersBeyond(%q) = %q, want %d numbers", tc.text, got, tc.want)
		}
	}
}
