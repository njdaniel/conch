package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
