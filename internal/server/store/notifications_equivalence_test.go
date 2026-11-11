package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"testing"
)

// The feed's candidate list must equal what the single statement of issue
// #1713 returned (issue #1730): every channel the reader is in joined at once,
// every condition applied, sorted by id and cut at the limit. That statement
// is kept here as the reference, and never in production code, because its
// cost follows the reader's backlog. The test builds random worlds
// (memberships, bans, removed members, read marks, history bounds, threads,
// whispers, removals, own posts, half of them ANALYZEd) from a fixed seed, and
// compares the two over many cursors and limits.
const referencePendingHead = `SELECT m.id, m.channel_id, ch.name, m.author_id, m.body, m.audience_kind
	FROM messages m
	JOIN channels ch ON ch.id = m.channel_id
	JOIN ` + liveMembers + ` mem ON mem.channel_id = m.channel_id AND mem.principal_id = ?
	LEFT JOIN read_marks rm ON rm.principal_id = ? AND rm.channel_id = m.channel_id
	WHERE m.id > ? AND m.id > COALESCE(rm.last_read_id, 0)
	  AND m.author_id <> ? AND ` + notRemovedCond + `
	  AND `

type feedRow struct {
	id, ch, author int64
	name, body     string
	whisper        bool
}

func referenceCandidates(t *testing.T, s *Store, reader, after int64, limit int) []feedRow {
	_, pa := visibilityArgs(Reader{PrincipalID: reader, Scoped: true})
	args := []any{reader, reader, after, reader}
	args = append(append(args, pa...), limit)
	rows, err := s.db.Query(referencePendingHead+messageVisible(true)+pendingNotificationsTail, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []feedRow
	for rows.Next() {
		var r feedRow
		var k sql.NullString
		if err := rows.Scan(&r.id, &r.ch, &r.name, &r.author, &r.body, &k); err != nil {
			t.Fatal(err)
		}
		r.whisper = k.Valid && k.String == "principals"
		out = append(out, r)
	}
	return out
}

func TestPendingCandidatesEquivalence(t *testing.T) {
	ctx := context.Background()
	seeds, maxMsgs, maxChans := int64(8), 400, 8
	if os.Getenv("CONCH_BENCH") != "" {
		seeds, maxMsgs, maxChans = 25, 1500, 12
	}
	for seed := int64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			f := newFeedFixture(t)
			ps := []int64{f.dave.ID, f.erin.ID, f.frank.ID, f.alice.ID, f.bob.ID}
			chans := []int64{f.ch.ID}
			nch := 1 + rng.Intn(maxChans)
			for i := 0; i < nch; i++ {
				ch, err := f.s.CreateChannel(ctx, fmt.Sprintf("z%d", i))
				if err != nil {
					t.Fatal(err)
				}
				chans = append(chans, ch.ID)
			}
			ex := func(q string, a ...any) {
				if _, err := f.s.db.Exec(q, a...); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			for _, ch := range chans {
				for _, p := range ps {
					if rng.Intn(4) > 0 {
						ex("INSERT OR IGNORE INTO channel_members (channel_id, principal_id, created_at) VALUES (?,?,0)", ch, p)
					} else {
						ex("DELETE FROM channel_members WHERE channel_id=? AND principal_id=?", ch, p)
					}
				}
			}
			n := 50 + rng.Intn(maxMsgs)
			var roots []int64
			for i := 0; i < n; i++ {
				ch := chans[rng.Intn(len(chans))]
				if rng.Intn(3) == 0 {
					ch = chans[0] // skew
				}
				au := ps[rng.Intn(len(ps))]
				var kind any
				if rng.Intn(4) == 0 {
					kind = "principals"
				}
				var root any
				if len(roots) > 0 && rng.Intn(6) == 0 {
					var rch int64
					r := roots[rng.Intn(len(roots))]
					if err := f.s.db.QueryRow("SELECT channel_id FROM messages WHERE id=?", r).Scan(&rch); err != nil {
						t.Fatal(err)
					}
					ch, root = rch, r
				}
				res, err := f.s.db.Exec("INSERT INTO messages (channel_id, author_id, body, created_at, audience_kind, thread_root_id) VALUES (?,?,?,?,?,?)", ch, au, fmt.Sprintf("b%d", i), i, kind, root)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				if root == nil {
					roots = append(roots, id)
				}
				if kind != nil {
					for _, p := range ps {
						if rng.Intn(2) == 0 {
							ex("INSERT INTO message_recipients (message_id, principal_id) VALUES (?,?)", id, p)
						}
					}
				}
				if rng.Intn(15) == 0 {
					ex("INSERT INTO message_removals (message_id, channel_id, removed_at) VALUES (?,?,0)", id, ch)
				}
			}
			maxID := int64(n + 50)
			for _, ch := range chans {
				for _, p := range ps {
					switch rng.Intn(6) {
					case 0:
						ex("INSERT OR IGNORE INTO channel_bans (channel_id, principal_id, created_at) VALUES (?,?,0)", ch, p)
					case 1:
						ex("UPDATE channel_members SET history_after=? WHERE channel_id=? AND principal_id=?", rng.Int63n(maxID), ch, p)
					}
					if rng.Intn(3) == 0 {
						ex("INSERT OR REPLACE INTO read_marks (principal_id, channel_id, last_read_id, updated_at) VALUES (?,?,?,0)", p, ch, rng.Int63n(maxID))
					}
				}
			}
			if seed%2 == 0 {
				ex("ANALYZE")
			}
			for _, p := range ps {
				for _, after := range []int64{0, 1, rng.Int63n(maxID), rng.Int63n(maxID), maxID} {
					for _, limit := range []int{1, 2, 3, 7, 255, 256, 257, 513, 2001} {
						want := referenceCandidates(t, f.s, p, after, limit)
						got, err := f.s.pendingCandidates(ctx, Reader{PrincipalID: p, Scoped: true}, after, limit)
						if err != nil {
							t.Fatal(err)
						}
						var g []feedRow
						for _, c := range got {
							g = append(g, feedRow{c.summary.MessageID, c.summary.ChannelID, c.summary.AuthorID, c.summary.ChannelName, c.body, c.whisper})
						}
						if !reflect.DeepEqual(want, g) {
							t.Fatalf("seed %d reader %d after %d limit %d: reference %d rows, feed %d rows", seed, p, after, limit, len(want), len(g))
						}
					}
				}
			}
			t.Logf("seed %d ok (%d msgs, %d chans)", seed, n, len(chans))
		})
	}
}
