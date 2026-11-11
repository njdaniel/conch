package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/push"
	"github.com/njdaniel/conch/internal/server/store"
)

// startPush turns the push options into the server's push configuration and,
// with Web Push on, records the VAPID key with the store before anything is
// served (issue #1714). A key that is not the one the registrations were made
// under (lost and made again, or replaced) ends every active webpush
// registration with reason stale, audited, in the store's one transaction,
// and says so in one WARN line: a subscription is bound to the key it was made
// with, so each of them is dead anyway. With Web Push off the key is not
// touched, no registration is ended and nothing is created.
//
// The WARN line names a count and never a key, a file's contents or a target.
func startPush(ctx context.Context, st *store.Store, opts serveOptions, key *push.VAPIDKey, now time.Time) (server.PushConfig, error) {
	if !opts.pushWebPush {
		return server.PushConfig{}, nil
	}
	replaced, ended, err := st.ReconcilePushVAPIDKey(ctx, key.Fingerprint(), now)
	if err != nil {
		return server.PushConfig{}, fmt.Errorf("serve: record the push key: %w", err)
	}
	if replaced || ended > 0 {
		slog.Warn("push: the VAPID key is not the one the registrations were made under (it was lost or replaced); every Web Push registration was ended as stale, and each app must register again",
			"registrations_ended", ended, "key_file", push.KeyFileName)
	}
	if opts.pushAllowPrivate {
		// As --metrics-allow-remote is warned about: the operator has loosened a
		// protection, and says so in the log on every start.
		slog.Warn("push: --push-webpush-allow-private is on: push endpoints on private addresses and any port are accepted")
	}
	slog.Info("push: Web Push is on", "allow_private", opts.pushAllowPrivate)
	return server.PushConfig{WebPush: true, AllowPrivate: opts.pushAllowPrivate, VAPID: key}, nil
}
