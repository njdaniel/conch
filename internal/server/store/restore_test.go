package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// restoreWorld is a server's database as a backup holds it (issue #716): an
// operator with a password and a confirmed second factor, a member signed
// in with a password and holding a recent-auth proof, an agent with a
// script credential, a second operator with a setup token, a sign-in link,
// a pending operator sign-in, and a guest whose visit is active, in a
// channel with a post of its own.
type restoreWorld struct {
	*loginWorld
	agent, op2            Principal
	agentTok, opTok       string
	memberSession         IssuedLogin
	guest                 CreatedGuest
	guestPost             int64
	linkCode, setupTok    string
	challenge             string
	channel               Channel
	inviteCode            string
	memberPost, totpStepB int64
}

func newRestoreWorld(t *testing.T) *restoreWorld {
	t.Helper()
	ctx := context.Background()
	w := &restoreWorld{loginWorld: newLoginWorld(t)}
	var err error
	if _, w.opTok, err = w.s.CreateCredential(ctx, "system", w.op.ID, "script", nil); err != nil {
		t.Fatal(err)
	}
	if w.agent, err = w.s.CreatePrincipal(ctx, PrincipalAgent, "bot"); err != nil {
		t.Fatal(err)
	}
	if _, w.agentTok, err = w.s.CreateCredential(ctx, "system", w.agent.ID, "agent", nil); err != nil {
		t.Fatal(err)
	}
	if w.memberSession, err = w.s.IssueLoginCredential(ctx, w.issue(t, w.member), w.now); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.RecordRecentAuth(ctx, w.issue(t, w.member), w.memberSession.Credential.ID, w.now); err != nil {
		t.Fatal(err)
	}
	link, err := w.s.CreateSignInLink(ctx, principalActor(w.op.ID), w.op.ID, w.member.ID, schema.CreateSignInLinkRequestV1{}, w.now)
	if err != nil {
		t.Fatal(err)
	}
	w.linkCode = link.Code
	w.challenge = w.loginWorld.challenge(t)
	if w.op2, err = w.s.CreatePrincipal(ctx, PrincipalHuman, "second"); err != nil {
		t.Fatal(err)
	}
	w.exec(t, "UPDATE principals SET role = 'operator' WHERE id = ?", w.op2.ID)
	setup, err := w.s.IssueSetupToken(ctx, w.op2.ID, w.now)
	if err != nil {
		t.Fatal(err)
	}
	w.setupTok = setup.Token
	if w.channel, err = w.s.CreateChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.AddChannelMember(ctx, "system", w.channel.ID, w.member.ID, 0); err != nil {
		t.Fatal(err)
	}
	if w.guest, err = w.s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{w.channel.ID}, Idle: time.Hour, Absolute: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	m, err := w.s.InsertMessage(ctx, w.channel.ID, w.guest.Principal.ID, "hello from a guest")
	if err != nil {
		t.Fatal(err)
	}
	w.guestPost = m.ID
	if m, err = w.s.InsertMessage(ctx, w.channel.ID, w.member.ID, "hello from a member"); err != nil {
		t.Fatal(err)
	}
	w.memberPost = m.ID
	if _, err := w.s.CreateHook(ctx, "system", "a-hook-token-for-the-restore-test", w.channel.ID, w.agent.ID); err != nil {
		t.Fatal(err)
	}
	// Guests may join from the bare address and through an open invite.
	both, on, channels := schema.OpenJoiningBoth, true, []string{"ops"}
	if _, err := w.s.PutJoinSettings(ctx, "system", w.op.ID, schema.PutJoinSettingsRequestV1{
		GuestsEnabled: &on, OpenJoining: &both, OpenChannels: &channels}); err != nil {
		t.Fatal(err)
	}
	inv, err := w.s.CreateInvite(ctx, principalActor(w.op.ID), w.op.ID,
		schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"ops"}, JoinAs: schema.InviteJoinBoth}, InviteOptions{}, w.now)
	if err != nil {
		t.Fatal(err)
	}
	w.inviteCode = inv.Code
	// The operator signed in with a code once before the backup.
	w.totpStepB = totpFloor(w.now) - schema.TOTPWindowSteps - 10
	w.exec(t, "UPDATE totp SET last_step = ? WHERE principal_id = ?", w.totpStepB, w.op.ID)
	return w
}

// backup is the database as it is now, in a file of its own: what an
// online backup would hold.
func backup(t *testing.T, s *Store) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conch.db")
	if _, err := s.db.Exec("VACUUM INTO ?", path); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// As conch-backup's archive is unpacked: private to its owner.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// restored opens the backup at path as a restore would, before conchd
// restore has run on it.
func restored(t *testing.T, w *restoreWorld, path string) *restoreWorld {
	t.Helper()
	r := *w
	lw := *w.loginWorld
	lw.s = openTestStoreAt(t, path)
	r.loginWorld = &lw
	return &r
}

func (w *restoreWorld) live(t *testing.T, token string) bool {
	t.Helper()
	_, err := w.s.ResolveCredential(context.Background(), token)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrCredentialInvalid):
		return false
	default:
		t.Fatalf("ResolveCredential: %v", err)
		return false
	}
}

func (w *restoreWorld) tokens() map[string]string {
	return map[string]string{
		"operator script credential": w.opTok,
		"agent credential":           w.agentTok,
		"member session":             w.memberSession.Token,
		"guest credential":           w.guest.Token,
	}
}

// A backup taken before a guest ended its visit, before a credential was
// revoked and before a link was used, restored: the backup holds all of them
// as live, and after InvalidateRestored none of them works, nor does
// anything else the backup could sign someone in with. The live database
// the backup came from is not touched by any of it.
func TestInvalidateRestoredBackup(t *testing.T) {
	ctx := context.Background()
	src := newRestoreWorld(t)
	path := backup(t, src.s)

	// After the backup, on the live server: the guest ends its visit, the
	// member's session is revoked, the link is used, and the operator signs
	// in with a later code.
	if _, err := src.s.EndGuestVisit(ctx, principalActor(src.guest.Principal.ID), src.guest.Principal.ID, schema.GuestEndVisit); err != nil {
		t.Fatal(err)
	}
	if err := src.s.RevokeCredential(ctx, "system", src.memberSession.Credential.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := src.s.RedeemSignInLink(ctx, HashSignInLinkCode(src.linkCode), src.now); err != nil {
		t.Fatal(err)
	}
	usedStep := src.totpStepB + 5
	src.exec(t, "UPDATE totp SET last_step = ? WHERE principal_id = ?", usedStep, src.op.ID)

	r := restored(t, src, path)
	// The backup, as it was restored, would bring every one of them back.
	for what, tok := range r.tokens() {
		if !r.live(t, tok) {
			t.Fatalf("the %s is not live in the backup; the test proves nothing", what)
		}
	}
	if _, err := r.s.PreviewSignInLink(ctx, HashSignInLinkCode(r.linkCode), r.now); err != nil {
		t.Fatalf("the link is not usable in the backup: %v", err)
	}

	var announcedAuth []int64
	var announcedGuests []EndedGuest
	r.s.OnAuthChanged(func(ids []int64) { announcedAuth = append(announcedAuth, ids...) })
	r.s.OnGuestsEnded(func(g []EndedGuest) { announcedGuests = append(announcedGuests, g...) })
	res, err := r.s.InvalidateRestored(ctx, r.now)
	if err != nil {
		t.Fatalf("InvalidateRestored: %v", err)
	}
	var principals int
	if err := r.s.db.QueryRow("SELECT COUNT(*) FROM principals").Scan(&principals); err != nil {
		t.Fatal(err)
	}
	if len(res.Principals) != principals || len(res.GuestsEnded) != 1 || res.GuestsEnded[0].PrincipalID != r.guest.Principal.ID ||
		res.GuestsEnded[0].Reason != schema.GuestEndRestore || res.SetupTokens != 1 || res.TOTPFloor != totpFloor(r.now) ||
		!res.InvitesPaused || res.Hooks != 1 {
		t.Errorf("result %+v, want %d principals, the guest, one setup token, invites paused, one hook", res, principals)
	}
	// An invite closed since the backup is open in it: no invite works
	// until an operator has looked.
	if paused, err := r.s.InvitesPaused(ctx); err != nil || !paused {
		t.Errorf("invites paused = %v, %v after the restore", paused, err)
	}
	if !res.JoiningPaused {
		t.Errorf("the restore did not report pausing guest joining: %+v", res)
	}
	// Joining turned off since the backup may be on in it: no visit starts
	// from the bare address until an operator has looked, nor through an
	// invite, since invites are paused too. Guests stay enabled: that is
	// the operator's choice, not the restore's.
	if _, err := r.s.JoinAsGuest(ctx, GuestEntry{}); !errors.Is(err, ErrJoiningPaused) {
		t.Errorf("a guest joining from the bare address after the restore = %v, want ErrJoiningPaused", err)
	}
	if _, err := r.s.RedeemInviteGuest(ctx, HashInviteCode(r.inviteCode), GuestEntry{}, r.now); !errors.Is(err, ErrInvitesPaused) {
		t.Errorf("a guest joining through an invite after the restore = %v, want ErrInvitesPaused", err)
	}
	if set, _, err := r.s.JoinSettings(ctx); err != nil || !set.JoiningPaused || !set.GuestsEnabled {
		t.Errorf("join settings after the restore = %+v, %v; want paused, guests still enabled", set, err)
	}
	if len(announcedAuth) != principals || len(announcedGuests) != 1 {
		t.Errorf("announced %d principals and %d guests after the commit", len(announcedAuth), len(announcedGuests))
	}

	for what, tok := range r.tokens() {
		if r.live(t, tok) {
			t.Errorf("the %s still works after the restore", what)
		}
	}
	if _, err := r.s.RedeemSignInLink(ctx, HashSignInLinkCode(r.linkCode), r.now); !errors.Is(err, ErrSignInLinkInvalid) {
		t.Errorf("the link = %v after the restore, want ErrSignInLinkInvalid", err)
	}
	if _, err := r.s.PendingLoginOf(ctx, r.challenge, r.now); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("the pending sign-in = %v after the restore, want ErrChallengeInvalid", err)
	}
	if _, err := r.s.RedeemSetupToken(ctx, HashSetupToken(r.setupTok), r.now); !errors.Is(err, ErrSetupTokenInvalid) {
		t.Errorf("the setup token = %v after the restore, want ErrSetupTokenInvalid", err)
	}
	if stands, err := r.s.RecentAuthStands(ctx, r.memberSession.Credential.ID, r.now); err != nil || stands {
		t.Errorf("the member's proof stands = %v, %v after the restore", stands, err)
	}
	var proofs, tokens int
	if err := r.s.db.QueryRow("SELECT (SELECT COUNT(*) FROM credentials WHERE recent_auth_until IS NOT NULL), (SELECT COUNT(*) FROM setup_tokens)").Scan(&proofs, &tokens); err != nil {
		t.Fatal(err)
	}
	if proofs != 0 || tokens != 0 {
		t.Errorf("%d proofs and %d setup tokens left", proofs, tokens)
	}

	// The guest's visit ended for good, its post keeps its attribution, and
	// it is in no channel; the member's post and membership are there.
	v, err := r.s.GuestVisitOf(ctx, r.guest.Principal.ID)
	if err != nil || v.State != schema.GuestStateEnded || v.EndReason != schema.GuestEndRestore {
		t.Errorf("the visit is %+v, %v; want ended, reason restore", v, err)
	}
	var handle string
	if err := r.s.db.QueryRow("SELECT handle FROM message_guest_attribution WHERE message_id = ?", r.guestPost).Scan(&handle); err != nil || handle != r.guest.Visit.Handle {
		t.Errorf("the guest's post is attributed to %q, %v; want %q", handle, err, r.guest.Visit.Handle)
	}
	if in, _ := r.s.IsChannelMember(ctx, r.channel.ID, r.guest.Principal.ID); in {
		t.Error("the ended guest is still a member")
	}
	if in, _ := r.s.IsChannelMember(ctx, r.channel.ID, r.member.ID); !in {
		t.Error("the member lost their membership")
	}
	var author int64
	if err := r.s.db.QueryRow("SELECT author_id FROM messages WHERE id = ?", r.memberPost).Scan(&author); err != nil || author != r.member.ID {
		t.Errorf("the member's post is by %d, %v", author, err)
	}

	// The member signs in again with the password the backup kept, and the
	// session that earns is live.
	again, err := r.s.IssueLoginCredential(ctx, r.issue(t, r.member), r.now)
	if err != nil || !r.live(t, again.Token) {
		t.Fatalf("the member's sign-in after the restore = %v", err)
	}

	// The operator's password and second factor are kept, but no code that
	// may already have been used is accepted: the step the live server took
	// after the backup, and every step up to now and the window after it.
	for _, step := range []int64{usedStep, res.TOTPFloor - 2, res.TOTPFloor - 1, res.TOTPFloor} {
		hash := HashChallenge(mustPending(t, r.loginWorld, r.now).Challenge)
		if _, err := r.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: stepCheck(step)}, r.now); !errors.Is(err, ErrSecondFactorWrong) {
			t.Errorf("a code of step %d (floor %d) = %v, want ErrSecondFactorWrong", step, res.TOTPFloor, err)
		}
	}
	hash := HashChallenge(mustPending(t, r.loginWorld, r.now).Challenge)
	if _, err := r.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: stepCheck(res.TOTPFloor + 1)}, r.now); err != nil {
		t.Errorf("the first code after the floor = %v", err)
	}

	// The trail says what happened, with no secret in it.
	counts := map[string]int{}
	for _, e := range allAuditEvents(t, r.s) {
		switch {
		case e.Action == AuditRestoreInvalidated:
			counts[e.Action]++
			want := fmt.Sprintf("principals=%d guests_ended=1 setup_tokens=1 totp_floor_step=%d", principals, res.TOTPFloor)
			if e.Actor != hostActor || e.Detail != want {
				t.Errorf("restore_invalidated by %q: %q, want %q", e.Actor, e.Detail, want)
			}
		case e.Action == AuditAuthGenerationBumped && strings.HasPrefix(e.Detail, "cause=restore "):
			counts["bumped"]++
		case e.Action == AuditGuestEnded && e.Detail == "reason=restore":
			counts[e.Action]++
		case e.Action == AuditInvitesPaused && e.Actor == hostActor:
			counts[e.Action]++
		case e.Action == AuditJoinSettingsChanged && e.Actor == hostActor && e.Detail == "joining_paused=false->true":
			counts[e.Action]++
		}
		for _, secret := range []string{r.opTok, r.agentTok, r.memberSession.Token, r.guest.Token, r.linkCode, r.setupTok} {
			if strings.Contains(e.Detail, secret) || strings.Contains(e.Subject, secret) {
				t.Errorf("audit %s holds a secret", e.Action)
			}
		}
	}
	if counts[AuditRestoreInvalidated] != 1 || counts["bumped"] != principals || counts[AuditGuestEnded] != 1 || counts[AuditInvitesPaused] != 1 ||
		counts[AuditJoinSettingsChanged] != 1 {
		t.Errorf("audit counts %v, want one restore_invalidated, %d bumps, one guest_ended, one invites_paused, one join_settings_changed", counts, principals)
	}

	// The live database is as the live server left it.
	if !src.live(t, src.opTok) || !src.live(t, src.agentTok) {
		t.Error("restoring a copy touched the live database's credentials")
	}
	var lastStep int64
	if err := src.s.db.QueryRow("SELECT last_step FROM totp WHERE principal_id = ?", src.op.ID).Scan(&lastStep); err != nil || lastStep != usedStep {
		t.Errorf("the live database's last_step = %d, %v; want %d", lastStep, err, usedStep)
	}
	for _, e := range allAuditEvents(t, src.s) {
		if e.Action == AuditRestoreInvalidated {
			t.Error("the live database records a restore")
		}
	}
}

// mustPending is a pending sign-in of w's operator, made at now.
func mustPending(t *testing.T, w *loginWorld, now time.Time) PendingLogin {
	t.Helper()
	p, err := w.s.CreatePendingLogin(context.Background(), w.issue(t, w.op), "addr", now)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A restore that fails part-way writes nothing: every credential, visit and
// token the backup holds is as it was, and nothing says the database was
// restored, so serve is still not to be started on it. Once the cause is
// gone, running it again finishes it.
func TestInvalidateRestoredAllOrNothing(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name, trigger string
	}{
		{"while ending a visit", `CREATE TRIGGER fail_restore BEFORE UPDATE ON guest_visits WHEN NEW.end_reason = 'restore'
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
		{"while moving a generation on", `CREATE TRIGGER fail_restore BEFORE UPDATE OF auth_generation ON principals WHEN NEW.id = (SELECT MAX(id) FROM principals)
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
		{"while deleting the setup tokens", `CREATE TRIGGER fail_restore BEFORE DELETE ON setup_tokens
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
		{"at the last audit row", `CREATE TRIGGER fail_restore BEFORE INSERT ON audit_events WHEN NEW.action = 'restore_invalidated'
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := newRestoreWorld(t)
			r := restored(t, src, backup(t, src.s))
			r.exec(t, tt.trigger)
			before := len(allAuditEvents(t, r.s))
			if _, err := r.s.InvalidateRestored(ctx, r.now); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("InvalidateRestored = %v, want the injected failure", err)
			}
			for what, tok := range r.tokens() {
				if !r.live(t, tok) {
					t.Errorf("the %s changed although the restore failed", what)
				}
			}
			if v, _ := r.s.GuestVisitOf(ctx, r.guest.Principal.ID); v.State != schema.GuestStateActive {
				t.Errorf("the visit is %s although the restore failed", v.State)
			}
			if got := len(allAuditEvents(t, r.s)); got != before {
				t.Errorf("%d audit rows written by a failed restore", got-before)
			}
			if _, restoredMark, err := r.s.StartRestored(ctx, r.now); err != nil || restoredMark {
				t.Errorf("StartRestored after a failed restore = %v, %v", restoredMark, err)
			}
			r.exec(t, "DROP TRIGGER fail_restore")
			if _, err := r.s.InvalidateRestored(ctx, r.now); err != nil {
				t.Fatalf("the restore run again = %v", err)
			}
			for what, tok := range r.tokens() {
				if r.live(t, tok) {
					t.Errorf("the %s works after the restore was run again", what)
				}
			}
		})
	}
}

// Running the restore twice, as after an interruption whose outcome nobody
// saw, is harmless: the second run moves every generation on again and finds
// no visit left to end, and the member still signs in with their password.
func TestInvalidateRestoredTwice(t *testing.T) {
	ctx := context.Background()
	src := newRestoreWorld(t)
	r := restored(t, src, backup(t, src.s))
	first, err := r.s.InvalidateRestored(ctx, r.now)
	if err != nil {
		t.Fatal(err)
	}
	after, err := r.s.IssueLoginCredential(ctx, r.issue(t, r.member), r.now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.s.InvalidateRestored(ctx, r.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("the second run = %v", err)
	}
	if len(second.Principals) != len(first.Principals) || len(second.GuestsEnded) != 0 || second.SetupTokens != 0 || second.InvitesPaused || second.JoiningPaused {
		t.Errorf("the second run did %+v", second)
	}
	if r.live(t, after.Token) {
		t.Error("a session from between the two runs survived the second")
	}
	if _, err := r.s.IssueLoginCredential(ctx, r.issue(t, r.member), r.now); err != nil {
		t.Errorf("the member's sign-in after two runs = %v", err)
	}
	var generation int64
	if err := r.s.db.QueryRow("SELECT auth_generation FROM principals WHERE id = ?", r.agent.ID).Scan(&generation); err != nil || generation != 2 {
		t.Errorf("the agent's generation = %d, %v; want 2", generation, err)
	}
}

// The TOTP floor is the current step and the window after it, and a floor
// never lowers a step already taken: an operator whose last code was later
// keeps it.
func TestRestoreTOTPFloor(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1_800_000_015, 0) // 15 s into step 60_000_000
	for _, tt := range []struct {
		name      string
		lastStep  int64
		wantAfter int64
	}{
		{"an old code", 59_999_000, 60_000_001},
		{"a code of this step", 60_000_000, 60_000_001},
		{"a later step already taken", 60_000_005, 60_000_005},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newLoginWorld(t)
			w.exec(t, "UPDATE totp SET last_step = ? WHERE principal_id = ?", tt.lastStep, w.op.ID)
			res, err := w.s.InvalidateRestored(ctx, at)
			if err != nil {
				t.Fatal(err)
			}
			if res.TOTPFloor != 60_000_001 {
				t.Errorf("floor = %d, want 60000001", res.TOTPFloor)
			}
			var got int64
			if err := w.s.db.QueryRow("SELECT last_step FROM totp WHERE principal_id = ?", w.op.ID).Scan(&got); err != nil || got != tt.wantAfter {
				t.Errorf("last_step = %d, %v; want %d", got, err, tt.wantAfter)
			}
		})
	}
}

// The first start on a restored database refuses, from its own clock, every
// code up to its step and the window after it, and removes the mark; every
// later start, and every start on a database that was never restored, does
// nothing and writes nothing.
func TestStartRestored(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	before := len(allAuditEvents(t, w.s))
	if floor, restoredMark, err := w.s.StartRestored(ctx, w.now); err != nil || restoredMark || floor != 0 {
		t.Fatalf("StartRestored on a database never restored = %d, %v, %v", floor, restoredMark, err)
	}
	if got := len(allAuditEvents(t, w.s)); got != before {
		t.Errorf("an ordinary start wrote %d audit rows", got-before)
	}

	restoredAt := w.now
	if _, err := w.s.InvalidateRestored(ctx, restoredAt); err != nil {
		t.Fatal(err)
	}
	// Started ten minutes after the restore: the floor moves to the start.
	start := restoredAt.Add(10 * time.Minute)
	floor, restoredMark, err := w.s.StartRestored(ctx, start)
	if err != nil || !restoredMark || floor != totpFloor(start) {
		t.Fatalf("the first start = %d, %v, %v; want %d, true", floor, restoredMark, err, totpFloor(start))
	}
	var lastStep int64
	if err := w.s.db.QueryRow("SELECT last_step FROM totp WHERE principal_id = ?", w.op.ID).Scan(&lastStep); err != nil || lastStep != floor {
		t.Errorf("last_step = %d, %v; want %d", lastStep, err, floor)
	}
	// For the first 2 × 30 s every code is refused; then the current one
	// is accepted.
	startStep := start.Unix() / schema.TOTPPeriodSeconds
	for step := startStep - schema.TOTPWindowSteps; step <= startStep+schema.TOTPWindowSteps; step++ {
		hash := HashChallenge(mustPending(t, w, start).Challenge)
		if _, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: stepCheck(step)}, start); !errors.Is(err, ErrSecondFactorWrong) {
			t.Errorf("a code of step %d at the start = %v, want refused", step, err)
		}
	}
	later := start.Add(2 * schema.TOTPPeriodSeconds * time.Second)
	hash := HashChallenge(mustPending(t, w, later).Challenge)
	if _, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: stepCheck(later.Unix() / schema.TOTPPeriodSeconds)}, later); err != nil {
		t.Errorf("the current code 2 × 30 s after the start = %v", err)
	}
	var started int
	for _, e := range allAuditEvents(t, w.s) {
		if e.Action == AuditRestoreStarted {
			started++
			if e.Detail != fmt.Sprintf("totp_floor_step=%d voice_rooms_retired=0", floor) {
				t.Errorf("restore_started detail %q", e.Detail)
			}
		}
	}
	if started != 1 {
		t.Errorf("%d restore_started rows, want 1", started)
	}
	before = len(allAuditEvents(t, w.s))
	if _, restoredMark, err := w.s.StartRestored(ctx, later); err != nil || restoredMark {
		t.Errorf("the second start = %v, %v; want nothing to do", restoredMark, err)
	}
	if got := len(allAuditEvents(t, w.s)); got != before {
		t.Errorf("the second start wrote %d audit rows", got-before)
	}
}

// The first start on restored data retires every live voice room and drops
// its holders, whatever prefix the room was made under and whether or not
// anyone is recorded in it (issue #782): the backup cannot say who joined
// afterwards. A room retired before keeps its time, the count is audited
// without names, and the channel's next room is a new one under the current
// prefix. An ordinary start retires nothing.
func TestStartRestoredRetiresVoiceRooms(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	if err := w.s.SetVoiceRoomPrefix("old-"); err != nil {
		t.Fatal(err)
	}
	var chans []Channel
	for _, name := range []string{"empty", "held", "rotated"} {
		ch, err := w.s.CreateChannel(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		chans = append(chans, ch)
	}
	empty, err := w.s.ChannelVoiceRoom(ctx, chans[0].ID) // no holders, earlier prefix
	if err != nil {
		t.Fatal(err)
	}
	held, err := w.s.ChannelVoiceRoom(ctx, chans[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := w.s.CreateCredential(ctx, "system", w.member.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.s.RecordVoiceHolder(ctx, held.ID, w.member.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	first, err := w.s.ChannelVoiceRoom(ctx, chans[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := w.s.RotateVoiceRoom(ctx, first.ID, VoiceRotateMemberRemoved); err != nil || !ok {
		t.Fatalf("rotate: %v %v", ok, err)
	}
	if err := w.s.SetVoiceRoomPrefix("new-"); err != nil {
		t.Fatal(err)
	}
	retiredAt := func(id int64) time.Time {
		t.Helper()
		rooms, err := w.s.ListRetiredVoiceRooms(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rooms {
			if r.ID == id {
				return r.RetiredAt
			}
		}
		return time.Time{}
	}
	firstRetired := retiredAt(first.ID)
	liveBefore, err := w.s.ListVoiceRooms(ctx)
	if err != nil || len(liveBefore) != 3 {
		t.Fatalf("live rooms before = %d, %v; want 3", len(liveBefore), err)
	}

	// An ordinary start: nothing to do.
	if _, restoredMark, err := w.s.StartRestored(ctx, w.now); err != nil || restoredMark {
		t.Fatalf("an ordinary start = %v, %v", restoredMark, err)
	}
	if live, err := w.s.ListVoiceRooms(ctx); err != nil || len(live) != 3 {
		t.Fatalf("live rooms after an ordinary start = %d, %v; want 3", len(live), err)
	}

	if _, err := w.s.InvalidateRestored(ctx, w.now); err != nil {
		t.Fatal(err)
	}
	start := w.now.Add(time.Minute).Truncate(time.Millisecond)
	if _, restoredMark, err := w.s.StartRestored(ctx, start); err != nil || !restoredMark {
		t.Fatalf("the first start = %v, %v", restoredMark, err)
	}
	if live, err := w.s.ListVoiceRooms(ctx); err != nil || len(live) != 0 {
		t.Errorf("live rooms after the first start = %d, %v; want none", len(live), err)
	}
	for _, r := range liveBefore {
		if got := retiredAt(r.ID); !got.Equal(start) {
			t.Errorf("room %d retired at %v, want the start (%v)", r.ID, got, start)
		}
		holders, err := w.s.VoiceRoomHolders(ctx, r.ID)
		if err != nil || len(holders) != 0 {
			t.Errorf("room %d holders = %d, %v; want none", r.ID, len(holders), err)
		}
	}
	if got := retiredAt(first.ID); !got.Equal(firstRetired) {
		t.Errorf("a room retired before the start moved from %v to %v", firstRetired, got)
	}
	var detail string
	for _, e := range allAuditEvents(t, w.s) {
		if e.Action == AuditRestoreStarted {
			detail = e.Detail
		}
	}
	if !strings.HasSuffix(detail, " voice_rooms_retired=3") {
		t.Errorf("restore_started detail %q, want voice_rooms_retired=3", detail)
	}
	for _, r := range liveBefore {
		if strings.Contains(detail, r.RoomName) {
			t.Error("restore_started names a room")
		}
	}
	next, err := w.s.ChannelVoiceRoom(ctx, chans[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if next.RoomName == empty.RoomName || next.ID == empty.ID || !strings.HasPrefix(next.RoomName, "new-") {
		t.Errorf("the channel's next room = %d %q; want a new room under the current prefix", next.ID, next.RoomName)
	}
	if err := w.s.RecordVoiceHolder(ctx, held.ID, w.member.ID, cred.ID); !errors.Is(err, ErrVoiceRoomRetired) {
		t.Errorf("recording a holder in a room the start retired = %v, want ErrVoiceRoomRetired", err)
	}
}

// A backup in which invites and guest joining were already paused: the
// restore leaves them paused and writes nothing about either, and the entry
// points stay closed.
func TestInvalidateRestoredAlreadyPaused(t *testing.T) {
	ctx := context.Background()
	src := newRestoreWorld(t)
	paused := true
	if _, err := src.s.PutJoinSettings(ctx, "system", src.op.ID, schema.PutJoinSettingsRequestV1{JoiningPaused: &paused}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.s.SetInvitesPaused(ctx, "system", true); err != nil {
		t.Fatal(err)
	}
	r := restored(t, src, backup(t, src.s))
	before := len(allAuditEvents(t, r.s))
	res, err := r.s.InvalidateRestored(ctx, r.now)
	if err != nil {
		t.Fatal(err)
	}
	if res.InvitesPaused || res.JoiningPaused {
		t.Errorf("the restore says it paused what was paused already: %+v", res)
	}
	for _, e := range allAuditEvents(t, r.s)[before:] {
		if e.Action == AuditInvitesPaused || e.Action == AuditJoinSettingsChanged {
			t.Errorf("the restore wrote %s %q for a switch already paused", e.Action, e.Detail)
		}
	}
	if _, err := r.s.JoinAsGuest(ctx, GuestEntry{}); !errors.Is(err, ErrJoiningPaused) {
		t.Errorf("joining = %v, want ErrJoiningPaused", err)
	}
	if p, err := r.s.InvitesPaused(ctx); err != nil || !p {
		t.Errorf("invites paused = %v, %v", p, err)
	}
}
