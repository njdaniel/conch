package main

import (
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	_ "modernc.org/sqlite" // the driver conchd itself uses; loads the P1 fixture

	"github.com/njdaniel/conch/pkg/schema"
)

// p1SQL is a database as a P1 conchd left it: schema version 4, written by
// the conchd built from the last P1 commit (testdata/make-p1-fixture.sh). No
// current code had a hand in it, which is the point.
//
//go:embed testdata/p1.sql
var p1SQL string

// p1SchemaVersion is the schema version P1 shipped with (migrations 1 to 4).
const p1SchemaVersion = 4

// p1Database is what the fixture held before the upgrade, read straight from
// its tables. Everything asserted after the upgrade is compared with this, so
// the expectations are the fixture's own rows and not a second copy of them.
type p1Database struct {
	principals  []p1Principal
	channels    map[string]int64
	messages    []p1Message
	audit       []p1AuditEvent
	approvals   []p1Approval
	resolutions map[int64]string // approval id -> resolution_json
	hookToken   string
	hookChannel int64
	hookAuthor  int64
}

type p1Principal struct {
	id   int64
	kind string
	name string
}

type p1Message struct {
	id, channelID, authorID, createdAt int64
	body                               string
	payloadSchema                      sql.NullString
	payloadJSON                        []byte
}

type p1AuditEvent struct {
	id, createdAt                  int64
	actor, action, subject, detail string
}

type p1Approval struct {
	id        int64
	title     string
	state     string
	quorum    int
	decisions []p1Decision
}

type p1Decision struct {
	principalID      int64
	optionID, reason string
}

// loadP1 writes the fixture into a new database file at path and reads back
// what it holds.
func loadP1(path string) (p1Database, error) {
	var p1 p1Database
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return p1, err
	}
	defer func() { _ = db.Close() }()
	ctx := runCtx
	// The dump is one script with its own BEGIN and COMMIT, so it must run
	// on one connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return p1, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, p1SQL); err != nil {
		return p1, fmt.Errorf("load testdata/p1.sql: %w", err)
	}
	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return p1, err
	}
	if version != p1SchemaVersion {
		return p1, fmt.Errorf("the fixture is at schema version %d, want %d: it is not a P1 database", version, p1SchemaVersion)
	}

	query := func(q string, scan func(*sql.Rows) error) error {
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}

	p1.channels = map[string]int64{}
	p1.resolutions = map[int64]string{}
	err = errors.Join(
		query(`SELECT id, kind, name FROM principals ORDER BY id`, func(r *sql.Rows) error {
			var p p1Principal
			err := r.Scan(&p.id, &p.kind, &p.name)
			p1.principals = append(p1.principals, p)
			return err
		}),
		query(`SELECT id, name FROM channels`, func(r *sql.Rows) error {
			var id int64
			var channel string
			err := r.Scan(&id, &channel)
			p1.channels[channel] = id
			return err
		}),
		query(`SELECT id, channel_id, author_id, body, created_at, payload_schema, payload_json FROM messages ORDER BY id`, func(r *sql.Rows) error {
			var m p1Message
			err := r.Scan(&m.id, &m.channelID, &m.authorID, &m.body, &m.createdAt, &m.payloadSchema, &m.payloadJSON)
			p1.messages = append(p1.messages, m)
			return err
		}),
		query(`SELECT id, actor, action, subject, detail, created_at FROM audit_events ORDER BY id`, func(r *sql.Rows) error {
			var e p1AuditEvent
			err := r.Scan(&e.id, &e.actor, &e.action, &e.subject, &e.detail, &e.createdAt)
			p1.audit = append(p1.audit, e)
			return err
		}),
		query(`SELECT id, title, state, quorum FROM approvals ORDER BY id`, func(r *sql.Rows) error {
			var a p1Approval
			err := r.Scan(&a.id, &a.title, &a.state, &a.quorum)
			p1.approvals = append(p1.approvals, a)
			return err
		}),
		query(`SELECT approval_id, principal_id, option_id, reason FROM approval_decisions ORDER BY id`, func(r *sql.Rows) error {
			var approvalID int64
			var d p1Decision
			if err := r.Scan(&approvalID, &d.principalID, &d.optionID, &d.reason); err != nil {
				return err
			}
			for i := range p1.approvals {
				if p1.approvals[i].id == approvalID {
					p1.approvals[i].decisions = append(p1.approvals[i].decisions, d)
				}
			}
			return nil
		}),
		query(`SELECT approval_id, resolution_json FROM approval_resolutions`, func(r *sql.Rows) error {
			var id int64
			var resolution string
			err := r.Scan(&id, &resolution)
			p1.resolutions[id] = resolution
			return err
		}),
		query(`SELECT token, channel_id, principal_id FROM hooks`, func(r *sql.Rows) error {
			return r.Scan(&p1.hookToken, &p1.hookChannel, &p1.hookAuthor)
		}),
	)
	if err != nil {
		return p1, fmt.Errorf("read the P1 fixture: %w", err)
	}
	// A fixture that lost its contents would make every comparison below
	// pass on nothing.
	states := map[string]int{}
	for _, a := range p1.approvals {
		states[a.state]++
	}
	if len(p1.messages) < 5 || len(p1.audit) < 20 || p1.hookToken == "" ||
		states["pending"] < 2 || states["resolved"] < 2 || states["expired"] < 1 {
		return p1, fmt.Errorf("the P1 fixture is not what this program expects: %d messages, %d audit rows, approvals by state %v, hook token present: %v",
			len(p1.messages), len(p1.audit), states, p1.hookToken != "")
	}
	return p1, nil
}

func (p1 p1Database) principal(who string) (p1Principal, error) {
	for _, p := range p1.principals {
		if p.name == who {
			return p, nil
		}
	}
	return p1Principal{}, fmt.Errorf("the P1 fixture has no principal named %s", who)
}

// approval returns the fixture's one approval in this state with this quorum.
func (p1 p1Database) approval(state string, quorum int) (p1Approval, error) {
	var found []p1Approval
	for _, a := range p1.approvals {
		if a.state == state && a.quorum == quorum {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return p1Approval{}, fmt.Errorf("the P1 fixture has %d %s approvals with quorum %d, want exactly one", len(found), state, quorum)
	}
	return found[0], nil
}

// upgrade loads the P1 database, starts today's conchd on it and shows that
// nothing P1 stored was lost: every row is served through the public API as
// it was written.
func (h *harness) upgrade() error {
	dataDir := filepath.Join(h.root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	p1, err := loadP1(filepath.Join(dataDir, "conch.db"))
	if err != nil {
		return err
	}
	h.p1 = p1
	h.secret(p1.hookToken)
	step("a P1 database is in place: schema version %d, %d principals, %d messages, %d approvals, %d audit rows, one webhook",
		p1SchemaVersion, len(p1.principals), len(p1.messages), len(p1.approvals), len(p1.audit))

	ntfy, err := newFakeNtfy()
	if err != nil {
		return err
	}
	h.ntfy = ntfy
	h.onCleanup(ntfy.close)
	if err := h.startConchd(dataDir); err != nil {
		return err
	}
	step("bootstrap-operator and serve run on it: the upgrade needs nothing but conchd")

	var operator schema.WhoAmIResponseV1
	if err := h.op().call(http.MethodGet, "/v1/whoami", nil, &operator); err != nil {
		return err
	}
	if operator.Role != schema.RoleOperator || operator.Kind != schema.PrincipalHuman {
		return fmt.Errorf("bootstrap-operator's credential is for a %s with role %s, want a human operator", operator.Kind, operator.Role)
	}
	h.operatorID = operator.ID

	// The people and agents P1 knew are the same principals afterwards.
	for _, target := range []struct {
		who  string
		into **person
	}{{"nick", &h.nick}, {"ana", &h.ana}, {"leviathan", &h.leviathan}, {"scout", &h.scout}} {
		was, err := p1.principal(target.who)
		if err != nil {
			return err
		}
		*target.into = &person{name: was.name, id: was.id}
	}
	h.opsID = p1.channels["ops"]
	for _, p := range []*person{h.nick, h.ana} {
		if err := h.signIn(p); err != nil {
			return err
		}
	}
	for _, was := range p1.principals {
		_, token, err := h.issue(was.id, "upgrade-check")
		if err != nil {
			return err
		}
		var who schema.WhoAmIResponseV1
		if err := (api{baseURL: h.proc.baseURL, token: token}).call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
			return fmt.Errorf("whoami as P1's %s: %w", was.name, err)
		}
		if who.ID != was.id || string(who.Kind) != was.kind || who.Name != was.name || who.Role != schema.RoleMember {
			return fmt.Errorf("P1's %s %q (id %d) is now %s %q (id %d) with role %q, want the same principal as a member", was.kind, was.name, was.id, who.Kind, who.Name, who.ID, who.Role)
		}
	}
	step("every P1 principal is the same principal, a member and not an operator")

	// Channels and messages, read by a person P1 knew: the upgrade made the
	// access everyone had explicit, so nick is a member of both channels.
	var channels schema.ListChannelsResponse
	if err := h.as(h.nick).call(http.MethodGet, "/v1/channels", nil, &channels); err != nil {
		return err
	}
	got := map[string]int64{}
	for _, c := range channels.Channels {
		got[c.Name] = c.ID
	}
	if !reflect.DeepEqual(got, p1.channels) {
		return fmt.Errorf("nick's channels after the upgrade = %v, want P1's %v", got, p1.channels)
	}
	for channel, channelID := range p1.channels {
		list, err := h.allMessages(h.nick, channel)
		if err != nil {
			return err
		}
		var want []p1Message
		for _, m := range p1.messages {
			if m.channelID == channelID {
				want = append(want, m)
			}
		}
		if len(list) != len(want) {
			return fmt.Errorf("channel %s has %d messages after the upgrade, P1 stored %d", channel, len(list), len(want))
		}
		for i, m := range list {
			if err := sameMessage(m, want[i]); err != nil {
				return fmt.Errorf("channel %s: %w", channel, err)
			}
		}
	}
	step("all %d P1 messages are served as P1 stored them: ids, authors, text, typed payloads and times", len(p1.messages))

	// The audit trail: P1's rows are the head of today's export, unchanged.
	events, err := h.audit()
	if err != nil {
		return err
	}
	if len(events) < len(p1.audit) {
		return fmt.Errorf("the audit export has %d rows, P1 stored %d", len(events), len(p1.audit))
	}
	for i, was := range p1.audit {
		e := events[i]
		if e.ID != was.id || e.Actor != was.actor || e.Action != was.action || e.Subject != was.subject || e.Detail != was.detail || e.CreatedAt.Time().UnixMilli() != was.createdAt {
			return fmt.Errorf("audit row %d changed in the upgrade: %s %s %s, want %s %s %s", was.id, e.Actor, e.Action, e.Subject, was.actor, was.action, was.subject)
		}
	}
	step("all %d P1 audit rows head the audit export, unchanged", len(p1.audit))

	// Approvals: each by id, with the resolution P1 recorded.
	var open []int64
	for _, was := range p1.approvals {
		var read schema.GetApprovalResponseV1
		if err := h.as(h.nick).call(http.MethodGet, fmt.Sprintf("/v1/approvals/%d", was.id), nil, &read); err != nil {
			return err
		}
		if err := read.Approval.Validate(); err != nil {
			return fmt.Errorf("P1 approval %d does not validate today: %w", was.id, err)
		}
		if read.Approval.Title != was.title || string(read.Approval.State) != was.state || read.Approval.Quorum != was.quorum {
			return fmt.Errorf("P1 approval %d is now %q in state %s with quorum %d, want %q, %s, %d", was.id, read.Approval.Title, read.Approval.State, read.Approval.Quorum, was.title, was.state, was.quorum)
		}
		stored, resolved := p1.resolutions[was.id]
		switch {
		case resolved && read.Resolution == nil:
			return fmt.Errorf("P1 approval %d lost its resolution", was.id)
		case !resolved && read.Resolution != nil:
			return fmt.Errorf("P1 approval %d gained a resolution it never had", was.id)
		case resolved:
			served, err := json.Marshal(read.Resolution)
			if err != nil {
				return err
			}
			same, err := sameJSON(served, []byte(stored))
			if err != nil {
				return fmt.Errorf("P1 approval %d's resolution: %w", was.id, err)
			}
			if !same {
				return fmt.Errorf("P1 approval %d's resolution changed in the upgrade", was.id)
			}
		}
		if was.state == "pending" {
			open = append(open, was.id)
		}
	}
	var listed schema.ListApprovalsResponseV1
	if err := h.as(h.nick).call(http.MethodGet, "/v1/approvals", nil, &listed); err != nil {
		return err
	}
	var listedIDs []int64
	for _, a := range listed.Approvals {
		listedIDs = append(listedIDs, a.ID)
	}
	if !reflect.DeepEqual(listedIDs, open) {
		return fmt.Errorf("open approvals after the upgrade = %v, want the ones P1 left open, %v", listedIDs, open)
	}
	step("all %d P1 approvals are served by id in the state P1 left them, resolutions intact; the %d it left open are still open", len(p1.approvals), len(open))

	// Starting on old data must not tell anyone anything: every P1
	// transition already has its notification attempt on record.
	if hits := h.ntfy.delivered(); len(hits) != 0 {
		return fmt.Errorf("starting on the P1 database sent %d notifications about old approvals, want none: %+v", len(hits), hits)
	}
	for _, e := range events[len(p1.audit):] {
		if strings.HasPrefix(e.Subject, "approval:") {
			return fmt.Errorf("starting on the P1 database wrote %s for %s, want no new row about a P1 approval", e.Action, e.Subject)
		}
	}
	step("starting on the P1 database re-sent no notification and changed no approval")

	// Search covers what was written before search existed.
	for _, c := range []struct {
		channel, words string
		wantBody       string
	}{
		{"general", "walrus lighthouse", "the walrus keeps the lighthouse"},
		{"ops", "candidate", "deploy candidate 41 ready"},
	} {
		var found schema.SearchMessagesResponseV2
		if err := h.as(h.ana).call(http.MethodGet, "/v2/channels/"+c.channel+"/messages/search?q="+url.QueryEscape(c.words), nil, &found); err != nil {
			return err
		}
		if len(found.Messages) != 1 || found.Messages[0].Body != c.wantBody {
			return fmt.Errorf("searching %s for %q found %d messages, want the one P1 message with those words", c.channel, c.words, len(found.Messages))
		}
	}
	step("search finds messages P1 wrote before search existed")
	return nil
}

// sameMessage reports how a served message differs from the P1 row, if it
// does. It never quotes a body.
func sameMessage(m schema.MessageV1, was p1Message) error {
	if err := m.Validate(); err != nil {
		return fmt.Errorf("P1 message %d does not validate today: %w", was.id, err)
	}
	for field, same := range map[string]bool{
		"id":      m.ID == was.id,
		"channel": m.ChannelID == was.channelID,
		"author":  m.AuthorID == was.authorID,
		"text":    m.Body == was.body,
		"time":    m.CreatedAt.Time().UnixMilli() == was.createdAt,
	} {
		if !same {
			return fmt.Errorf("message %d's %s changed in the upgrade", was.id, field)
		}
	}
	if !was.payloadSchema.Valid {
		if m.Payload != nil {
			return fmt.Errorf("message %d gained a payload in the upgrade", was.id)
		}
		return nil
	}
	if m.Payload == nil || m.Payload.Schema != was.payloadSchema.String {
		return fmt.Errorf("message %d lost its %s payload in the upgrade", was.id, was.payloadSchema.String)
	}
	same, err := sameJSON(m.Payload.Data, was.payloadJSON)
	if err != nil {
		return fmt.Errorf("message %d's payload data: %w", was.id, err)
	}
	if !same {
		return fmt.Errorf("message %d's payload data changed in the upgrade", was.id)
	}
	return nil
}

// sameJSON reports whether two JSON documents say the same thing.
func sameJSON(a, b []byte) (bool, error) {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if err := da.Decode(&x); err != nil {
		return false, err
	}
	if err := db.Decode(&y); err != nil {
		return false, err
	}
	return reflect.DeepEqual(x, y), nil
}

// audit exports the whole audit trail through the public API, as the
// operator, and checks every line is a valid conch.audit_event.v1 in
// ascending id order.
func (h *harness) audit() ([]schema.AuditEventV1, error) {
	resp, err := h.op().send(http.MethodGet, "/v1/audit", nil)
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/audit: status %d (%s)", resp.status, errorCode(resp.body))
	}
	return parseAudit(resp.body)
}

// parseAudit reads an audit export: JSON Lines, one event per line.
func parseAudit(export []byte) ([]schema.AuditEventV1, error) {
	var events []schema.AuditEventV1
	for n, line := range bytes.Split(bytes.TrimRight(export, "\n"), []byte("\n")) {
		var e schema.AuditEventV1
		if err := decodeStrict(line, &e); err != nil {
			return nil, fmt.Errorf("audit export line %d: %w", n+1, err)
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("audit export line %d: %w", n+1, err)
		}
		if len(events) > 0 && e.ID <= events[len(events)-1].ID {
			return nil, fmt.Errorf("audit export line %d has id %d after id %d: not ascending", n+1, e.ID, events[len(events)-1].ID)
		}
		events = append(events, e)
	}
	return events, nil
}

// chain returns the actions recorded about one subject, in order.
func chain(events []schema.AuditEventV1, subject string) []string {
	var actions []string
	for _, e := range events {
		if e.Subject == subject {
			actions = append(actions, e.Action)
		}
	}
	return actions
}

// wantChain checks the audit trail's actions about one approval, in order.
func (h *harness) wantChain(approvalID int64, want ...string) error {
	events, err := h.audit()
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("approval:%d", approvalID)
	if got := chain(events, subject); !reflect.DeepEqual(got, want) {
		return fmt.Errorf("audit chain for %s = %v, want %v", subject, got, want)
	}
	return nil
}

// allMessages reads a whole channel as one person, page by page.
func (h *harness) allMessages(reader *person, channel string) ([]schema.MessageV1, error) {
	var all []schema.MessageV1
	after := int64(0)
	for {
		var page schema.ListMessagesResponseV1
		if err := h.as(reader).call(http.MethodGet, fmt.Sprintf("/v1/channels/%s/messages?limit=100&after=%d", channel, after), nil, &page); err != nil {
			return nil, err
		}
		if len(page.Messages) == 0 {
			return all, nil
		}
		all = append(all, page.Messages...)
		after = page.Messages[len(page.Messages)-1].ID
	}
}
