package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Renaming (issue #441): PUT /v1/principals/{id}/name.

func renamePath(id int64) string { return fmt.Sprintf("/v1/principals/%d/name", id) }

func renameBody(t *testing.T, name string) string {
	t.Helper()
	return `{"name":` + jsonString(t, name) + `}`
}

// Who may rename whom. Each case runs on a fresh server, so no limit or
// earlier rename carries over.
func TestRenameAccess(t *testing.T) {
	tests := []struct {
		name string
		// caller and target pick a token and a principal id from the
		// fixture; target -1 is an id nobody has.
		caller, target string
		newName        string
		status         int
		code           string
		denied         bool // an access_denied row is written
	}{
		{name: "a person renames themselves", caller: "alice", target: "alice", newName: "Alice Liddell", status: 200},
		{name: "an operator renames a person", caller: "root", target: "alice", newName: "Alice Liddell", status: 200},
		{name: "an operator renames an agent", caller: "root", target: "bot", newName: "builder", status: 200},
		{name: "an operator renames themselves", caller: "root", target: "root", newName: "Root", status: 200},
		{name: "a member renames someone else", caller: "alice", target: "bot", newName: "evil", status: 403, code: "forbidden", denied: true},
		{name: "a member renames the operator", caller: "alice", target: "root", newName: "evil", status: 403, code: "forbidden", denied: true},
		{name: "a member probes an id nobody has", caller: "alice", target: "nobody", newName: "evil", status: 403, code: "forbidden", denied: true},
		{name: "an agent renames itself", caller: "bot", target: "bot", newName: "builder", status: 403, code: schema.ErrorCodeHumanRequired, denied: true},
		{name: "an operator renames an id nobody has", caller: "root", target: "nobody", newName: "someone", status: 404, code: "principal_not_found"},
		{name: "no credential", caller: "", target: "alice", newName: "Alice Liddell", status: 401, code: "unauthenticated"},
		{name: "a clash by case", caller: "alice", target: "alice", newName: "ROOT", status: 409, code: "principal_exists"},
		{name: "a clash by look-alike", caller: "alice", target: "alice", newName: "r\u043e\u043et", status: 409, code: "principal_exists"},
		{name: "an invalid name", caller: "alice", target: "alice", newName: "a\u202eb", status: 400, code: "invalid_request"},
		{name: "a blank name", caller: "alice", target: "alice", newName: "  ", status: 400, code: "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, AuthRequired)
			tokens := map[string]string{"root": f.rootTok, "alice": f.aliceTok, "bot": f.botTok, "": ""}
			ids := map[string]int64{"root": f.root.ID, "alice": f.alice.ID, "bot": f.bot.ID, "nobody": 9999}
			before := countAction(f.audit(t), "access_denied")
			rec := f.do(t, "PUT", renamePath(ids[tt.target]), tokens[tt.caller], renameBody(t, tt.newName))
			if tt.status != http.StatusOK {
				assertErrorBody(t, rec, tt.status, tt.code)
			} else {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d %s", rec.Code, rec.Body)
				}
				got := decodeBody[schema.RenamePrincipalResponseV1](t, rec).Principal
				if got.ID != ids[tt.target] || got.Name != tt.newName {
					t.Errorf("response principal = %+v, want id %d named %q", got, ids[tt.target], tt.newName)
				}
				stored, err := f.srv.store.PrincipalByID(context.Background(), ids[tt.target])
				if err != nil || stored.Name != tt.newName {
					t.Errorf("stored = %+v, %v", stored, err)
				}
			}
			if after := countAction(f.audit(t), "access_denied"); (after > before) != tt.denied {
				t.Errorf("access_denied rows %d -> %d; want a new one: %v", before, after, tt.denied)
			}
			if tt.status != http.StatusOK {
				if n := countAction(f.audit(t), store.AuditPrincipalRenamed); n != 0 {
					t.Errorf("%d principal_renamed rows after a refusal", n)
				}
			}
		})
	}
}

// The rename is audited with who did it, whom and both names; the denial of
// a member renaming someone else names the route and the reason only.
func TestRenameAudit(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	if rec := f.do(t, "PUT", renamePath(f.alice.ID), f.aliceTok, renameBody(t, "Alice Liddell")); rec.Code != http.StatusOK {
		t.Fatalf("self rename = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "PUT", renamePath(f.alice.ID), f.rootTok, renameBody(t, "alice \"to=x\"")); rec.Code != http.StatusOK {
		t.Fatalf("operator rename = %d %s", rec.Code, rec.Body)
	}
	var renames []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == store.AuditPrincipalRenamed {
			renames = append(renames, e)
		}
	}
	subject := fmt.Sprintf("principal:%d", f.alice.ID)
	want := []store.AuditEvent{
		{Actor: subject, Subject: subject, Detail: `from="alice" to="Alice Liddell"`},
		{Actor: fmt.Sprintf("principal:%d", f.root.ID), Subject: subject, Detail: `from="Alice Liddell" to="alice \"to=x\""`},
	}
	if len(renames) != len(want) {
		t.Fatalf("principal_renamed rows = %+v, want %d", renames, len(want))
	}
	for i, w := range want {
		if renames[i].Actor != w.Actor || renames[i].Subject != w.Subject || renames[i].Detail != w.Detail {
			t.Errorf("row %d = %+v, want %+v", i, renames[i], w)
		}
	}

	rec := f.do(t, "PUT", renamePath(f.bot.ID), f.aliceTok, renameBody(t, "evil"))
	assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
	var denial store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" {
			denial = e
		}
	}
	if denial.Actor != subject || denial.Subject != "PUT /v1/principals/{id}/name" || !strings.Contains(denial.Detail, "forbidden") || strings.Contains(denial.Detail, "evil") {
		t.Errorf("denial row = %+v", denial)
	}
}

// A person may rename themselves five times a day. The sixth is 429 with how
// long to wait; an operator's rename is not refused by the limit but counts
// toward it; and a day after the first the person may rename again.
func TestRenameRateLimit(t *testing.T) {
	f := newSessionFixture(t)
	self := func(name string) int {
		return f.do(t, "PUT", renamePath(f.alice.ID), f.aliceTok, renameBody(t, name)).Code
	}
	for i := 1; i <= renamesPerDay; i++ {
		if code := self(fmt.Sprintf("alice-%d", i)); code != http.StatusOK {
			t.Fatalf("rename %d = %d", i, code)
		}
		f.clock.Advance(time.Minute)
	}
	rec := f.do(t, "PUT", renamePath(f.alice.ID), f.aliceTok, renameBody(t, "alice-6"))
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	// The first rename was five minutes ago: a day less five minutes to go.
	wantWait := int((24*time.Hour - 5*time.Minute).Seconds())
	if got := rec.Header().Get("Retry-After"); got != strconv.Itoa(wantWait) {
		t.Errorf("Retry-After = %q, want %d", got, wantWait)
	}
	if got := decodeBody[schema.Error](t, rec).RetryAfterSeconds; got != wantWait {
		t.Errorf("retry_after_seconds = %d, want %d", got, wantWait)
	}
	// Renaming to the name already held is no rename and is not refused.
	if code := self("alice-5"); code != http.StatusOK {
		t.Errorf("rename to the current name = %d, want 200", code)
	}
	// An operator may still rename the person.
	if rec := f.do(t, "PUT", renamePath(f.alice.ID), f.rootTok, renameBody(t, "alice-op")); rec.Code != http.StatusOK {
		t.Fatalf("operator rename over the limit = %d %s", rec.Code, rec.Body)
	}
	// The operator's rename counts toward nothing: once the first of hers is
	// a day old, one more fits, and only one.
	f.clock.Advance(24*time.Hour - 5*time.Minute + time.Millisecond)
	if code := self("alice-7"); code != http.StatusOK {
		t.Errorf("rename a day after the first = %d, want 200: the operator's rename does not count", code)
	}
	if code := self("alice-8"); code != http.StatusTooManyRequests {
		t.Errorf("the next = %d, want 429", code)
	}
	// Another person's renames are their own.
	if code := f.do(t, "PUT", renamePath(f.bob.ID), f.bobTok, renameBody(t, "Robert")).Code; code != http.StatusOK {
		t.Errorf("bob's first rename = %d", code)
	}
}

// A rename refused because the name is taken tells the caller the name, or
// one like it, is on the server. So a person's refused attempts are audited
// (without the name) and counted: after renameAttemptsPerDay attempts in a
// day, renamed or refused, the next is 429 whatever the name. An operator's
// refused rename is neither audited so nor counted.
func TestRenameAttemptLimit(t *testing.T) {
	f := newSessionFixture(t)
	for i := 1; i <= renameAttemptsPerDay; i++ {
		taken := []string{"ROOT", "r\u043e\u043et", "B\u041eB", "bob"}[i%4]
		rec := f.do(t, "PUT", renamePath(f.alice.ID), f.aliceTok, renameBody(t, taken))
		assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodePrincipalExists)
		f.clock.Advance(time.Second)
	}
	refused := 0
	for _, e := range f.audit(t) {
		if e.Action != store.AuditPrincipalRenameRefused {
			continue
		}
		refused++
		self := fmt.Sprintf("principal:%d", f.alice.ID)
		if e.Actor != self || e.Subject != self || e.Detail != "reason=name_taken" {
			t.Errorf("refusal row = %+v", e)
		}
	}
	if refused != renameAttemptsPerDay {
		t.Errorf("%d principal_rename_refused rows, want %d", refused, renameAttemptsPerDay)
	}
	rec := f.do(t, "PUT", renamePath(f.alice.ID), f.aliceTok, renameBody(t, "a free name"))
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// The operator is not limited, and the operator's refusals are not
	// recorded as the person's.
	if rec := f.do(t, "PUT", renamePath(f.alice.ID), f.rootTok, renameBody(t, "BOB")); rec.Code != http.StatusConflict {
		t.Fatalf("operator rename to a taken name = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "PUT", renamePath(f.alice.ID), f.rootTok, renameBody(t, "a free name")); rec.Code != http.StatusOK {
		t.Fatalf("operator rename = %d %s", rec.Code, rec.Body)
	}
	if n := countAction(f.audit(t), store.AuditPrincipalRenameRefused); n != renameAttemptsPerDay {
		t.Errorf("%d refusal rows after the operator's, want %d", n, renameAttemptsPerDay)
	}
}

// A name made only of characters a reader cannot see is 400 invalid_request
// on create, rename and redeem; the invite is not used by the refusal.
func TestInvisibleNameRefused(t *testing.T) {
	const invisible = "\u3164\u2800\u3164"
	f := newInviteFixture(t, Config{})
	body := fmt.Sprintf(`{"kind":"human","name":%s}`, jsonString(t, invisible))
	assertErrorBody(t, f.do(t, "POST", "/v0/principals", f.rootTok, body), http.StatusBadRequest, "invalid_request")
	assertErrorBody(t, f.do(t, "PUT", renamePath(f.alice.ID), f.aliceTok, renameBody(t, invisible)), http.StatusBadRequest, "invalid_request")
	assertErrorBody(t, f.do(t, "PUT", renamePath(f.alice.ID), f.rootTok, renameBody(t, "\u3164")), http.StatusBadRequest, "invalid_request")
	resp := f.createInvite(t, f.rootTok, personal("general"))
	assertErrorBody(t, f.redeem(t, resp.Code, invisible), http.StatusBadRequest, "invalid_request")
	if rec := f.redeem(t, resp.Code, "newcomer"); rec.Code != http.StatusCreated {
		t.Fatalf("redeem after the 400 = %d %s; the refusal used the invite up", rec.Code, rec.Body)
	}
	if n := countAction(f.audit(t), store.AuditPrincipalRenameRefused); n != 0 {
		t.Errorf("%d refusal rows for an invisible name, want 0", n)
	}
}

// Redeeming an invite with a name that clashes with an existing one by case
// or look-alike letters is 409 principal_exists, and the invite stays usable.
func TestRedeemLookAlikeKeepsTheInvite(t *testing.T) {
	f := newInviteFixture(t, Config{})
	resp := f.createInvite(t, f.rootTok, personal("general"))
	for _, name := range []string{"ALICE", "AIice", "\u0430lice", "Alice\u2800", "R00T"} {
		assertErrorBody(t, f.redeem(t, resp.Code, name), http.StatusConflict, schema.ErrorCodePrincipalExists)
	}
	if rec := f.redeem(t, resp.Code, "newcomer"); rec.Code != http.StatusCreated {
		t.Fatalf("redeem after the 409s = %d %s; a 409 used the invite up", rec.Code, rec.Body)
	}
}

// After a rename the members list shows the new name, old messages still
// carry the same author id (so a client shows the new name on them), and an
// open events socket gets a roster with the new name.
func TestRenameShowsEverywhere(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	old, err := f.srv.store.InsertMessageV1(ctx, f.general.ID, f.bob.ID, "said before the rename", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.create(t, f.bobTok, humanBody)
	events := dialEvents(t, wsTestServer(t, f.srv), "/v1/events/ws?channel=general", f.aliceTok)
	named := func(name string) func(schema.SessionRosterV1) bool {
		return func(r schema.SessionRosterV1) bool {
			s, ok := rosterSession(r, f.bob.ID)
			return ok && s.Name == name
		}
	}
	events.until("a roster naming bob", isRoster(named("bob")))

	if rec := f.do(t, "PUT", renamePath(f.bob.ID), f.bobTok, renameBody(t, "Robert")); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	events.until("a roster naming Robert", isRoster(named("Robert")))

	members := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/general/members", f.aliceTok, ""))
	found := false
	for _, m := range members.Members {
		if m.PrincipalID == f.bob.ID {
			found = true
			if m.Name != "Robert" {
				t.Errorf("members list names bob %q, want Robert", m.Name)
			}
		}
	}
	if !found {
		t.Fatal("bob is not in the members list")
	}
	list := decodeBody[schema.ListMessagesResponseV2](t, f.do(t, "GET", "/v2/channels/general/messages", f.aliceTok, ""))
	for _, m := range list.Messages {
		if m.ID == old.ID && m.AuthorID != f.bob.ID {
			t.Errorf("the old message's author = %d, want %d", m.AuthorID, f.bob.ID)
		}
	}
	who := decodeBody[schema.WhoAmIResponseV1](t, f.do(t, "GET", "/v1/whoami", f.bobTok, ""))
	if who.Name != "Robert" {
		t.Errorf("whoami name = %q, want Robert", who.Name)
	}
}

// With authentication off the route is open like every other administrative
// route, has no limit, and audits the rename as the system.
func TestRenameAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	for i := 0; i <= renamesPerDay; i++ {
		if rec := f.do(t, "PUT", renamePath(f.alice.ID), "", renameBody(t, fmt.Sprintf("a%d", i))); rec.Code != http.StatusOK {
			t.Fatalf("rename %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	for _, e := range f.audit(t) {
		if e.Action == store.AuditPrincipalRenamed && e.Actor != "system" {
			t.Errorf("rename audited as %q, want system", e.Actor)
		}
	}
}
