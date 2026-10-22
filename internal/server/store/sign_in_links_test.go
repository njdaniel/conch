package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The sign-in link tests reuse the invite fixture: op is the operator, alice
// and bob are members.

func (f *inviteFixture) makeSignInLink(creator, target Principal, req schema.CreateSignInLinkRequestV1) (schema.CreateSignInLinkResponseV1, error) {
	f.t.Helper()
	return f.s.CreateSignInLink(f.ctx, principalActor(creator.ID), creator.ID, target.ID, req, f.now)
}

func (f *inviteFixture) mustSignInLink(target Principal) schema.CreateSignInLinkResponseV1 {
	f.t.Helper()
	resp, err := f.makeSignInLink(f.op, target, schema.CreateSignInLinkRequestV1{})
	if err != nil {
		f.t.Fatalf("CreateSignInLink: %v", err)
	}
	return resp
}

func (f *inviteFixture) useSignInLink(code string, at time.Time) (RedeemedSignInLink, error) {
	f.t.Helper()
	return f.s.RedeemSignInLink(f.ctx, HashSignInLinkCode(code), at)
}

func (f *inviteFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.s.db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func TestCreateSignInLinkRules(t *testing.T) {
	at := func(f *inviteFixture, d time.Duration) *schema.Timestamp {
		ts := schema.NewTimestamp(f.now.Add(d))
		return &ts
	}
	tests := []struct {
		name    string
		setup   func(f *inviteFixture) (creator, target Principal)
		req     func(f *inviteFixture) schema.CreateSignInLinkRequestV1
		wantErr error
	}{
		{"an operator, for a member", func(f *inviteFixture) (Principal, Principal) { return f.op, f.alice }, nil, nil},
		{"a member may not", func(f *inviteFixture) (Principal, Principal) { return f.alice, f.bob }, nil, ErrSignInLinkNotAllowed},
		{"a disabled operator may not", func(f *inviteFixture) (Principal, Principal) {
			f.exec("UPDATE principals SET role = 'operator', disabled_at = 1 WHERE id = ?", f.bob.ID)
			return f.bob, f.alice
		}, nil, ErrSignInLinkNotAllowed},
		{"a maker who does not exist", func(f *inviteFixture) (Principal, Principal) { return Principal{ID: 9999}, f.alice }, nil, ErrSignInLinkNotAllowed},
		{"for an agent", func(f *inviteFixture) (Principal, Principal) {
			bot, err := f.s.CreatePrincipal(f.ctx, PrincipalAgent, "bot")
			if err != nil {
				f.t.Fatal(err)
			}
			return f.op, bot
		}, nil, ErrSignInLinkTarget},
		{"for an operator", func(f *inviteFixture) (Principal, Principal) {
			f.exec("UPDATE principals SET role = 'operator' WHERE id = ?", f.bob.ID)
			return f.op, f.bob
		}, nil, ErrSignInLinkTarget},
		{"for the operator themselves", func(f *inviteFixture) (Principal, Principal) { return f.op, f.op }, nil, ErrSignInLinkTarget},
		{"for a disabled member", func(f *inviteFixture) (Principal, Principal) {
			if _, err := f.s.DisablePrincipal(f.ctx, "system", f.bob.ID); err != nil {
				f.t.Fatal(err)
			}
			return f.op, f.bob
		}, nil, ErrPrincipalDisabled},
		{"for a hidden member", func(f *inviteFixture) (Principal, Principal) {
			if _, err := f.s.HidePrincipal(f.ctx, "system", f.bob.ID, HideOptions{}); err != nil {
				f.t.Fatal(err)
			}
			return f.op, f.bob
		}, nil, ErrPrincipalHidden},
		{"for nobody", func(f *inviteFixture) (Principal, Principal) { return f.op, Principal{ID: 9999} }, nil, ErrPrincipalNotFound},
		{"an expiry of a week", func(f *inviteFixture) (Principal, Principal) { return f.op, f.alice },
			func(f *inviteFixture) schema.CreateSignInLinkRequestV1 {
				return schema.CreateSignInLinkRequestV1{ExpiresAt: at(f, schema.MaxSignInLinkLifetime)}
			}, nil},
		{"an expiry past a week", func(f *inviteFixture) (Principal, Principal) { return f.op, f.alice },
			func(f *inviteFixture) schema.CreateSignInLinkRequestV1 {
				return schema.CreateSignInLinkRequestV1{ExpiresAt: at(f, schema.MaxSignInLinkLifetime+time.Second)}
			}, ErrSignInLinkRequestInvalid},
		{"an expiry in the past", func(f *inviteFixture) (Principal, Principal) { return f.op, f.alice },
			func(f *inviteFixture) schema.CreateSignInLinkRequestV1 {
				return schema.CreateSignInLinkRequestV1{ExpiresAt: at(f, -time.Second)}
			}, ErrSignInLinkRequestInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			creator, target := tt.setup(f)
			req := schema.CreateSignInLinkRequestV1{}
			if tt.req != nil {
				req = tt.req(f)
			}
			before := f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditSignInLinkCreated)
			resp, err := f.makeSignInLink(creator, target, req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateSignInLink = %v, want %v", err, tt.wantErr)
			}
			after := f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditSignInLinkCreated)
			if tt.wantErr != nil {
				if n := f.count("SELECT COUNT(*) FROM sign_in_links"); n != 0 || after != before {
					t.Errorf("a refusal wrote %d links and %d audit rows", n, after-before)
				}
				return
			}
			if schema.ValidateSignInLinkCode(resp.Code) != nil {
				t.Errorf("code is not well-formed")
			}
			if resp.SignInLink.PrincipalID != target.ID || resp.SignInLink.CreatedBy != creator.ID {
				t.Errorf("link = %+v", resp.SignInLink)
			}
			want := req.EffectiveExpiry(f.now)
			if got := resp.SignInLink.ExpiresAt.Time(); !got.Equal(want) {
				t.Errorf("expires = %v, want %v", got, want)
			}
			if after != before+1 {
				t.Errorf("sign_in_link_created rows = %d, want one more", after-before)
			}
		})
	}
}

func TestCreateSignInLinkDefaultsAndAudit(t *testing.T) {
	f := newInviteFixture(t)
	resp := f.mustSignInLink(f.alice)
	if got, want := resp.SignInLink.ExpiresAt.Time(), f.now.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("default expiry = %v, want %v", got, want)
	}
	var stored string
	if err := f.s.db.QueryRow("SELECT code_hash FROM sign_in_links WHERE id = ?", resp.SignInLink.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != HashSignInLinkCode(resp.Code) || strings.Contains(stored, resp.Code) {
		t.Errorf("stored %q, want the code's hash only", stored)
	}
	evs := f.audit()
	last := evs[len(evs)-1]
	wantDetail := fmt.Sprintf("sign_in_link=%d expires=%s", resp.SignInLink.ID, resp.SignInLink.ExpiresAt.Time().Format(time.RFC3339))
	if last.Action != AuditSignInLinkCreated || last.Actor != principalActor(f.op.ID) || last.Subject != principalActor(f.alice.ID) || last.Detail != wantDetail {
		t.Errorf("audit = %+v, want sign_in_link_created by the operator about alice with %q", last, wantDetail)
	}
}

// Using a link signs the same person in again, with a new renewable
// credential, once.
func TestRedeemSignInLink(t *testing.T) {
	f := newInviteFixture(t)
	// Alice keeps the credential she has: a link adds one.
	_, keep, err := f.s.CreateCredential(f.ctx, "system", f.alice.ID, "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := f.mustSignInLink(f.alice)
	preview, err := f.s.PreviewSignInLink(f.ctx, HashSignInLinkCode(resp.Code), f.now)
	if err != nil || preview.Name != "alice" {
		t.Fatalf("preview = %+v, %v; want alice", preview, err)
	}
	used := f.now.Add(time.Minute)
	got, err := f.useSignInLink(resp.Code, used)
	if err != nil {
		t.Fatalf("RedeemSignInLink: %v", err)
	}
	if got.Principal.ID != f.alice.ID || got.Credential.PrincipalID != f.alice.ID {
		t.Fatalf("redeemed %+v, want alice", got)
	}
	if !got.Credential.Renews || got.Credential.ExpiresAt == nil || !got.Credential.ExpiresAt.Time().Equal(used.Truncate(time.Millisecond).Add(90*24*time.Hour)) {
		t.Errorf("credential = %+v, want renewable for an invite's default 90 days", got.Credential)
	}
	if want := fmt.Sprintf("sign-in link %d", resp.SignInLink.ID); got.Credential.Label != want {
		t.Errorf("label = %q, want %q", got.Credential.Label, want)
	}
	for name, tok := range map[string]string{"new": got.Token, "kept": keep} {
		p, err := f.s.ResolveCredential(f.ctx, tok)
		if err != nil || p.ID != f.alice.ID {
			t.Errorf("%s credential resolves to %+v, %v; want alice", name, p, err)
		}
	}
	// Used: it works no more, for preview or redeem.
	if _, err := f.useSignInLink(resp.Code, used); !errors.Is(err, ErrSignInLinkInvalid) {
		t.Errorf("second use = %v, want ErrSignInLinkInvalid", err)
	}
	if _, err := f.s.PreviewSignInLink(f.ctx, HashSignInLinkCode(resp.Code), used); !errors.Is(err, ErrSignInLinkInvalid) {
		t.Errorf("preview after use = %v, want ErrSignInLinkInvalid", err)
	}
	var credID int64
	var usedAt int64
	if err := f.s.db.QueryRow("SELECT credential_id, used_at FROM sign_in_links WHERE id = ?", resp.SignInLink.ID).Scan(&credID, &usedAt); err != nil {
		t.Fatal(err)
	}
	if credID != got.Credential.ID || usedAt != used.UnixMilli() {
		t.Errorf("link row: credential %d used %d, want %d at %d", credID, usedAt, got.Credential.ID, used.UnixMilli())
	}

	actor := fmt.Sprintf("sign_in_link:%d", resp.SignInLink.ID)
	var trail []string
	for _, e := range f.audit() {
		if e.Actor == actor {
			trail = append(trail, e.Action+"|"+e.Subject+"|"+e.Detail)
		}
	}
	want := []string{
		fmt.Sprintf("%s|principal:%d|sign_in_link=%d credential=%d", AuditSignInLinkRedeemed, f.alice.ID, resp.SignInLink.ID, got.Credential.ID),
		fmt.Sprintf("credential_created|principal:%d|credential=%d label=%q", f.alice.ID, got.Credential.ID, got.Credential.Label),
	}
	if strings.Join(trail, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit trail:\n%s\nwant:\n%s", strings.Join(trail, "\n"), strings.Join(want, "\n"))
	}
}

// A person who joined through an invite gets that invite's credential
// lifetime back, as redemption gave it.
func TestSignInLinkCredentialLifetimeFollowsTheInvite(t *testing.T) {
	f := newInviteFixture(t)
	days := int64(30 * 24 * 60 * 60)
	inv := f.mustCreate(f.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"general"}, CredentialLifetimeSeconds: &days})
	joined, err := f.redeem(inv.Code, "wren")
	if err != nil {
		t.Fatal(err)
	}
	link := f.mustSignInLink(joined.Principal)
	got, err := f.useSignInLink(link.Code, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if want := f.now.Add(30 * 24 * time.Hour); got.Credential.ExpiresAt == nil || !got.Credential.ExpiresAt.Time().Equal(want) || !got.Credential.Renews {
		t.Errorf("credential = %+v, want renewable until %v", got.Credential, want)
	}
}

// Every way a link stops working is the one error, and writes nothing.
func TestRedeemSignInLinkRefusals(t *testing.T) {
	tests := []struct {
		name string
		// break makes the link unusable and returns the time to try it at.
		breakIt func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time)
	}{
		{"unknown", func(f *inviteFixture, _ schema.CreateSignInLinkResponseV1) (string, time.Time) {
			return schema.SignInLinkCodePrefix + strings.Repeat("Q", 43), f.now
		}},
		{"an invite's code", func(f *inviteFixture, _ schema.CreateSignInLinkResponseV1) (string, time.Time) {
			return f.mustCreate(f.op, personal("general")).Code, f.now
		}},
		{"expired", func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time) {
			return link.Code, f.now.Add(24 * time.Hour)
		}},
		{"used", func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time) {
			if _, err := f.useSignInLink(link.Code, f.now); err != nil {
				f.t.Fatal(err)
			}
			return link.Code, f.now
		}},
		{"its person disabled", func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time) {
			if _, err := f.s.DisablePrincipal(f.ctx, "system", f.alice.ID); err != nil {
				f.t.Fatal(err)
			}
			return link.Code, f.now
		}},
		{"its person disabled and enabled again", func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time) {
			if _, err := f.s.DisablePrincipal(f.ctx, "system", f.alice.ID); err != nil {
				f.t.Fatal(err)
			}
			if _, err := f.s.EnablePrincipal(f.ctx, "system", f.alice.ID); err != nil {
				f.t.Fatal(err)
			}
			return link.Code, f.now
		}},
		{"its person hidden", func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time) {
			if _, err := f.s.HidePrincipal(f.ctx, "system", f.alice.ID, HideOptions{}); err != nil {
				f.t.Fatal(err)
			}
			return link.Code, f.now
		}},
		{"its person made an operator", func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time) {
			f.exec("UPDATE principals SET role = 'operator' WHERE id = ?", f.alice.ID)
			return link.Code, f.now
		}},
		{"its maker no longer an operator", func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time) {
			f.exec("UPDATE principals SET role = 'member' WHERE id = ?", f.op.ID)
			return link.Code, f.now
		}},
		{"its maker disabled", func(f *inviteFixture, link schema.CreateSignInLinkResponseV1) (string, time.Time) {
			// A second operator with a live credential, so the first may go.
			f.exec("UPDATE principals SET role = 'operator' WHERE id = ?", f.bob.ID)
			if _, _, err := f.s.CreateCredential(f.ctx, "system", f.bob.ID, "spare", nil); err != nil {
				f.t.Fatal(err)
			}
			if _, err := f.s.DisablePrincipal(f.ctx, "system", f.op.ID); err != nil {
				f.t.Fatal(err)
			}
			return link.Code, f.now
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			link := f.mustSignInLink(f.alice)
			code, at := tt.breakIt(f, link)
			creds := f.count("SELECT COUNT(*) FROM credentials")
			rows := f.count("SELECT COUNT(*) FROM audit_events")
			if _, err := f.s.PreviewSignInLink(f.ctx, HashSignInLinkCode(code), at); !errors.Is(err, ErrSignInLinkInvalid) {
				t.Errorf("preview = %v, want ErrSignInLinkInvalid", err)
			}
			if _, err := f.useSignInLink(code, at); !errors.Is(err, ErrSignInLinkInvalid) {
				t.Errorf("redeem = %v, want ErrSignInLinkInvalid", err)
			}
			if c, r := f.count("SELECT COUNT(*) FROM credentials"), f.count("SELECT COUNT(*) FROM audit_events"); c != creds || r != rows {
				t.Errorf("a refusal wrote %d credentials and %d audit rows", c-creds, r-rows)
			}
		})
	}
}

// Disabling the person a link is for, or the operator who made it, closes
// the link, audited; a used or expired one is left alone.
func TestDisableClosesSignInLinks(t *testing.T) {
	f := newInviteFixture(t)
	forAlice := f.mustSignInLink(f.alice)
	usedForAlice := f.mustSignInLink(f.alice)
	if _, err := f.useSignInLink(usedForAlice.Code, f.now); err != nil {
		t.Fatal(err)
	}
	forBob := f.mustSignInLink(f.bob)
	// Bob becomes an operator who makes a link for alice.
	f.exec("UPDATE principals SET role = 'operator' WHERE id = ?", f.bob.ID)
	byBob, err := f.makeSignInLink(f.bob, f.alice, schema.CreateSignInLinkRequestV1{})
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE principals SET role = 'member' WHERE id = ?", f.bob.ID)

	f.exec("UPDATE principals SET role = 'operator' WHERE id = ?", f.bob.ID)
	if _, err := f.s.DisablePrincipal(f.ctx, "principal:1", f.bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DisablePrincipal(f.ctx, "principal:1", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	closed := func(id int64) bool {
		return f.count("SELECT COUNT(*) FROM sign_in_links WHERE id = ? AND closed_at IS NOT NULL", id) == 1
	}
	for name, c := range map[string]struct {
		id   int64
		want bool
	}{
		"alice's open link": {forAlice.SignInLink.ID, true},
		"alice's used link": {usedForAlice.SignInLink.ID, false},
		"bob's link":        {forBob.SignInLink.ID, true},
		"the link bob made": {byBob.SignInLink.ID, true},
	} {
		if got := closed(c.id); got != c.want {
			t.Errorf("%s closed = %v, want %v", name, got, c.want)
		}
	}
	var details []string
	for _, e := range f.audit() {
		if e.Action == AuditSignInLinkClosed {
			details = append(details, e.Subject+"|"+e.Detail)
		}
	}
	want := []string{
		fmt.Sprintf("principal:%d|sign_in_link=%d reason=principal_disabled", f.bob.ID, forBob.SignInLink.ID),
		fmt.Sprintf("principal:%d|sign_in_link=%d reason=creator_disabled", f.alice.ID, byBob.SignInLink.ID),
		fmt.Sprintf("principal:%d|sign_in_link=%d reason=principal_disabled", f.alice.ID, forAlice.SignInLink.ID),
	}
	if strings.Join(details, "\n") != strings.Join(want, "\n") {
		t.Errorf("sign_in_link_closed rows:\n%s\nwant:\n%s", strings.Join(details, "\n"), strings.Join(want, "\n"))
	}
}

// Two uses of one link at once: one wins.
func TestRedeemSignInLinkConcurrently(t *testing.T) {
	f := newInviteFixture(t)
	link := f.mustSignInLink(f.alice)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.RedeemSignInLink(f.ctx, HashSignInLinkCode(link.Code), f.now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrSignInLinkInvalid):
			t.Errorf("redeem = %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d uses won, want 1", wins)
	}
	if n := f.count("SELECT COUNT(*) FROM credentials WHERE label = ?", fmt.Sprintf("sign-in link %d", link.SignInLink.ID)); n != 1 {
		t.Errorf("credentials issued = %d, want 1", n)
	}
}

// No code, hash or token is in an audit row or an error.
func TestSignInLinkSecretsNeverLeak(t *testing.T) {
	f := newInviteFixture(t)
	link := f.mustSignInLink(f.alice)
	got, err := f.useSignInLink(link.Code, f.now)
	if err != nil {
		t.Fatal(err)
	}
	_, refusal := f.useSignInLink(link.Code, f.now)
	var all strings.Builder
	for _, e := range f.audit() {
		fmt.Fprintf(&all, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	fmt.Fprintln(&all, refusal)
	for what, secret := range map[string]string{
		"code": link.Code, "code hash": HashSignInLinkCode(link.Code), "token": got.Token, "token hash": hashCredentialToken(got.Token),
	} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("the %s appears in the audit trail or an error", what)
		}
	}
}

// An operator closes a link: it works no more, and closing it again, or
// closing one already used, changes nothing.
func TestCloseSignInLink(t *testing.T) {
	f := newInviteFixture(t)
	link := f.mustSignInLink(f.alice)
	actor := principalActor(f.op.ID)
	for i, want := range []bool{true, false} {
		changed, err := f.s.CloseSignInLink(f.ctx, actor, link.SignInLink.ID, f.now)
		if err != nil || changed != want {
			t.Fatalf("close %d = %v, %v; want changed %v", i+1, changed, err, want)
		}
	}
	if _, err := f.useSignInLink(link.Code, f.now); !errors.Is(err, ErrSignInLinkInvalid) {
		t.Errorf("redeem after close = %v, want ErrSignInLinkInvalid", err)
	}
	if _, err := f.s.PreviewSignInLink(f.ctx, HashSignInLinkCode(link.Code), f.now); !errors.Is(err, ErrSignInLinkInvalid) {
		t.Errorf("preview after close = %v, want ErrSignInLinkInvalid", err)
	}

	used := f.mustSignInLink(f.alice)
	if _, err := f.useSignInLink(used.Code, f.now); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.s.CloseSignInLink(f.ctx, actor, used.SignInLink.ID, f.now); err != nil || changed {
		t.Errorf("close after use = %v, %v; want nothing changed", changed, err)
	}
	if n := f.count("SELECT COUNT(*) FROM sign_in_links WHERE id = ? AND closed_at IS NULL", used.SignInLink.ID); n != 1 {
		t.Errorf("a used link was marked closed")
	}
	if _, err := f.s.CloseSignInLink(f.ctx, actor, 9999, f.now); !errors.Is(err, ErrNotFound) {
		t.Errorf("close unknown = %v, want ErrNotFound", err)
	}

	var rows []string
	for _, e := range f.audit() {
		if e.Action == AuditSignInLinkClosed {
			rows = append(rows, e.Actor+"|"+e.Subject+"|"+e.Detail)
		}
	}
	want := fmt.Sprintf("%s|principal:%d|sign_in_link=%d reason=operator", actor, f.alice.ID, link.SignInLink.ID)
	if len(rows) != 1 || rows[0] != want {
		t.Errorf("sign_in_link_closed rows = %q, want [%q]", rows, want)
	}
}

// prove gives credential id a recent-password proof until d from now, as a
// re-authentication records one.
func (w *recoveryWorld) prove(id int64, d time.Duration) {
	w.t.Helper()
	w.exec("UPDATE credentials SET recent_auth_until = ? WHERE id = ?", w.now.Add(d).UnixMilli(), id)
}

// CreateDeviceLink checks, in the transaction that writes, everything the
// route checked before it: the caller is still a member who may be given a
// link, the credential is theirs and holds a standing proof, and the
// request is well-formed. A refusal writes no link and audits nothing.
func TestCreateDeviceLinkRechecks(t *testing.T) {
	life := schema.RecentAuthLifetimeSeconds * time.Second
	tests := []struct {
		name string
		// setup returns who asks and on which credential.
		setup   func(w *recoveryWorld) (principal, credential int64)
		req     schema.CreateDeviceLinkRequestV1
		wantErr error
	}{
		{"a member with a proof", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.aliceA, life)
			return w.alice.ID, w.aliceA
		}, schema.CreateDeviceLinkRequestV1{Label: "Phone"}, nil},
		{"no proof", func(w *recoveryWorld) (int64, int64) { return w.alice.ID, w.aliceA }, schema.CreateDeviceLinkRequestV1{}, ErrReauthRequired},
		{"a proof on another of hers", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.aliceB, life)
			return w.alice.ID, w.aliceA
		}, schema.CreateDeviceLinkRequestV1{}, ErrReauthRequired},
		{"someone else's credential and proof", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.bobCred, life)
			return w.alice.ID, w.bobCred
		}, schema.CreateDeviceLinkRequestV1{}, ErrReauthRequired},
		{"a proof that ends now", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.aliceA, 0)
			return w.alice.ID, w.aliceA
		}, schema.CreateDeviceLinkRequestV1{}, ErrReauthRequired},
		{"a revoked credential", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.aliceA, life)
			w.exec("UPDATE credentials SET revoked_at = 1 WHERE id = ?", w.aliceA)
			return w.alice.ID, w.aliceA
		}, schema.CreateDeviceLinkRequestV1{}, ErrReauthRequired},
		{"a credential of an older generation", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.aliceA, life)
			w.exec("UPDATE principals SET auth_generation = auth_generation + 1 WHERE id = ?", w.alice.ID)
			return w.alice.ID, w.aliceA
		}, schema.CreateDeviceLinkRequestV1{}, ErrReauthRequired},
		{"an operator", func(w *recoveryWorld) (int64, int64) {
			cred, _ := w.session(w.op.ID)
			w.prove(cred, life)
			return w.op.ID, cred
		}, schema.CreateDeviceLinkRequestV1{}, ErrDeviceLinkNotAllowed},
		{"a member made an operator", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.aliceA, life)
			w.exec("UPDATE principals SET role = 'operator' WHERE id = ?", w.alice.ID)
			return w.alice.ID, w.aliceA
		}, schema.CreateDeviceLinkRequestV1{}, ErrDeviceLinkNotAllowed},
		{"an agent", func(w *recoveryWorld) (int64, int64) {
			bot, err := w.s.CreatePrincipal(w.ctx, PrincipalAgent, "bot")
			if err != nil {
				w.t.Fatal(err)
			}
			cred, _ := w.session(bot.ID)
			w.prove(cred, life)
			return bot.ID, cred
		}, schema.CreateDeviceLinkRequestV1{}, ErrDeviceLinkNotAllowed},
		{"a disabled member", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.aliceA, life)
			w.exec("UPDATE principals SET disabled_at = 1 WHERE id = ?", w.alice.ID)
			return w.alice.ID, w.aliceA
		}, schema.CreateDeviceLinkRequestV1{}, ErrDeviceLinkNotAllowed},
		{"nobody", func(w *recoveryWorld) (int64, int64) { return 9999, w.aliceA }, schema.CreateDeviceLinkRequestV1{}, ErrDeviceLinkNotAllowed},
		{"longer than an hour", func(w *recoveryWorld) (int64, int64) {
			w.prove(w.aliceA, life)
			return w.alice.ID, w.aliceA
		}, schema.CreateDeviceLinkRequestV1{ExpiresAt: func() *schema.Timestamp {
			ts := schema.NewTimestamp(time.Now().Add(2 * time.Hour))
			return &ts
		}()}, ErrSignInLinkRequestInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newRecoveryWorld(t)
			principal, cred := tt.setup(w)
			resp, err := w.s.CreateDeviceLink(w.ctx, principalActor(principal), principal, cred, tt.req, w.now)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateDeviceLink = %v, want %v", err, tt.wantErr)
			}
			devices := w.count("SELECT COUNT(*) FROM sign_in_links WHERE purpose = 'device'")
			issued := w.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditDeviceLinkIssued)
			if tt.wantErr != nil {
				if devices != 0 || issued != 0 {
					t.Errorf("a refusal wrote %d links and %d device_link_issued rows", devices, issued)
				}
				return
			}
			if devices != 1 || issued != 1 {
				t.Errorf("links = %d, device_link_issued rows = %d; want 1 each", devices, issued)
			}
			l := resp.SignInLink
			if l.PrincipalID != principal || l.CreatedBy != principal || l.Purpose != schema.SignInLinkPurposeDevice || l.Label != tt.req.Label {
				t.Errorf("link = %+v", l)
			}
			if n := w.count("SELECT COUNT(*) FROM sign_in_links WHERE id = ? AND auth_generation = 0 AND label = ?", l.ID, tt.req.Label); n != 1 {
				t.Errorf("the row does not record the generation and the label")
			}
		})
	}
}
