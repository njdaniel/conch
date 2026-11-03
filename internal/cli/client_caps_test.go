package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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
		// okErr is what a body at the cap may still fail with after it has
		// been read whole (a check made on the decoded document).
		okErr string
	}{
		{"channel directory", "channels", maxChannelDirectoryBody, func(c *Client) error {
			_, err := c.ChannelDirectory(ctx, false)
			return err
		}, ""},
		{"channel directory, all", "channels", maxChannelDirectoryBody, func(c *Client) error {
			_, err := c.ChannelDirectory(ctx, true)
			return err
		}, ""},
		{"directory members", "members", maxDirectoryMembersBody, func(c *Client) error {
			_, err := c.DirectoryMembers(ctx, 0, 200)
			return err
		}, ""},
		{"member links", "invites", maxMemberLinksBody, func(c *Client) error {
			_, err := c.ListMemberLinks(ctx)
			return err
		}, ""},
		{"link joiners", "joiners", maxJoinersBody, func(c *Client) error {
			_, err := c.MemberLinkJoiners(ctx, 3)
			return err
		}, ""},
		{"channel members", "members", maxChannelMembersBody, func(c *Client) error {
			_, err := c.ListChannelMembers(ctx, "general")
			return err
		}, ""},
		{"principals", "principals", maxPrincipalsBody, func(c *Client) error {
			_, err := c.ListPrincipals(ctx, PrincipalListFilter{})
			return err
		}, ""},
		{"guests", "guests", maxGuestsBody, func(c *Client) error {
			_, err := c.ListGuests(ctx, GuestListFilter{})
			return err
		}, ""},
		{"channel list", "channels", maxChannelListBody, func(c *Client) error {
			_, err := c.ListChannels(ctx)
			return err
		}, ""},
		{"v1 messages", "messages", pageBody(1, messageRowBound), func(c *Client) error {
			_, err := c.ListMessages(ctx, "general", 0, 1)
			return err
		}, ""},
		{"v2 messages", "messages", pageBody(1, messageRowBound), func(c *Client) error {
			_, err := c.ListMessagesV2(ctx, "general", 0, 1)
			return err
		}, ""},
		{"v2 messages, newest first", "messages", pageBody(1, messageRowBound), func(c *Client) error {
			_, err := c.ListMessagesV2Page(ctx, "general", MessagesPage{NewestFirst: true}, 1)
			return err
		}, ""},
		{"thread", "messages", pageBody(1, messageRowBound), func(c *Client) error {
			_, err := c.ListThreadV2(ctx, "general", 5, 0, 1)
			return err
		}, ""},
		{"search", "messages", pageBody(1, messageRowBound), func(c *Client) error {
			_, err := c.SearchMessagesV2(ctx, "general", "words", 0, 1)
			return err
		}, ""},
		{"reports", "reports", pageBody(1, reportRowBound), func(c *Client) error {
			_, err := c.ListProblemReports(ctx, 1)
			return err
		}, ""},
		{"reports, whole page", "reports", pageBody(maxReportPage, reportRowBound), func(c *Client) error {
			_, err := c.ListProblemReports(ctx, 0)
			return err
		}, ""},
		{"nets", "nets", maxNetsBody, func(c *Client) error {
			_, err := c.ListNets(ctx, "general")
			return err
		}, ""},
		{"read state", "read_state", maxReadStateBody, func(c *Client) error {
			_, err := c.ListReadState(ctx)
			return err
		}, ""},
		{"credentials", "credentials", maxCredentialsBody, func(c *Client) error {
			_, err := c.ListCredentials(ctx, 3)
			return err
		}, ""},
		{"runs", "runs", pageBody(1, runRowBound), func(c *Client) error {
			_, err := c.ListRuns(ctx, "general", 0, RunsPage{}, 1)
			return err
		}, ""},
		{"activity", "activity", pageBody(1, activityRowBound), func(c *Client) error {
			_, err := c.ListActivity(ctx, 7, 0, 1)
			return err
		}, ""},
		{"interventions", "interventions", pageBody(1, interventionRowBound), func(c *Client) error {
			_, err := c.ListInterventions(ctx, 7, 0, 1)
			return err
		}, ""},
		{"a limit past the page is read as the page", "runs", pageBody(maxRunPage, runRowBound), func(c *Client) error {
			_, err := c.ListRuns(ctx, "general", 0, RunsPage{}, 100000)
			return err
		}, ""},
		{"device links", "sign_in_links", maxDeviceLinksBody, func(c *Client) error {
			_, err := c.ListOwnDeviceLinks(ctx)
			return err
		}, ""},
		{"voice occupancy", "channels", maxVoiceOccupancyBody, func(c *Client) error {
			_, err := c.VoiceOccupancy(ctx)
			return err
		}, "invalid voice occupancy"},
		{"hooks", "hooks", maxHooksBody, func(c *Client) error {
			_, err := c.ListHooks(ctx)
			return err
		}, ""},
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
				case !over && err != nil && (tt.okErr == "" || !strings.Contains(err.Error(), tt.okErr)):
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
		{"thread", []string{"thread", "--server", "{url}", "general", "5"}, func(n int64) any {
			return schema.ListMessagesResponseV2{Messages: []schema.MessageV2{}, NextAfter: n}
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
			args := append([]string(nil), tc.args...)
			if i := slices.Index(args, "{url}"); i >= 0 {
				args[i] = srv.URL // flags come before the positional arguments
			} else {
				args = append(args, "--server", srv.URL)
			}
			err := RunWithStdin(context.Background(), args, strings.NewReader(""), &stdout, &stderr, "vtest")
			if err == nil || !strings.Contains(err.Error(), "pages do not end") {
				t.Fatalf("err = %v, want the page-limit refusal", err)
			}
			if got := pages.Load(); got != maxListPages {
				t.Errorf("read %d pages, want exactly %d", got, maxListPages)
			}
		})
	}
}
