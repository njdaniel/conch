package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Guests are removed for now (issue #1005): no route starts a visit, and no
// route makes an invite that admits one. A server that was running when
// they were removed still has visits live, for up to a day, and guest
// invites; their confinement is what the guests' phase checks. So the
// visits and the guest invites are made here, in the running server's
// database beside it, as the entry routes made them before (a host command
// beside the server is the precedent: hostCommand). It is the one thing this
// check does other than through the public API; everything the guests then
// do, and everything done to them, goes through it, and the entry routes are
// tried as refusals (gates.go, the newcomers' rows).

// underStore runs fn on the running server's store, opened beside it.
func (w *world) underStore(fn func(ctx context.Context, st *store.Store) error) error {
	ctx := context.Background()
	// Opening beside a running server cannot fold its write-ahead log, and
	// says so; that is expected here, not news.
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(old)
	st, err := store.Open(ctx, filepath.Join(w.d.dataDir, "conch.db"))
	if err != nil {
		return fmt.Errorf("open the server's store beside it: %w", err)
	}
	defer func() { _ = st.Close() }()
	return fn(ctx, st)
}

// underDB runs query on the running server's database file, beside it.
func (w *world) underDB(query string, args ...any) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(w.d.dataDir, "conch.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(query, args...)
	return err
}

// legacyInvite is an operator's open invite to the plaza that admits mode,
// as one made before guests were removed: made over the API to admit
// accounts, then given its mode in the database.
func (w *world) legacyInvite(mode schema.InviteJoinMode) (plazaInvite, error) {
	resp, err := w.invite(w.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{w.p.channel}, Voice: true})
	if err != nil {
		return plazaInvite{}, err
	}
	if mode != schema.InviteJoinAccount {
		if err := w.underDB("UPDATE invites SET join_as = ? WHERE id = ?", string(mode), resp.Invite.ID); err != nil {
			return plazaInvite{}, fmt.Errorf("give invite %d the mode %s: %w", resp.Invite.ID, mode, err)
		}
	}
	return plazaInvite{id: resp.Invite.ID, code: resp.Code}, nil
}

// seedVisit starts a visit as an entry route did before guests were
// removed: from the bare address into its open channels under the joining
// settings' bounds and voice, or, with an invite, through it into the plaza
// with its voice and a use spent. The guest comes from a's address. Its
// visit is what the server then says of it (GET /v1/whoami).
func (w *world) seedVisit(a api, label string, inv *plazaInvite) (*member, error) {
	var g store.CreatedGuest
	err := w.underStore(func(ctx context.Context, st *store.Store) error {
		set, _, err := st.JoinSettings(ctx)
		if err != nil {
			return err
		}
		ng := store.NewGuest{Label: label, ServerName: "conch", Origin: schema.GuestOriginAddress, VoiceAllowed: set.JoinVoice,
			Idle: time.Duration(set.GuestIdleSeconds) * time.Second, Absolute: time.Duration(set.GuestAbsoluteSeconds) * time.Second}
		actor := "system"
		names := set.OpenChannels
		if inv != nil {
			names = []string{w.p.channel}
			invites, err := st.ListInvites(ctx, store.InviteFilter{}, time.Now())
			if err != nil {
				return err
			}
			for _, i := range invites {
				if i.ID == inv.id {
					ng.InvitedBy, ng.VoiceAllowed = i.CreatedBy, i.Voice
					ng.CredentialLifetime = time.Duration(i.CredentialLifetimeSeconds) * time.Second
				}
			}
			if ng.InvitedBy == 0 {
				return fmt.Errorf("invite %d is not listed", inv.id)
			}
			ng.Origin, ng.InviteID, actor = schema.GuestOriginInvite, inv.id, "invite:"+strconv.FormatInt(inv.id, 10)
		}
		for _, name := range names {
			c, err := st.ChannelByName(ctx, name)
			if err != nil {
				return err
			}
			ng.Channels = append(ng.Channels, c.ID)
		}
		if g, err = st.CreateGuest(ctx, actor, ng); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("start a visit beside the server: %w", err)
	}
	if inv != nil {
		if err := w.underDB("UPDATE invites SET uses = uses + 1 WHERE id = ?", inv.id); err != nil {
			return nil, err
		}
	}
	w.h.secret(g.Visit.Handle+"'s credential", g.Token)
	m := &member{name: g.Visit.Handle, id: g.Principal.ID, token: g.Token, credID: g.Credential.ID, addr: a.addr}
	m.api = w.as(m)
	var who schema.WhoAmIResponseV1
	if err := m.api.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return nil, err
	}
	if who.Guest == nil {
		return nil, fmt.Errorf("the visit started beside the server is not a guest's: %+v", who)
	}
	if err := who.Guest.Validate(); err != nil {
		return nil, fmt.Errorf("the visit the server answered: %w", err)
	}
	m.guest = who.Guest
	return m, nil
}
