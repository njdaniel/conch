package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Issue #153: a transaction that was begun is always ended, whatever happens
// to the caller's context, and a connection that may still hold one is never
// reused.

// txStore is a store whose database can be reopened to see what was durable.
type txStore struct {
	*Store
	path string
}

func openTxStore(t *testing.T, maxConns int) *txStore {
	t.Helper()
	path := newTestDBPath(t)
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if maxConns > 0 {
		// One connection makes the connection a transaction ran on the one
		// every later call gets, so a leaked transaction cannot hide.
		s.db.SetMaxOpenConns(maxConns)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &txStore{Store: s, path: path}
}

func insertMarker(ctx context.Context, tx execer, action string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO audit_events (actor, action, subject, detail, created_at) VALUES ('tx-test', ?, 's', 'd', 1)", action)
	return err
}

// durable reports how many rows with action exist for a second, independent
// handle on the database file: what a restart would find.
func (s *txStore) durable(t *testing.T, action string) int {
	t.Helper()
	other, err := Open(context.Background(), s.path)
	if err != nil {
		t.Fatalf("open an independent handle: %v", err)
	}
	defer func() { _ = other.Close() }()
	var n int
	if err := other.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action = ?", action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertHealthy is what must hold after any cancelled transaction: the next
// transaction works, a plain write that reports success is durable, and a
// writer on another connection is not locked out.
func (s *txStore) assertHealthy(t *testing.T, label string) {
	t.Helper()
	next := "next-" + label
	if err := s.withImmediateTx(context.Background(), func(tx execer) error {
		return insertMarker(context.Background(), tx, next)
	}); err != nil {
		t.Fatalf("the next transaction failed: %v", err)
	}
	plain := "plain-" + label
	if _, err := s.AppendAuditEvent(context.Background(), "tx-test", plain, "s", "d"); err != nil {
		t.Fatalf("a plain write failed: %v", err)
	}
	if n := s.durable(t, next); n != 1 {
		t.Errorf("the next transaction's row: %d durable, want 1", n)
	}
	if n := s.durable(t, plain); n != 1 {
		t.Errorf("a plain write reported success but %d rows are durable, want 1", n)
	}
	other, err := Open(context.Background(), s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	// The write is bounded by SQLite's own busy timeout, not by this context:
	// a lock that is really held makes it fail with "database is locked" after
	// busy_timeout, while a slow machine only makes it take longer. The
	// context is a backstop far beyond either, so a loaded machine (a race
	// build next to the server suite) cannot hit it and be mistaken for a
	// held lock (issue #294).
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := other.AppendAuditEvent(ctx, "tx-test", "other-"+label, "s", "d"); err != nil {
		t.Errorf("a writer on another connection is locked out: %v", err)
	}
}

func TestWithImmediateTxEndsTheTransactionWhenCancelled(t *testing.T) {
	tests := []struct {
		name string
		// run calls withImmediateTx once with a context that is cancelled at
		// the point under test; marker is the action its body tried to write.
		run        func(s *txStore, marker string) error
		wantErr    bool
		wantMarker int // rows the cancelled call may leave: 0 unless it committed
	}{
		{"cancelled before the call", func(s *txStore, marker string) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return s.withImmediateTx(ctx, func(tx execer) error { return insertMarker(ctx, tx, marker) })
		}, true, 0},
		{"cancelled in the body before any write", func(s *txStore, marker string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			return s.withImmediateTx(ctx, func(tx execer) error {
				cancel()
				return insertMarker(ctx, tx, marker)
			})
		}, true, 0},
		{"cancelled in the body after a write, body reports the error", func(s *txStore, marker string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			return s.withImmediateTx(ctx, func(tx execer) error {
				if err := insertMarker(ctx, tx, marker); err != nil {
					return err
				}
				cancel()
				return insertMarker(ctx, tx, marker)
			})
		}, true, 0},
		// The case that poisoned the pool: the body succeeds, then the context
		// is gone when COMMIT is due.
		{"cancelled after the body succeeded", func(s *txStore, marker string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			return s.withImmediateTx(ctx, func(tx execer) error {
				if err := insertMarker(ctx, tx, marker); err != nil {
					return err
				}
				cancel()
				return nil
			})
		}, true, 0},
		{"deadline passes in the body", func(s *txStore, marker string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			return s.withImmediateTx(ctx, func(tx execer) error {
				if err := insertMarker(ctx, tx, marker); err != nil {
					return err
				}
				<-ctx.Done()
				return nil
			})
		}, true, 0},
		{"the body fails on its own", func(s *txStore, marker string) error {
			return s.withImmediateTx(context.Background(), func(tx execer) error {
				if err := insertMarker(context.Background(), tx, marker); err != nil {
					return err
				}
				return errors.New("body failed")
			})
		}, true, 0},
		{"the body runs a statement SQLite rejects", func(s *txStore, marker string) error {
			return s.withImmediateTx(context.Background(), func(tx execer) error {
				if err := insertMarker(context.Background(), tx, marker); err != nil {
					return err
				}
				_, err := tx.ExecContext(context.Background(), "INSERT INTO no_such_table VALUES (1)")
				return err
			})
		}, true, 0},
		{"not cancelled at all", func(s *txStore, marker string) error {
			return s.withImmediateTx(context.Background(), func(tx execer) error {
				return insertMarker(context.Background(), tx, marker)
			})
		}, false, 1},
	}
	for _, conns := range []int{1, 0} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s/max conns %d", tt.name, conns), func(t *testing.T) {
				s := openTxStore(t, conns)
				marker := "marker"
				err := tt.run(s, marker)
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, want an error: %v", err, tt.wantErr)
				}
				if n := s.durable(t, marker); n != tt.wantMarker {
					t.Errorf("the call's own rows: %d durable, want %d", n, tt.wantMarker)
				}
				s.assertHealthy(t, "after")
				// And again, so one clean follow-up was not luck.
				if err := tt.run(s, marker+"-2"); (err != nil) != tt.wantErr {
					t.Fatalf("second run: err = %v", err)
				}
				s.assertHealthy(t, "after-2")
			})
		}
	}
}

// A context cancelled while COMMIT is running does not interrupt it: the
// caller was still there when its body returned, so its work is kept, and the
// outcome is never left unknown.
func TestWithImmediateTxCommitIsNotInterrupted(t *testing.T) {
	s := openTxStore(t, 1)
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		marker := fmt.Sprintf("commit-%d", i)
		done := make(chan struct{})
		err := s.withImmediateTx(ctx, func(tx execer) error {
			if err := insertMarker(ctx, tx, marker); err != nil {
				return err
			}
			// Cancel from another goroutine as the body returns, so the
			// cancellation lands around the COMMIT.
			go func() { cancel(); close(done) }()
			return nil
		})
		<-done
		cancel()
		var n int
		if qerr := s.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action = ?", marker).Scan(&n); qerr != nil {
			t.Fatal(qerr)
		}
		// Either the call saw the cancellation first and rolled back, or it
		// committed. What it reports and what is stored must agree.
		if want := map[bool]int{true: 1, false: 0}[err == nil]; n != want {
			t.Fatalf("iteration %d: err = %v but %d rows stored", i, err, n)
		}
	}
	s.assertHealthy(t, "after")
}

// A transaction that cannot be ended must not go back to the pool: the
// connection is discarded, and the next caller gets a clean one.
func TestWithImmediateTxDiscardsAConnectionItCannotClean(t *testing.T) {
	s := openTxStore(t, 1)
	s.rollbackSQL = "ROLLBACK TO no_such_savepoint" // fails and leaves the transaction open
	err := s.withImmediateTx(context.Background(), func(tx execer) error {
		if err := insertMarker(context.Background(), tx, "stuck"); err != nil {
			return err
		}
		return errors.New("body failed")
	})
	if err == nil || err.Error() != "body failed" {
		t.Fatalf("err = %v, want the body's error", err)
	}
	s.rollbackSQL = ""
	if n := s.durable(t, "stuck"); n != 0 {
		t.Errorf("the stuck transaction's row is durable (%d)", n)
	}
	s.assertHealthy(t, "after")
	if stats := s.db.Stats(); stats.OpenConnections > 1 {
		t.Errorf("open connections = %d, want at most 1", stats.OpenConnections)
	}
}

// rollback's verdict on a connection, case by case.
func TestRollbackVerdict(t *testing.T) {
	s := openTxStore(t, 1)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if !s.rollback(ctx, conn) {
		t.Error("a connection with no transaction is not clean")
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if !s.rollback(ctx, conn) {
		t.Error("a rolled-back transaction is not clean")
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("the connection still holds a transaction: %v", err)
	}
	s.rollbackSQL = "ROLLBACK TO no_such_savepoint"
	if s.rollback(ctx, conn) {
		t.Error("a failed rollback was called clean")
	}
	s.rollbackSQL = ""
	if !s.rollback(ctx, conn) {
		t.Error("rollback after the failed one")
	}
	// The verdict rests on SQLite's wording for "nothing to roll back".
	_, err = conn.ExecContext(ctx, "ROLLBACK")
	if err == nil || !strings.Contains(err.Error(), "no transaction is active") {
		t.Errorf("ROLLBACK with no transaction = %v; rollback() matches on \"no transaction is active\"", err)
	}
}

// Many transactions, each cancelled at a random moment, concurrently. After
// them the store is healthy, and every transaction is all there or not at all.
func TestWithImmediateTxRandomCancellation(t *testing.T) {
	for _, conns := range []int{1, 0} {
		t.Run(fmt.Sprintf("max conns %d", conns), func(t *testing.T) {
			s := openTxStore(t, conns)
			const workers, perWorker = 10, 50
			body := func(ctx context.Context, rng *rand.Rand, marker string) error {
				return s.withImmediateTx(ctx, func(tx execer) error {
					// Two rows: a transaction is whole or absent.
					if err := insertMarker(ctx, tx, marker); err != nil {
						return err
					}
					time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
					return insertMarker(ctx, tx, marker)
				})
			}
			// Deadlines are spread over twice the median time one transaction
			// takes here, measured afresh each round, so that on any machine
			// (and under any load) some land before, some during and some
			// after it. A round that still shows one outcome is repeated, a
			// bounded number of times; the counts are kept across rounds.
			calRng := rand.New(rand.NewSource(3)) // #nosec G404 -- test timing only
			committed, abandoned := 0, 0
			for round := 0; round < maxTimingRounds && (committed == 0 || abandoned == 0); round++ {
				span := 2 * medianDuration(t, 9, func(i int) error {
					return body(context.Background(), calRng, fmt.Sprintf("cal-%d-%d", round, i))
				})
				var wg sync.WaitGroup
				results := make([][]error, workers)
				for w := 0; w < workers; w++ {
					results[w] = make([]error, perWorker)
					wg.Add(1)
					go func(w int) {
						defer wg.Done()
						rng := rand.New(rand.NewSource(int64(round)*104729 + int64(w)*7919 + 1)) // #nosec G404 -- test timing only
						for i := 0; i < perWorker; i++ {
							ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rng.Int63n(int64(span)+1)))
							results[w][i] = body(ctx, rng, fmt.Sprintf("rand-%d-%d-%d", round, w, i))
							cancel()
						}
					}(w)
				}
				wg.Wait()
				for w := range results {
					for i, err := range results[w] {
						var n int
						if qerr := s.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action = ?", fmt.Sprintf("rand-%d-%d-%d", round, w, i)).Scan(&n); qerr != nil {
							t.Fatal(qerr)
						}
						want := 0
						if err == nil {
							want = 2
							committed++
						} else {
							abandoned++
							if strings.Contains(err.Error(), "within a transaction") {
								t.Fatalf("transaction %d-%d-%d ran on a connection that already held one: %v", round, w, i, err)
							}
						}
						if n != want {
							t.Errorf("transaction %d-%d-%d: err = %v, %d rows stored, want %d", round, w, i, err, n, want)
						}
					}
				}
			}
			t.Logf("%d committed, %d abandoned", committed, abandoned)
			if committed == 0 || abandoned == 0 {
				t.Errorf("committed %d, abandoned %d: the timings exercised only one outcome", committed, abandoned)
			}
			s.assertHealthy(t, "after")
		})
	}
}

// A COMMIT that SQLite refuses leaves the transaction open; it must be rolled
// back before the connection is reused. A deferred foreign-key violation is
// the way to make a commit fail after every statement succeeded.
func TestWithImmediateTxFailedCommitIsRolledBack(t *testing.T) {
	for _, conns := range []int{1, 0} {
		t.Run(fmt.Sprintf("max conns %d", conns), func(t *testing.T) {
			s := openTxStore(t, conns)
			ctx := context.Background()
			err := s.withImmediateTx(ctx, func(tx execer) error {
				if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
					return err
				}
				if err := insertMarker(ctx, tx, "doomed"); err != nil {
					return err
				}
				// No such channel or principal: checked only at COMMIT.
				_, err := tx.ExecContext(ctx, "INSERT INTO channel_members (channel_id, principal_id, created_at) VALUES (424242, 424242, 1)")
				return err
			})
			if err == nil || !strings.HasPrefix(err.Error(), "store: commit:") {
				t.Fatalf("err = %v, want the commit to fail", err)
			}
			if n := s.durable(t, "doomed"); n != 0 {
				t.Errorf("a row of the refused transaction is durable (%d)", n)
			}
			s.assertHealthy(t, "after")
		})
	}
}

// The message inserts use database/sql's own Tx (BeginTx), not
// withImmediateTx. It already rolls back on cancellation; this holds it to
// the same standard, since the two share the pool: after cancellations at
// random moments the store is healthy, and each insert is whole or absent.
func TestBeginTxSitesSurviveCancellation(t *testing.T) {
	for _, conns := range []int{1, 0} {
		t.Run(fmt.Sprintf("max conns %d", conns), func(t *testing.T) {
			s := openTxStore(t, conns)
			bg := context.Background()
			channel, err := s.CreateChannel(bg, "ops")
			if err != nil {
				t.Fatal(err)
			}
			author, err := s.CreatePrincipal(bg, PrincipalHuman, "ann")
			if err != nil {
				t.Fatal(err)
			}
			// Deadlines are spread over twice the median time one insert takes
			// here, measured afresh each round, so that on any machine (and
			// under any load) some land before, some during and some after it.
			// A round that still shows one outcome is repeated, a bounded
			// number of times; the counts are kept across rounds.
			rng := rand.New(rand.NewSource(11)) // #nosec G404 -- test timing only
			stored, refused := 0, 0
			for round := 0; round < maxTimingRounds && (stored == 0 || refused == 0); round++ {
				span := 2 * medianDuration(t, 9, func(i int) error {
					_, err := s.InsertMessageV1(bg, channel.ID, author.ID, fmt.Sprintf("timing-%d-%d", round, i), nil)
					return err
				})
				for i := 0; i < 300; i++ {
					ctx, cancel := context.WithTimeout(bg, time.Duration(rng.Int63n(int64(span)+1)))
					body := fmt.Sprintf("cancelled-insert-%d-%d", round, i)
					_, err := s.InsertMessageV1(ctx, channel.ID, author.ID, body, nil)
					cancel()
					var n int
					if qerr := s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE body = ?", body).Scan(&n); qerr != nil {
						t.Fatal(qerr)
					}
					if want := map[bool]int{true: 1, false: 0}[err == nil]; n != want {
						t.Fatalf("insert %d-%d: err = %v but %d rows stored", round, i, err, n)
					}
					if err == nil {
						stored++
					} else {
						refused++
					}
				}
			}
			t.Logf("%d stored, %d refused", stored, refused)
			if stored == 0 || refused == 0 {
				t.Errorf("stored %d, refused %d: the timings exercised only one outcome", stored, refused)
			}
			s.assertHealthy(t, "after")
		})
	}
}

// maxTimingRounds bounds how often a timing test recalibrates and tries again
// when its deadlines all landed on one side of the work.
const maxTimingRounds = 5

// medianDuration runs do n times, failing the test on an error, and returns
// the median time one call took. One call alone can be far off on a loaded
// machine; the median of several is not.
func medianDuration(t *testing.T, n int, do func(i int) error) time.Duration {
	t.Helper()
	took := make([]time.Duration, n)
	for i := range took {
		started := time.Now()
		if err := do(i); err != nil {
			t.Fatal(err)
		}
		took[i] = time.Since(started)
	}
	sort.Slice(took, func(a, b int) bool { return took[a] < took[b] })
	return max(took[n/2], time.Microsecond)
}
