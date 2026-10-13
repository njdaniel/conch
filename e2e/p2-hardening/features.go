package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// channelList shows the channel list (#57) through REST, through the client
// call the TUI makes, and in the TUI itself: each person sees their channels
// and no others.
func (h *harness) channelList() error {
	names := func(resp schema.ListChannelsResponse) []string {
		var out []string
		for _, c := range resp.Channels {
			out = append(out, c.Name)
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []struct {
		who  *person
		want []string
	}{
		{h.nick, []string{"general", "lab", "ops", "vault"}},
		{h.ana, []string{"general", "ops"}},
		{h.outsider, nil},
	} {
		var rest schema.ListChannelsResponse
		if err := h.as(c.who).call(http.MethodGet, "/v1/channels", nil, &rest); err != nil {
			return err
		}
		if got := names(rest); !reflect.DeepEqual(got, c.want) {
			return fmt.Errorf("GET /v1/channels as %s = %v, want %v", c.who.name, got, c.want)
		}
		client, err := cli.NewClient(h.proc.baseURL, httpClient)
		if err != nil {
			return err
		}
		viaClient, err := client.WithToken(c.who.token).ListChannels(runCtx)
		if err != nil {
			return fmt.Errorf("the client's channel list as %s: %w", c.who.name, err)
		}
		if got := names(viaClient); !reflect.DeepEqual(got, c.want) {
			return fmt.Errorf("the client's channel list as %s = %v, want %v", c.who.name, got, c.want)
		}
	}
	step("GET /v1/channels and the client call the TUI makes list each person's channels and no others")
	// The same screen for two people: nick's shows the channels ana's must
	// not, so their absence there is the list and not the drawing.
	if err := h.tuiChannelList(h.nick, []string{"general", "lab", "ops", "vault"}, nil); err != nil {
		return err
	}
	return h.tuiChannelList(h.ana, []string{"general", "ops"}, []string{"lab", "vault"})
}

// search shows message search (#22) through the API and the CLI: it finds
// what the searcher may read and nothing else.
func (h *harness) search() error {
	// Three messages share a word nobody has used: one to the channel from a
	// person, one from an agent, and a whisper from nick to ana.
	if _, err := h.nick.cli.run("", "send", "ops", "the heron counts pebbles at dusk"); err != nil {
		return err
	}
	if _, err := tool[schema.PostMessageResponseV2](h.leviathan, "post_message", map[string]any{"channel": "ops", "body": "heron report filed"}); err != nil {
		return err
	}
	if _, err := h.nick.cli.run("", "send", "--to", strconv.FormatInt(h.ana.id, 10), "ops", "heron, for ana only"); err != nil {
		return err
	}
	const whisper = "heron, for ana only"
	path := "/v2/channels/ops/messages/search?q=" + url.QueryEscape("heron")
	for _, c := range []struct {
		who  *person
		want []string // newest first
	}{
		{h.ana, []string{whisper, "heron report filed", "the heron counts pebbles at dusk"}},
		{h.nick, []string{whisper, "heron report filed", "the heron counts pebbles at dusk"}},
		// leviathan may read ops and is not in the whisper's audience.
		{h.leviathan, []string{"heron report filed", "the heron counts pebbles at dusk"}},
	} {
		var found schema.SearchMessagesResponseV2
		if err := h.as(c.who).call(http.MethodGet, path, nil, &found); err != nil {
			return err
		}
		var got []string
		for _, m := range found.Messages {
			if err := m.Validate(); err != nil {
				return err
			}
			got = append(got, m.Body)
		}
		if !reflect.DeepEqual(got, c.want) {
			return fmt.Errorf("search as %s found %d messages, want %d, newest first, and only what %s may read", c.who.name, len(got), len(c.want), c.who.name)
		}
	}
	if _, err := h.as(h.outsider).refused("a non-member searches a channel", http.MethodGet, path, nil, http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if _, err := h.as(h.courier).refused("an agent searches a channel it is not in", http.MethodGet, path, nil, http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	// The CLI: every word must match, in any order; no match prints nothing.
	out, err := h.ana.cli.run("", "search", "ops", "pebbles", "HERON")
	if err != nil {
		return err
	}
	if lines := nonEmptyLines(out); len(lines) != 1 || !strings.Contains(lines[0], "the heron counts pebbles at dusk") {
		return fmt.Errorf("conch search ops pebbles HERON printed %d lines, want the one message with both words", len(lines))
	}
	out, err = h.ana.cli.run("", "search", "ops", "heron")
	if err != nil {
		return err
	}
	if lines := nonEmptyLines(out); len(lines) != 3 || !strings.Contains(lines[0], whisper) || !strings.Contains(lines[0], "[whisper:") {
		return fmt.Errorf("conch search ops heron as ana printed %d lines, want 3 with the whisper first and marked as one", len(lines))
	}
	out, err = h.ana.cli.run("", "search", "ops", "heron", "walrus")
	if err != nil {
		return fmt.Errorf("conch search with no match must exit 0: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("conch search with no match printed %d bytes, want nothing", len(out))
	}
	step("search finds a word for exactly the people and agents who may read each message, through REST and conch search")
	return nil
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// files shows file upload (#23): a person uploads with the CLI, the people
// who can see the message fetch the same bytes, and nobody else can.
func (h *harness) files() error {
	// Bytes that are not text: a file is stored and served as it is.
	content := make([]byte, 70_000)
	for i := range content {
		content[i] = byte(i*31 + i/251)
	}
	sum := sha256.Sum256(content)
	path := filepath.Join(h.nick.cli.home, "tide-table.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return err
	}
	stdout, _, err := h.nick.cli.runSplit("", "upload", "--message", "the tide table for the week", "ops", path)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("conch upload printed %q, want the file id alone", strings.TrimSpace(stdout))
	}
	metaPath, filePath := fmt.Sprintf("/v2/files/%d/meta", id), fmt.Sprintf("/v2/files/%d", id)

	var meta schema.FileV1
	if err := h.as(h.ana).call(http.MethodGet, metaPath, nil, &meta); err != nil {
		return err
	}
	if err := meta.Validate(); err != nil {
		return err
	}
	if meta.ID != id || meta.ChannelID != h.opsID || meta.UploaderID != h.nick.id || meta.Filename != "tide-table.bin" ||
		meta.Size != int64(len(content)) || meta.SHA256 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("file %d's metadata is for a %d-byte %q by principal %d, want the %d bytes nick uploaded as tide-table.bin with their SHA-256", id, meta.Size, meta.Filename, meta.UploaderID, len(content))
	}
	resp, err := h.as(h.ana).send(http.MethodGet, filePath, nil)
	if err != nil {
		return err
	}
	if resp.status != http.StatusOK || !bytes.Equal(resp.body, content) {
		return fmt.Errorf("GET %s as a member: status %d and %d bytes, want the %d bytes uploaded", filePath, resp.status, len(resp.body), len(content))
	}
	// Never served as anything a browser would run or show in place.
	if ct, nosniff, disp := resp.header.Get("Content-Type"), resp.header.Get("X-Content-Type-Options"), resp.header.Get("Content-Disposition"); ct != schema.FileDownloadMediaType || nosniff != "nosniff" || !strings.HasPrefix(disp, "attachment") {
		return fmt.Errorf("a download is served as %q, nosniff %q, disposition %q; want %s, nosniff, attachment", ct, nosniff, disp, schema.FileDownloadMediaType)
	}
	saved := filepath.Join(h.ana.cli.home, "fetched.bin")
	if _, err := h.ana.cli.run("", "download", "-o", saved, strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	fetched, err := os.ReadFile(saved) // #nosec G304 -- a path this program just chose inside its own temp directory
	if err != nil {
		return err
	}
	if !bytes.Equal(fetched, content) {
		return fmt.Errorf("conch download wrote %d bytes, want the %d uploaded", len(fetched), len(content))
	}
	// The message that carries it is in the channel, with the file attached.
	var list schema.ListMessagesResponseV2
	if err := h.as(h.ana).call(http.MethodGet, "/v2/channels/ops/messages?limit=100", nil, &list); err != nil {
		return err
	}
	attached := false
	for _, m := range list.Messages {
		if m.Body == "the tide table for the week" && m.Payload != nil && m.Payload.Schema == schema.FileAttachmentV1Name && m.AuthorID == h.nick.id {
			attached = true
		}
	}
	if !attached {
		return fmt.Errorf("ops has no message by nick attaching file %d", id)
	}
	step("conch upload stores a file; a member reads its metadata and fetches the same bytes, by REST and conch download")

	for _, c := range []struct {
		what   string
		as     api
		status int
		code   string
	}{
		{"a non-member", h.as(h.outsider), http.StatusNotFound, "file_not_found"},
		{"nobody", h.anonymous(), http.StatusUnauthorized, "unauthenticated"},
		// Files are for people: an agent is refused before the file is
		// looked at, even one that can read the channel.
		{"an agent that can read the channel", h.as(h.leviathan), http.StatusForbidden, "forbidden"},
	} {
		for _, p := range []string{metaPath, filePath} {
			if _, err := c.as.refused(c.what+" fetches "+p, http.MethodGet, p, nil, c.status, c.code); err != nil {
				return err
			}
		}
	}
	if _, err := h.as(h.outsider).refused("a non-member uploads", http.MethodPost, "/v2/channels/ops/files?filename=x.txt", []byte("x"), http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if _, err := h.as(h.leviathan).refused("an agent uploads", http.MethodPost, "/v2/channels/ops/files?filename=x.txt", []byte("x"), http.StatusForbidden, "forbidden"); err != nil {
		return err
	}
	if out, err := h.outsider.cli.run("", "download", "-o", filepath.Join(h.outsider.cli.home, "stolen.bin"), strconv.FormatInt(id, 10)); err == nil {
		return fmt.Errorf("conch download as a non-member succeeded (%d bytes of output)", len(out))
	}
	if _, err := os.Stat(filepath.Join(h.outsider.cli.home, "stolen.bin")); err == nil {
		return fmt.Errorf("a refused conch download left a file behind")
	}
	step("the file is refused to a non-member, to a request with no credential and to every agent")
	return nil
}

// threads shows replies (#25) through the API and the CLI: one level, read
// back in order, seen by whoever can see the root.
func (h *harness) threads() error {
	var root schema.PostMessageResponseV2
	if err := h.as(h.nick).call(http.MethodPost, "/v2/channels/ops/messages", map[string]any{"body": "which build goes out on Monday?"}, &root); err != nil {
		return err
	}
	rootID := root.Message.ID
	if _, err := h.ana.cli.run("", "send", "--reply-to", strconv.FormatInt(rootID, 10), "ops", "build 43, once the tide table is in"); err != nil {
		return err
	}
	var second schema.PostMessageResponseV2
	if err := h.as(h.nick).call(http.MethodPost, "/v2/channels/ops/messages", map[string]any{"body": "agreed", "thread_root_id": rootID}, &second); err != nil {
		return err
	}
	if second.Message.ThreadRootID != rootID {
		return fmt.Errorf("a reply posted over REST names root %d, want %d", second.Message.ThreadRootID, rootID)
	}
	// Something else in the channel, which is not part of the thread.
	if err := h.as(h.nick).call(http.MethodPost, "/v2/channels/ops/messages", map[string]any{"body": "unrelated"}, nil); err != nil {
		return err
	}

	wantBodies := []string{"which build goes out on Monday?", "build 43, once the tide table is in", "agreed"}
	threadPath := fmt.Sprintf("/v2/channels/ops/messages?thread=%d", rootID)
	for _, reader := range []*person{h.ana, h.leviathan} {
		var thread schema.ListMessagesResponseV2
		if err := h.as(reader).call(http.MethodGet, threadPath, nil, &thread); err != nil {
			return err
		}
		var got []string
		for i, m := range thread.Messages {
			if err := m.Validate(); err != nil {
				return err
			}
			if i == 0 && (m.ID != rootID || m.ThreadRootID != 0) || i > 0 && m.ThreadRootID != rootID {
				return fmt.Errorf("the thread as %s: message %d of the page has id %d and root %d, want the root first and then its replies", reader.name, i+1, m.ID, m.ThreadRootID)
			}
			got = append(got, m.Body)
		}
		if !reflect.DeepEqual(got, wantBodies) {
			return fmt.Errorf("the thread as %s has %d messages, want the root and its two replies in order", reader.name, len(got))
		}
	}
	out, err := h.ana.cli.run("", "thread", "ops", strconv.FormatInt(rootID, 10))
	if err != nil {
		return err
	}
	lines := nonEmptyLines(out)
	if len(lines) != len(wantBodies) {
		return fmt.Errorf("conch thread printed %d lines, want %d", len(lines), len(wantBodies))
	}
	for i, body := range wantBodies {
		marked := strings.Contains(lines[i], fmt.Sprintf("[reply:%d]", rootID))
		if !strings.Contains(lines[i], body) || marked != (i > 0) {
			return fmt.Errorf("conch thread line %d is not message %d of the thread, or is marked as a reply wrongly", i+1, i+1)
		}
	}
	step("a thread is posted and read back in order through REST and through conch send --reply-to and conch thread")

	// One level: a reply to a reply is refused. And a thread is as visible
	// as its channel.
	replyToReply := map[string]any{"body": "a reply to a reply", "thread_root_id": second.Message.ID}
	if _, err := h.as(h.nick).refused("a reply to a reply", http.MethodPost, "/v2/channels/ops/messages", replyToReply, http.StatusNotFound, "thread_not_found"); err != nil {
		return err
	}
	if _, err := h.as(h.outsider).refused("a non-member reads a thread", http.MethodGet, threadPath, nil, http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if _, err := h.as(h.nick).refused("a thread on a message that does not exist", http.MethodGet, "/v2/channels/ops/messages?thread=999999", nil, http.StatusNotFound, "thread_not_found"); err != nil {
		return err
	}
	step("threads are one level deep, and a thread is refused to whoever cannot read its channel")
	return nil
}

// principalReview shows that the review's report reaches Conch through a
// plain command (#26): ops/principal-review.sh post, run here by hand, with
// no scheduler, no Claude session and no GitHub call anywhere near it.
func (h *harness) principalReview() error {
	if runtime.GOOS != "linux" {
		h.skip("ops/principal-review.sh post", "the script is written for GNU tools and is run only on Linux, as its own tests are (e2e/reviewpost)")
		return nil
	}
	if _, err := h.createChannel("principal-review"); err != nil {
		return err
	}
	var err error
	if h.reviewer, err = h.newHuman("reviewer", "principal-review"); err != nil {
		return err
	}
	if err := h.addMember("principal-review", h.nick); err != nil {
		return err
	}
	report := filepath.Join(h.reviewer.cli.home, "2026-10-13-principal-review.md")
	const prURL = "https://example.invalid/njdaniel/conch/pull/1"
	text := "# Principal review 2026-10-13\n\n" +
		"## Invariant drift\n\n- none found\n\n" +
		"## Decisions needed from Nick\n\n- whether the gate report reads well\n"
	if err := os.WriteFile(report, []byte(text), 0o600); err != nil {
		return err
	}
	// Anything that posting must not need is replaced by a command that
	// records it was called and fails.
	shims := filepath.Join(h.root, "shims")
	if err := os.MkdirAll(shims, 0o700); err != nil {
		return err
	}
	called := filepath.Join(h.root, "shim-called")
	for _, tool := range []string{"systemctl", "systemd-run", "crontab", "claude", "gh"} {
		script := "#!/bin/sh\necho " + tool + " >> '" + called + "'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(shims, tool), []byte(script), 0o700); err != nil { // #nosec G306 -- a script this program runs must be executable
			return err
		}
	}
	ctx, cancel := context.WithTimeout(runCtx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "ops/principal-review.sh", "post", report, prURL) // #nosec G204 -- the repository's own script, with a report this program just wrote
	ownProcessGroup(cmd)                                                                      // the script runs conch; stopping it stops that too
	var env []string
	for _, kv := range h.reviewer.cli.env {
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env,
		"PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CONCH_BIN="+h.bin.conch,
		"CONCH_REVIEW_LOG_DIR="+filepath.Join(h.reviewer.cli.home, "review-state"),
		"CONCH_AGENT_LOG_DIR="+filepath.Join(h.reviewer.cli.home, "agent-state"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ops/principal-review.sh post: %w (%s)", err, firstLine(string(out)))
	}
	if data, err := os.ReadFile(called); err == nil { // #nosec G304 -- a path this program chose inside its own temp directory
		return fmt.Errorf("posting the report called %s; it must need none of them", strings.Join(strings.Fields(string(data)), ", "))
	}
	var list schema.ListMessagesResponseV2
	if err := h.as(h.nick).call(http.MethodGet, "/v2/channels/principal-review/messages", nil, &list); err != nil {
		return err
	}
	if len(list.Messages) != 1 {
		return fmt.Errorf("the principal-review channel has %d messages, want the one summary", len(list.Messages))
	}
	summary := list.Messages[0]
	for _, want := range []string{"Principal review 2026-10-13", "Decisions needed from Nick", "whether the gate report reads well", prURL} {
		if !strings.Contains(summary.Body, want) {
			return fmt.Errorf("the posted summary lacks %q", want)
		}
	}
	if summary.AuthorID != h.reviewer.id {
		return fmt.Errorf("the summary is by principal %d, want the reviewer's login, %d", summary.AuthorID, h.reviewer.id)
	}
	step("ops/principal-review.sh post puts a report's summary into Conch as the stored login, calling no scheduler, no Claude session and no gh")
	return nil
}

// auditAndHygiene shows audit export (#21) through the API and the CLI, that
// the trail records the refusals, and that no credential this run ever held
// is in the trail or in the server's log.
func (h *harness) auditAndHygiene() error {
	operator, err := h.newConchUser("gate-operator", h.proc.operatorToken)
	if err != nil {
		return err
	}
	stdout, stderr, err := operator.runSplit("", "audit", "export")
	if err != nil {
		return err
	}
	if strings.TrimSpace(stderr) != "" {
		return fmt.Errorf("conch audit export wrote %d bytes to standard error on success", len(stderr))
	}
	viaCLI, err := parseAudit([]byte(stdout))
	if err != nil {
		return fmt.Errorf("conch audit export: %w", err)
	}
	viaREST, err := h.audit()
	if err != nil {
		return err
	}
	if len(viaREST) < len(viaCLI) || !reflect.DeepEqual(viaREST[:len(viaCLI)], viaCLI) {
		return fmt.Errorf("conch audit export (%d events) is not the head of GET /v1/audit (%d events)", len(viaCLI), len(viaREST))
	}
	// Each export is recorded, by the operator who made it, and is not part
	// of itself: the CLI export is the first row the later REST one adds.
	if e := viaREST[len(viaCLI)]; e.Action != auditAuditExported || e.Actor != "principal:"+strconv.FormatInt(h.operatorID, 10) {
		return fmt.Errorf("the row after the CLI's export is %s by %s, want audit_exported by the operator", e.Action, e.Actor)
	}
	// --after and --limit resume an export where the last one stopped.
	head, _, err := operator.runSplit("", "audit", "export", "--limit", "10")
	if err != nil {
		return err
	}
	first, err := parseAudit([]byte(head))
	if err != nil {
		return fmt.Errorf("conch audit export --limit 10: %w", err)
	}
	if len(first) != 10 {
		return fmt.Errorf("conch audit export --limit 10 printed %d events", len(first))
	}
	rest, _, err := operator.runSplit("", "audit", "export", "--after", strconv.FormatInt(first[9].ID, 10), "--limit", "5")
	if err != nil {
		return err
	}
	next, err := parseAudit([]byte(rest))
	if err != nil {
		return fmt.Errorf("conch audit export --after %d --limit 5: %w", first[9].ID, err)
	}
	if len(next) != 5 || !reflect.DeepEqual(append(first, next...), viaCLI[:15]) {
		return fmt.Errorf("conch audit export --after %d --limit 5 printed %d events that do not continue the export", first[9].ID, len(next))
	}
	step("the audit trail exports as JSON Lines in id order, the same through GET /v1/audit and conch audit export, resumable, and each export is recorded")

	if _, err := h.as(h.nick).refused("a member exports the audit trail", http.MethodGet, "/v1/audit", nil, http.StatusForbidden, "forbidden"); err != nil {
		return err
	}
	out, errOut, err := h.nick.cli.runSplit("", "audit", "export")
	if err == nil || strings.TrimSpace(out) != "" || len(nonEmptyLines(errOut)) != 1 {
		return fmt.Errorf("conch audit export as a member: refused: %v, with %d bytes on standard output and %d lines on standard error; want a refusal in one line and nothing else", err != nil, len(out), len(nonEmptyLines(errOut)))
	}
	step("only an operator may export it")

	// The trail has everything this run did that mattered: the approvals in
	// order (checked as they happened), every refusal, every credential.
	final, err := h.audit()
	if err != nil {
		return err
	}
	actions := map[string]int{}
	for _, e := range final {
		actions[e.Action]++
	}
	for _, action := range []string{auditAccessDenied, auditApprovalCreated, auditDecisionCast, auditApprovalResolved, auditNotifySent, auditNotifyFailed, auditAuditExported, auditAgentRateLimited, "credential_created", "credential_rotated", "credential_revoked"} {
		if actions[action] == 0 {
			return fmt.Errorf("the audit trail has no %s row", action)
		}
	}

	// No credential anywhere it must not be. The export is searched as the
	// bytes the server sent, and the log as the bytes conchd wrote.
	resp, err := h.op().send(http.MethodGet, "/v1/audit", nil)
	if err != nil {
		return err
	}
	log, err := h.serverLog()
	if err != nil {
		return err
	}
	secrets := h.allSecrets()
	if len(secrets) < 12 {
		return fmt.Errorf("this run recorded only %d credentials; the search below would prove little", len(secrets))
	}
	for i, secret := range secrets {
		if bytes.Contains(resp.body, []byte(secret)) {
			return fmt.Errorf("credential %d of %d that this run held is in the audit export", i+1, len(secrets))
		}
		if strings.Contains(log, secret) {
			return fmt.Errorf("credential %d of %d that this run held is in conchd's log", i+1, len(secrets))
		}
	}
	step("none of the %d credentials this run held (issued, rotated out, revoked, the operator's, P1's webhook token) is in the audit export or in conchd's log", len(secrets))
	return h.checkServerLog(log)
}

// checkServerLog reads what conchd logged during the run. A refusal is not an
// error, and conchd must not have logged one as such: the only ERROR lines a
// correct run produces are the two about the approval raised and decided
// while this run had ntfy down.
func (h *harness) checkServerLog(log string) error {
	var unexpected []string
	failedNotifications := 0
	about := fmt.Sprintf("notification failed approval=%d ", h.degraded)
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, " ERROR ") {
			continue
		}
		if strings.Contains(line, about) {
			failedNotifications++
			continue
		}
		unexpected = append(unexpected, line)
	}
	if len(unexpected) > 0 {
		return fmt.Errorf("conchd logged %d errors in a run where nothing went wrong; the first: %s", len(unexpected), unexpected[0])
	}
	// The two it must have logged also show that this search can see an
	// error line at all.
	if failedNotifications != 2 {
		return fmt.Errorf("conchd logged %d errors about approval %d's notifications, want 2 (created and resolved, with ntfy down)", failedNotifications, h.degraded)
	}
	step("conchd logged two errors, both about the notifications it could not send while ntfy was down, and no other")
	return nil
}
