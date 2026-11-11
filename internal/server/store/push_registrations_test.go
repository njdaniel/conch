package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Markers: values that must never appear in an audit row or a read.
const (
	markEndpoint = "https://push.example.invalid/wpush/MARKER-ENDPOINT-7f3a"
	markP256DH   = "MARKER-P256DH-9c1e"
	markAuth     = "MARKER-AUTH-2b8d"
	markToken    = "MARKER-TOKEN-4d6f" // #nosec G101 -- a test marker, not a credential
	markApp      = "org.marker.app-5e0c"
)

func webPushTarget(n string) PushTarget {
	return PushTarget{Transport: schema.PushTransportWebPush, Platform: schema.PushPlatformAndroid, App: markApp,
		Endpoint: markEndpoint + n, P256DH: markP256DH, Auth: markAuth}
}

func relayTarget() PushTarget {
	return PushTarget{Transport: schema.PushTransportRelay, Platform: schema.PushPlatformIOS, App: markApp,
		Provider: schema.PushProviderAPNs, Token: markToken}
}

func registerPush(t *testing.T, s *Store, cred int64, tg PushTarget) schema.PushRegistrationV1 {
	t.Helper()
	r, err := s.CreatePushRegistration(context.Background(), cred, tg, notificationsT0)
	if err != nil {
		t.Fatalf("CreatePushRegistration: %v", err)
	}
	return r
}

type pushRow struct {
	state, reason                     string
	endpoint, p256dh, auth, prov, tok string
	ended                             bool
}

func pushRowOf(t *testing.T, s *Store, id string) pushRow {
	t.Helper()
	var r pushRow
	var reason *string
	var ended *int64
	if err := s.db.QueryRow(`SELECT state, end_reason, endpoint, p256dh, auth, provider, token, ended_at FROM push_registrations WHERE id = ?`, id).
		Scan(&r.state, &reason, &r.endpoint, &r.p256dh, &r.auth, &r.prov, &r.tok, &ended); err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		r.reason = *reason
	}
	r.ended = ended != nil
	return r
}

func pushAuditDetails(t *testing.T, s *Store, action string) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT detail FROM audit_events WHERE action = ? ORDER BY id", action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func requireEnded(t *testing.T, s *Store, id string, reason schema.PushRegistrationEndReason) {
	t.Helper()
	r := pushRowOf(t, s, id)
	if r.state != "expired" || r.reason != string(reason) || !r.ended {
		t.Errorf("registration = %+v, want expired with reason %s", r, reason)
	}
	if r.endpoint != "" || r.p256dh != "" || r.auth != "" || r.prov != "" || r.tok != "" {
		t.Errorf("an ended registration kept its target: %+v", r)
	}
	found := false
	for _, d := range pushAuditDetails(t, s, schema.AuditActionPushRegistrationEnded) {
		if strings.Contains(d, "registration="+id+" ") && strings.HasSuffix(d, "reason="+string(reason)) {
			found = true
		}
	}
	if !found {
		t.Errorf("no push_registration_ended row with reason %s for %s", reason, id)
	}
}

func TestPushRegistrationLifecycle(t *testing.T) {
	s, _, human := credentialFixture(t)
	ctx := context.Background()
	cred := credentialOf(t, s, human, "phone")
	other := credentialOf(t, s, human, "desk")

	r := registerPush(t, s, cred, webPushTarget("a"))
	if err := r.Validate(); err != nil {
		t.Fatalf("created registration is invalid: %v", err)
	}
	if len(r.ID) != schema.PushRegistrationIDLength {
		t.Errorf("id length %d, want %d", len(r.ID), schema.PushRegistrationIDLength)
	}
	if want := webPushTarget("a").Fingerprint(); r.TargetFingerprint != want {
		t.Errorf("fingerprint %q, want %q", r.TargetFingerprint, want)
	}

	t.Run("own listing only, with no target", func(t *testing.T) {
		registerPush(t, s, other, webPushTarget("b"))
		got, err := s.ListOwnPushRegistrations(ctx, cred)
		if err != nil || len(got) != 1 || got[0].ID != r.ID {
			t.Fatalf("list = %+v, %v", got, err)
		}
		if creds, err := s.PushCredentials(ctx, human.ID); err != nil || !creds[cred] || !creds[other] {
			t.Errorf("PushCredentials = %v, %v", creds, err)
		}
	})

	t.Run("replace bumps updated_at, changes the fingerprint, keeps the id", func(t *testing.T) {
		later := notificationsT0.Add(time.Hour)
		upd, err := s.ReplacePushTarget(ctx, cred, r.ID, webPushTarget("c"), later)
		if err != nil {
			t.Fatal(err)
		}
		if upd.ID != r.ID || upd.TargetFingerprint == r.TargetFingerprint {
			t.Errorf("replaced = %+v", upd)
		}
		var at int64
		if err := s.db.QueryRow("SELECT updated_at FROM push_registrations WHERE id = ?", r.ID).Scan(&at); err != nil || at != later.UnixMilli() {
			t.Errorf("updated_at = %d, %v", at, err)
		}
		// A different transport or platform finds nothing.
		if _, err := s.ReplacePushTarget(ctx, cred, r.ID, relayTarget(), later); !errors.Is(err, ErrPushRegistrationNotFound) {
			t.Errorf("replace with another transport = %v", err)
		}
	})

	t.Run("T13: another credential's, an ended and a never-issued id are the same error", func(t *testing.T) {
		gone := registerPush(t, s, cred, webPushTarget("d"))
		if err := s.DeletePushRegistration(ctx, cred, gone.ID, notificationsT0); err != nil {
			t.Fatal(err)
		}
		requireEnded(t, s, gone.ID, schema.PushRegistrationEndSelf)
		for name, id := range map[string]string{"ended": gone.ID, "never issued": "nope"} {
			if err := s.DeletePushRegistration(ctx, cred, id, notificationsT0); !errors.Is(err, ErrPushRegistrationNotFound) {
				t.Errorf("delete %s = %v", name, err)
			}
			if _, err := s.ReplacePushTarget(ctx, cred, id, webPushTarget("x"), notificationsT0); !errors.Is(err, ErrPushRegistrationNotFound) {
				t.Errorf("replace %s = %v", name, err)
			}
		}
		if err := s.DeletePushRegistration(ctx, other, r.ID, notificationsT0); !errors.Is(err, ErrPushRegistrationNotFound) {
			t.Errorf("delete another credential's = %v", err)
		}
		if _, err := s.GetOwnPushRegistration(ctx, other, r.ID); !errors.Is(err, ErrPushRegistrationNotFound) {
			t.Errorf("get another credential's = %v", err)
		}
	})

	t.Run("a revoked credential is not registered", func(t *testing.T) {
		c := credentialOf(t, s, human, "gone")
		if err := s.RevokeCredentialOf(ctx, "principal:test", human.ID, c); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreatePushRegistration(ctx, c, webPushTarget("e"), notificationsT0); !errors.Is(err, ErrNotFound) {
			t.Errorf("create on a revoked credential = %v", err)
		}
	})
}

func TestPushRegistrationLimits(t *testing.T) {
	ctx := context.Background()
	t.Run("per credential", func(t *testing.T) {
		s, _, human := credentialFixture(t)
		c := credentialOf(t, s, human, "phone")
		var ids []string
		for i := 0; i < schema.MaxPushRegistrationsPerCredential; i++ {
			ids = append(ids, registerPush(t, s, c, webPushTarget(string(rune('a'+i)))).ID)
		}
		if _, err := s.CreatePushRegistration(ctx, c, webPushTarget("z"), notificationsT0); !errors.Is(err, ErrTooManyPushRegistrations) {
			t.Fatalf("third = %v", err)
		}
		// Ending one frees a place.
		if err := s.DeletePushRegistration(ctx, c, ids[0], notificationsT0); err != nil {
			t.Fatal(err)
		}
		registerPush(t, s, c, webPushTarget("y"))
	})
	t.Run("per principal, across live credentials only", func(t *testing.T) {
		s, _, human := credentialFixture(t)
		var creds []int64
		for i := 0; i < schema.MaxPushRegistrationsPerPrincipal/schema.MaxPushRegistrationsPerCredential; i++ {
			c := credentialOf(t, s, human, "c")
			creds = append(creds, c)
			for j := 0; j < schema.MaxPushRegistrationsPerCredential; j++ {
				registerPush(t, s, c, webPushTarget("p"))
			}
		}
		extra := credentialOf(t, s, human, "extra")
		if _, err := s.CreatePushRegistration(ctx, extra, webPushTarget("q"), notificationsT0); !errors.Is(err, ErrTooManyPushRegistrations) {
			t.Fatalf("17th = %v", err)
		}
		if err := s.RevokeCredentialOf(ctx, "principal:test", human.ID, creds[0]); err != nil {
			t.Fatal(err)
		}
		registerPush(t, s, extra, webPushTarget("q"))
	})
}

func TestRevocationEntryPointsEndPushRegistrations(t *testing.T) {
	ctx := context.Background()
	type world struct {
		s          *Store
		human      Principal
		cred       int64
		other      int64
		regA, regB string // of cred, of other
	}
	newWorld := func(t *testing.T) world {
		s, _, human := credentialFixture(t)
		w := world{s: s, human: human, cred: credentialOf(t, s, human, "phone"), other: credentialOf(t, s, human, "desk")}
		w.regA = registerPush(t, s, w.cred, webPushTarget("a")).ID
		w.regB = registerPush(t, s, w.other, relayTarget()).ID
		return w
	}
	tests := []struct {
		name      string
		do        func(t *testing.T, w world) error
		othersToo bool
	}{
		{"sign out (own credential)", func(t *testing.T, w world) error {
			return w.s.RevokeOwnCredential(ctx, "principal:test", w.cred)
		}, false},
		{"end a sign-in by id", func(t *testing.T, w world) error {
			return w.s.RevokeCredentialOf(ctx, "principal:test", w.human.ID, w.cred)
		}, false},
		{"operator revoke", func(t *testing.T, w world) error { return w.s.RevokeCredential(ctx, "principal:test", w.cred) }, false},
		{"rotate", func(t *testing.T, w world) error {
			_, _, err := w.s.RotateCredential(ctx, "principal:test", w.cred)
			return err
		}, false},
		{"revoke all", func(t *testing.T, w world) error {
			_, err := w.s.RevokeAllCredentials(ctx, "principal:test", w.human.ID)
			return err
		}, true},
		{"disable", func(t *testing.T, w world) error {
			_, err := w.s.DisablePrincipal(ctx, "principal:test", w.human.ID)
			return err
		}, true},
		{"bootstrap revokes every live credential", func(t *testing.T, w world) error {
			_, err := w.s.BootstrapOperatorWith(ctx, "root", BootstrapOptions{})
			return err
		}, true},
		{"restore bumps every generation", func(t *testing.T, w world) error {
			_, err := w.s.InvalidateRestored(ctx, notificationsT0)
			return err
		}, true},
		{"a bump of the principal's generation", func(t *testing.T, w world) error {
			_, err := w.s.BumpAuthGeneration(ctx, "principal:test", w.human.ID, "test", notificationsT0)
			return err
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			if err := tt.do(t, w); err != nil {
				t.Fatal(err)
			}
			requireEnded(t, w.s, w.regA, schema.PushRegistrationEndCredentialRevoked)
			if got := pushRowOf(t, w.s, w.regB).state; (got == "expired") != tt.othersToo {
				t.Errorf("the other credential's registration is %s; the entry ends it = %v", got, tt.othersToo)
			}
			if tt.othersToo {
				requireEnded(t, w.s, w.regB, schema.PushRegistrationEndCredentialRevoked)
			}
		})
	}

	t.Run("a refused revocation keeps them", func(t *testing.T) {
		s := openTestStore(t)
		op, cred, _, err := s.BootstrapOperator(ctx, "root")
		if err != nil {
			t.Fatal(err)
		}
		reg := registerPush(t, s, cred.ID, webPushTarget("a"))
		for name, do := range map[string]func() error{
			"end by id":    func() error { return s.RevokeCredentialOf(ctx, "principal:test", op.ID, cred.ID) },
			"own sign-out": func() error { return s.RevokeOwnCredential(ctx, "principal:test", cred.ID) },
		} {
			if err := do(); !errors.Is(err, ErrLastOperator) {
				t.Fatalf("%s = %v, want ErrLastOperator", name, err)
			}
			r := pushRowOf(t, s, reg.ID)
			if r.state != "active" || r.endpoint == "" {
				t.Errorf("%s: a refused revocation changed the registration: %+v", name, r)
			}
		}
		if n := len(pushAuditDetails(t, s, schema.AuditActionPushRegistrationEnded)); n != 0 {
			t.Errorf("%d ended rows after a refusal", n)
		}
	})
}

func TestSweepEndsPushRegistrations(t *testing.T) {
	s, _, human := credentialFixture(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	soon := now.Add(time.Hour)
	expiring, _, err := s.CreateCredential(ctx, "system", human.ID, "expiring", &soon)
	if err != nil {
		t.Fatal(err)
	}
	idle := credentialOf(t, s, human, "idle")
	silent := credentialOf(t, s, human, "silent")
	fetched := credentialOf(t, s, human, "fetched")
	updated := credentialOf(t, s, human, "updated")
	if _, err := s.db.Exec("UPDATE credentials SET idle_ms = 60000, last_used_at = ? WHERE id = ?", now.Add(-time.Hour).UnixMilli(), idle); err != nil {
		t.Fatal(err)
	}
	reg := func(c int64, at time.Time) string {
		r, err := s.CreatePushRegistration(ctx, c, webPushTarget("s"), at)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	old := now.Add(-PushStaleAfter - time.Hour)
	rExpiring, rIdle := reg(expiring.ID, now), reg(idle, now)
	rSilent, rFetched, rUpdated := reg(silent, old), reg(fetched, old), reg(updated, old)
	if err := s.TouchPushFetch(ctx, fetched, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplacePushTarget(ctx, updated, rUpdated, webPushTarget("u"), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	res, err := s.SweepSessions(ctx, now, time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.PushEnded != 2 {
		t.Errorf("PushEnded = %d, want 2", res.PushEnded)
	}
	requireEnded(t, s, rIdle, schema.PushRegistrationEndCredentialExpired)
	requireEnded(t, s, rSilent, schema.PushRegistrationEndStale)
	for name, id := range map[string]string{"expiring, not yet": rExpiring, "fetched": rFetched, "updated": rUpdated} {
		if st := pushRowOf(t, s, id).state; st != "active" {
			t.Errorf("%s is %s, want active", name, st)
		}
	}
	// After the expiry.
	if _, err := s.SweepSessions(ctx, now.Add(2*time.Hour), time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	requireEnded(t, s, rExpiring, schema.PushRegistrationEndCredentialExpired)
}

func TestReconcilePushVAPIDKey(t *testing.T) {
	s, _, human := credentialFixture(t)
	ctx := context.Background()
	c := credentialOf(t, s, human, "phone")
	replaced, ended, err := s.ReconcilePushVAPIDKey(ctx, "key-one", notificationsT0)
	if err != nil || replaced || ended != 0 {
		t.Fatalf("first = %v %d %v", replaced, ended, err)
	}
	web := registerPush(t, s, c, webPushTarget("a"))
	relay := registerPush(t, s, c, relayTarget())
	// Same key: nothing.
	if replaced, ended, err = s.ReconcilePushVAPIDKey(ctx, "key-one", notificationsT0); err != nil || replaced || ended != 0 {
		t.Fatalf("same key = %v %d %v", replaced, ended, err)
	}
	// Another key: webpush registrations end as stale, relay stays.
	if replaced, ended, err = s.ReconcilePushVAPIDKey(ctx, "key-two", notificationsT0); err != nil || !replaced || ended != 1 {
		t.Fatalf("replaced = %v %d %v", replaced, ended, err)
	}
	requireEnded(t, s, web.ID, schema.PushRegistrationEndStale)
	if st := pushRowOf(t, s, relay.ID).state; st != "active" {
		t.Errorf("a relay registration is %s after a VAPID change", st)
	}
	if replaced, ended, _ = s.ReconcilePushVAPIDKey(ctx, "key-two", notificationsT0); replaced || ended != 0 {
		t.Errorf("settled key = %v %d", replaced, ended)
	}
}

// No audit row holds a target, an app or a fingerprint.
func TestPushAuditHoldsNoTarget(t *testing.T) {
	s, _, human := credentialFixture(t)
	ctx := context.Background()
	c := credentialOf(t, s, human, "phone")
	r := registerPush(t, s, c, webPushTarget("a"))
	registerPush(t, s, c, relayTarget())
	if err := s.DeletePushRegistration(ctx, c, r.ID, notificationsT0); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeOwnCredential(ctx, "principal:test", c); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("SELECT actor, action, subject, detail FROM audit_events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a, b, c, d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		all := a + b + c + d
		for _, m := range []string{"MARKER", markApp, r.TargetFingerprint} {
			if strings.Contains(all, m) {
				t.Errorf("audit row %q holds %q", b, m)
			}
		}
	}
	if n := len(pushAuditDetails(t, s, schema.AuditActionPushRegistrationCreated)); n != 2 {
		t.Errorf("%d created rows, want 2", n)
	}
}
