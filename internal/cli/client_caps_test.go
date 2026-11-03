package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// paddedBody is a well-formed JSON object of exactly size bytes, so a client
// with no cap decodes it whole.
func paddedBody(key string, size int) string {
	head := `{"` + key + `":[],"pad":"`
	const tail = `"}`
	return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
}

// A hostile server sends a list one byte over its cap: the client refuses it
// with its own line. A body exactly at the cap is still read.
func TestListReadsRefuseOversizedBody(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		key  string
		cap  int64
		read func(*Client) error
	}{
		{"channel directory", "channels", maxChannelDirectoryBody, func(c *Client) error {
			_, err := c.ChannelDirectory(ctx, false)
			return err
		}},
		{"channel directory, all", "channels", maxChannelDirectoryBody, func(c *Client) error {
			_, err := c.ChannelDirectory(ctx, true)
			return err
		}},
		{"directory members", "members", maxDirectoryMembersBody, func(c *Client) error {
			_, err := c.DirectoryMembers(ctx, 0, 200)
			return err
		}},
		{"member links", "invites", maxMemberLinksBody, func(c *Client) error {
			_, err := c.ListMemberLinks(ctx)
			return err
		}},
		{"link joiners", "joiners", maxJoinersBody, func(c *Client) error {
			_, err := c.MemberLinkJoiners(ctx, 3)
			return err
		}},
		{"channel members", "members", maxChannelMembersBody, func(c *Client) error {
			_, err := c.ListChannelMembers(ctx, "general")
			return err
		}},
		{"principals", "principals", maxPrincipalsBody, func(c *Client) error {
			_, err := c.ListPrincipals(ctx, PrincipalListFilter{})
			return err
		}},
		{"guests", "guests", maxGuestsBody, func(c *Client) error {
			_, err := c.ListGuests(ctx, GuestListFilter{})
			return err
		}},
		{"hooks", "hooks", maxHooksBody, func(c *Client) error {
			_, err := c.ListHooks(ctx)
			return err
		}},
	}
	for _, tt := range tests {
		for _, over := range []bool{true, false} {
			name := tt.name + " at the cap"
			size := int(tt.cap)
			if over {
				name = tt.name + " one byte over"
				size++
			}
			t.Run(name, func(t *testing.T) {
				body := paddedBody(tt.key, size)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				t.Cleanup(srv.Close)
				client, err := NewClient(srv.URL, srv.Client())
				if err != nil {
					t.Fatal(err)
				}
				err = tt.read(client.WithToken("tok"))
				switch {
				case over && (err == nil || !strings.Contains(ErrorLine(err), "larger than")):
					t.Fatalf("error = %v, want the client's refusal (larger than ...)", err)
				case !over && err != nil:
					t.Fatalf("a body at the cap: %v", err)
				}
			})
		}
	}
}

// A server whose cursor always moves on is followed for maxListPages pages
// and no further, so it cannot keep the client reading and holding for ever
// (issue #1198, security review of PR #1204).
func TestPagedListsStopAtThePageLimit(t *testing.T) {
	cases := []struct {
		name string
		args []string
		page func(n int64) any
	}{
		{"principals", []string{"admin", "principal", "list"}, func(n int64) any {
			return schema.ListPrincipalsResponseV1{Principals: []schema.PrincipalAdminV1{}, NextBefore: 1<<40 - n}
		}},
		{"guests", []string{"admin", "guest", "list"}, func(n int64) any {
			return schema.ListGuestsResponseV1{Guests: []schema.GuestAdminV1{}, NextBefore: 1<<40 - n}
		}},
		{"directory members", []string{"admin", "directory", "members"}, func(n int64) any {
			return schema.ServerMembersResponseV1{Members: []schema.ServerMemberV1{}, After: n + 1, Scope: schema.DirectoryScopeEveryone}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			var pages atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := pages.Add(1)
				_ = json.NewEncoder(w).Encode(tc.page(n))
			}))
			defer srv.Close()
			var stdout, stderr bytes.Buffer
			err := RunWithStdin(context.Background(), append(tc.args, "--server", srv.URL), strings.NewReader(""), &stdout, &stderr, "vtest")
			if err == nil || !strings.Contains(err.Error(), "pages do not end") {
				t.Fatalf("err = %v, want the page-limit refusal", err)
			}
			if got := pages.Load(); got != maxListPages {
				t.Errorf("read %d pages, want exactly %d", got, maxListPages)
			}
		})
	}
}
