package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// removalAdmin seats the agent in general and grants it the same read access
// it would need to see the message through MCP.
func removalAdmin(t *testing.T) (*doorFixture, *schema.AdminTargetV1) {
	t.Helper()
	d := newDoorFixture(t, schema.PermManageChannels)
	target := generalTarget(t, d)
	if _, err := d.srv.store.AddChannelMember(t.Context(), "system", target.ID, d.agent.ID, 0); err != nil {
		t.Fatal(err)
	}
	req := adminManifest(d, nil, nil, schema.PermManageChannels)
	req.Channels = append(req.Channels, schema.ChannelGrant{ChannelID: target.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}})
	d.putManifest(t, req)
	return d, target
}

func removeMessageParams(id int64) string {
	return fmt.Sprintf(`{"message_id":%d}`, id)
}

func postRemovalTestMessage(t *testing.T, d *doorFixture, body string) schema.MessageV2 {
	t.Helper()
	rec := d.do(t, "POST", "/v2/channels/general/messages", d.aliceTok, fmt.Sprintf(`{"body":%q}`, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("post message = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.PostMessageResponseV2](t, rec).Message
}

func readRemovalTestMessage(t *testing.T, d *doorFixture, id int64) schema.MessageV2 {
	t.Helper()
	rec := d.do(t, "GET", "/v2/channels/general/messages", d.aliceTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list messages = %d %s", rec.Code, rec.Body)
	}
	for _, msg := range decodeBody[schema.ListMessagesResponseV2](t, rec).Messages {
		if msg.ID == id {
			return msg
		}
	}
	t.Fatalf("message %d missing from history", id)
	return schema.MessageV2{}
}

// TestAdminRemoveMessageFullChain takes a months-old approval card through the
// MCP admin door, notification, human decision, removal, and audit.
func TestAdminRemoveMessageFullChain(t *testing.T) {
	d, target := removalAdmin(t)
	code, raw := mcpDoor.describe(t, d)
	var catalog schema.AdminCatalogResponseV1
	if code != "ok" || json.Unmarshal(raw, &catalog) != nil {
		t.Fatalf("describe_admin_actions = %s %s", code, raw)
	}
	wantEntry := schema.AdminCatalogEntryV1{
		Action: schema.AdminActionRemoveMessage, Route: "DELETE /v2/channels/{channel}/messages/{message_id}",
		Permission: schema.PermManageChannels, Gate: schema.AdminGateApproval, Held: true, Gated: true,
	}
	if !slices.Contains(catalog.Actions, wantEntry) {
		t.Fatalf("remove_message entry missing from catalog: %s", raw)
	}

	// Seed a legacy card, as the current post route refuses reserved payloads.
	card := d.do(t, "POST", "/v1/approvals", d.aliceTok, createApprovalBody(target.ID, d.alice.ID))
	if card.Code != http.StatusCreated {
		t.Fatalf("create card's approval = %d %s", card.Code, card.Body)
	}
	original := decodeBody[schema.CreateApprovalResponseV1](t, card).Approval
	d.waitNtfy(t, 1, "card's approval request")
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	const originalBody = "original card text"
	inserted, err := d.db.Exec("INSERT INTO messages (channel_id, author_id, body, created_at, payload_schema, payload_json) VALUES (?, ?, ?, ?, ?, ?)",
		target.ID, d.alice.ID, originalBody, time.Now().AddDate(0, -3, 0).UnixMilli(), schema.ApprovalV1Name, string(payload))
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewServer(d.srv.Handler())
	t.Cleanup(httpServer.Close)
	sock := openSocket(t, httpServer.URL, "/v2/ws?channel=general&after=0", d.aliceTok)
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("socket = %d %s", sock.status, sock.body)
	}
	if frames := sock.readUntil(t, messageID); !strings.Contains(string(frames[len(frames)-1]), originalBody) {
		t.Fatalf("original card missing from replay: %s", frames)
	}

	code, raw = mcpDoor.submit(t, d, schema.AdminActionRemoveMessage, target, removeMessageParams(messageID), "remove stale card")
	var pending schema.AdminActionResultV1
	if code != "ok" || json.Unmarshal(raw, &pending) != nil || pending.Validate() != nil || pending.Status != schema.AdminActionPendingApproval {
		t.Fatalf("submit_admin_action = %s %s", code, raw)
	}
	id := pending.ApprovalID
	route := "DELETE " + rmPath("general", messageID)
	if pending.Approval.ChannelID != d.approvalCh.ID || !strings.Contains(pending.Approval.Body, route) {
		t.Fatalf("unexpected removal approval: %+v", pending.Approval)
	}
	d.waitNtfy(t, 2, "removal approval request")
	d.decide(t, id, "run")
	d.waitNtfy(t, 3, "removal approval resolution")
	if got := readRemovalTestMessage(t, d, messageID); got.Removed || got.Body != originalBody || got.Payload == nil {
		t.Fatalf("card changed before the approved action ran: %+v", got)
	}

	code, raw = mcpDoor.run(t, d, id)
	if got := reduceResult(t, code, raw); got != "ok ran 204" {
		t.Fatalf("run_approved_action = %s (%s)", got, raw)
	}
	assertTombstone := func(got schema.MessageV2) {
		t.Helper()
		if got.ID != messageID || !got.Removed || got.Body != schema.MessageRemovedBody || got.Payload != nil {
			t.Fatalf("expected the card's tombstone, got %+v", got)
		}
	}
	frames := sock.readUntil(t, messageID)
	var live schema.MessageV2
	if err := json.Unmarshal(frames[len(frames)-1], &live); err != nil {
		t.Fatal(err)
	}
	assertTombstone(live)
	assertTombstone(readRemovalTestMessage(t, d, messageID))
	if a, err := d.srv.store.ApprovalByID(t.Context(), original.ID); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("card's approval after removal = %+v (%v)", a, err)
	}

	agent, human := fmt.Sprintf("principal:%d", d.agent.ID), fmt.Sprintf("principal:%d", d.root.ID)
	approvalSubject, messageSubject := fmt.Sprintf("approval:%d", id), fmt.Sprintf("message:%d", messageID)
	var chain []string
	for _, e := range d.audit(t) {
		if e.Subject == approvalSubject || e.Subject == messageSubject && e.Action == store.AuditMessageRemoved {
			chain = append(chain, e.Action+" "+e.Actor)
		}
		if e.Subject == messageSubject && e.Action == store.AuditMessageRemoved {
			if want := fmt.Sprintf("channel=%d approval=%d approved_by=%s", target.ID, id, human); e.Detail != want {
				t.Errorf("removal audit detail = %q, want %q", e.Detail, want)
			}
		}
	}
	want := []string{
		store.AuditApprovalCreated + " " + agent,
		store.AuditAdminActionRequested + " " + agent,
		approvals.AuditNotifySent + " system",
		store.AuditDecisionCast + " " + human,
		store.AuditApprovalResolved + " system",
		approvals.AuditNotifySent + " system",
		store.AuditMessageRemoved + " " + agent,
		store.AuditAdminActionRan + " " + agent,
	}
	if !slices.Equal(chain, want) {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}

func TestAdminRemoveMessageRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, d *doorFixture, approvalID int64)
		want   string
	}{
		{"pending", func(*testing.T, *doorFixture, int64) {}, schema.ErrorCodeApprovalNotApproved},
		{"refused by approver", func(t *testing.T, d *doorFixture, id int64) {
			d.decide(t, id, "refuse")
		}, schema.ErrorCodeApprovalNotApproved},
		{"expired", func(t *testing.T, d *doorFixture, id int64) {
			if r, err := d.srv.store.ExpireApproval(t.Context(), id); err != nil || r == nil {
				t.Fatalf("expire approval = %v, %v", r, err)
			}
		}, schema.ErrorCodeApprovalNotApproved},
		{"permission revoked", func(t *testing.T, d *doorFixture, id int64) {
			d.decide(t, id, "run")
			d.grant(t, d.agent.ID, schema.PermManageMembers)
		}, "ok ran 403 forbidden"},
		{"read revoked", func(t *testing.T, d *doorFixture, id int64) {
			d.decide(t, id, "run")
			d.grant(t, d.agent.ID, schema.PermManageChannels)
		}, "ok ran 404 message_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, target := removalAdmin(t)
			msg := postRemovalTestMessage(t, d, "keep this")
			id := d.submitGated(t, submitBody(schema.AdminActionRemoveMessage, target, removeMessageParams(msg.ID), ""))
			tc.before(t, d, id)
			code, raw := restDoor.run(t, d, id)
			if got := reduceResult(t, code, raw); got != tc.want {
				t.Errorf("run = %q (%s), want %q", got, raw, tc.want)
			}
			if got := readRemovalTestMessage(t, d, msg.ID); got.Removed || got.Body != msg.Body {
				t.Errorf("refused action changed the message: %+v", got)
			}
			for _, e := range d.audit(t) {
				if e.Action == store.AuditMessageRemoved {
					t.Errorf("refused action recorded a removal: %+v", e)
				}
			}
		})
	}
}

func TestAdminRemoveMessageMissingAndRemoved(t *testing.T) {
	d, target := removalAdmin(t)
	msg := postRemovalTestMessage(t, d, "already handled")
	for _, tc := range []struct {
		name string
		id   int64
		want string
	}{
		{"missing", msg.ID + 100, "ok ran 404 message_not_found"},
		{"removed by moderator before run", msg.ID, "ok ran 409 " + schema.ErrorCodeMessageAlreadyRemoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := d.submitGated(t, submitBody(schema.AdminActionRemoveMessage, target, removeMessageParams(tc.id), ""))
			d.decide(t, id, "run")
			if tc.id == msg.ID {
				rec := d.do(t, "DELETE", rmPath("general", msg.ID), d.rootTok, "")
				if rec.Code != http.StatusNoContent {
					t.Fatalf("moderator removal = %d %s", rec.Code, rec.Body)
				}
			}
			code, raw := restDoor.run(t, d, id)
			if got := reduceResult(t, code, raw); got != tc.want {
				t.Errorf("run = %q (%s), want %q", got, raw, tc.want)
			}
		})
	}
	for _, e := range d.audit(t) {
		if e.Action == store.AuditMessageRemoved && e.Actor != fmt.Sprintf("principal:%d", d.root.ID) {
			t.Errorf("failed action recorded a removal: %+v", e)
		}
	}
}
