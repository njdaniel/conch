package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The open channel set and the address's mode race accounts made at the
// bare address (POST /v1/join/account): whatever wins, an account is made
// only while the mode offers accounts, into exactly the channels open when
// its transaction committed (JoinAsAccount reads the policy and the open
// channels again inside its IMMEDIATE transaction). It was a race of guest
// joins until guests were removed (issue #1005).
//
// What was open at each commit is read back from the audit log: every
// settings change and every account is audited in the transaction that
// makes it, and SQLite runs one writer at a time, so the rows of each
// transaction are contiguous and in commit order. Replaying them gives the
// mode and the open set each account was made under.
//
// The joins go on until they have raced enough: at least minJoined accounts
// made, into at least minSets different channel sets, while the settings
// goroutine answered at least minChanges changes, some of them refusals.
// Each settings answer releases one bounded batch of joins. An unbounded
// stream can keep taking SQLite's unfair write lock until the settings
// change exceeds its busy timeout (issue #800); backpressure leaves only the
// current batch for that change to wait behind, without deciding which
// request wins. Every server of this package hashes with the cheapest Argon2
// parameters (password_test.go), so a join costs microseconds.
func TestOpenChannelsRaceAccountJoins(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.mustPutJoin(t, openTo("red"))
	const (
		joiners    = 4
		minJoined  = 20
		minSets    = 2
		minChanges = 12 // the six states twice
		limit      = 60 * time.Second
	)
	var changes, refusedChanges atomic.Int64
	race, stop := context.WithTimeout(context.Background(), limit)
	defer stop()
	permits := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The address is off in one state of six, so most joins race a
		// channel change and some race the switch.
		sets := []schema.PutJoinSettingsRequestV1{
			{OpenChannels: &[]string{"blue"}},
			{OpenChannels: &[]string{"red", "blue"}},
			{OpenChannels: &[]string{}}, // refused while on: the last channel stays
			{OpenChannels: &[]string{"red"}},
			{OpenJoining: modePtr(schema.OpenJoiningOff), OpenChannels: &[]string{}},
			openTo("red"),
		}
		for i := 0; race.Err() == nil; i++ {
			rec := f.putJoin(t, sets[i%len(sets)])
			switch rec.Code {
			case http.StatusOK:
			case http.StatusConflict:
				refusedChanges.Add(1)
			default:
				t.Errorf("settings change = %d %s", rec.Code, rec.Body)
				stop()
				return
			}
			changes.Add(1)
			for range joiners {
				select {
				case permits <- struct{}{}:
				case <-race.Done():
					return
				}
			}
		}
	}()
	var joined []schema.JoinAccountResponseV1
	answered := map[string]bool{} // the channel sets the accounts were made into
	var mu sync.Mutex
	raced := false
	// enough stops the joins once they have raced enough. mu is held.
	enough := func() {
		if len(joined) >= minJoined && len(answered) >= minSets && changes.Load() >= minChanges && refusedChanges.Load() > 0 {
			raced = true
			stop()
		}
	}
	var next atomic.Int64 // every join comes from an address of its own, under a name of its own
	var joining sync.WaitGroup
	for range joiners {
		joining.Add(1)
		go func() {
			defer joining.Done()
			for {
				select {
				case <-race.Done():
					return
				case <-permits:
				}
				n := next.Add(1)
				rec := f.joinAccount(t, fmt.Sprintf("joiner-%d", n), fmt.Sprintf("10.%d.%d.%d:1", n>>16&0xff, n>>8&0xff, n&0xff))
				mu.Lock()
				switch rec.Code {
				case http.StatusCreated:
					a := decodeBody[schema.JoinAccountResponseV1](t, rec)
					joined = append(joined, a)
					chans := make([]string, len(a.Channels))
					for i, c := range a.Channels {
						chans[i] = c.Name
					}
					slices.Sort(chans)
					answered[strings.Join(chans, ",")] = true
				case http.StatusForbidden:
					// joining_closed while the address is off.
				default:
					t.Errorf("join = %d %s", rec.Code, rec.Body)
					stop()
					mu.Unlock()
					return
				}
				enough()
				mu.Unlock()
			}
		}()
	}
	joining.Wait()
	stop()
	wg.Wait()
	if !raced {
		t.Fatalf("after %v, %d accounts made into channel sets %v while %d settings changes were answered (%d refused); want at least %d, %d and %d, and a refusal: the test proved nothing",
			limit, len(joined), answered, changes.Load(), refusedChanges.Load(), minJoined, minSets, minChanges)
	}

	// Replay the audit log: the mode and the open set as each transaction
	// committed, and each account's channels as it was made.
	ctx := context.Background()
	var events []store.AuditEvent
	for after := int64(0); ; {
		page, err := f.srv.store.ListAuditEvents(ctx, after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		events = append(events, page...)
		after = page[len(page)-1].ID
	}
	mode := schema.OpenJoiningOff
	open := map[int64]bool{}
	type made struct {
		mode     schema.OpenJoiningMode
		open     map[int64]bool
		channels map[int64]bool
	}
	accounts := map[int64]*made{}
	var current *made // the account whose transaction's rows are being read
	channelOf := func(subject string) int64 {
		id, err := strconv.ParseInt(strings.TrimPrefix(subject, "channel:"), 10, 64)
		if err != nil || !strings.HasPrefix(subject, "channel:") {
			t.Fatalf("an audit row's subject %q is not a channel", subject)
		}
		return id
	}
	for _, e := range events {
		switch e.Action {
		case store.AuditJoinSettingsChanged:
			current = nil
			for _, field := range strings.Fields(e.Detail) {
				if v, ok := strings.CutPrefix(field, "open_joining="); ok {
					_, to, _ := strings.Cut(v, "->")
					mode = schema.OpenJoiningMode(to)
				}
			}
		case store.AuditOpenChannelAdded:
			current = nil
			open[channelOf(e.Subject)] = true
		case store.AuditOpenChannelRemoved:
			current = nil
			delete(open, channelOf(e.Subject))
		case store.AuditPrincipalCreated:
			current = nil
			if e.Detail != "kind=human origin=address" {
				continue
			}
			id, err := strconv.ParseInt(strings.TrimPrefix(e.Subject, "principal:"), 10, 64)
			if err != nil {
				t.Fatalf("principal_created subject %q", e.Subject)
			}
			current = &made{mode: mode, open: maps.Clone(open), channels: map[int64]bool{}}
			accounts[id] = current
		case store.AuditMemberAdded:
			if current != nil && e.Actor == "system" {
				current.channels[channelOf(e.Subject)] = true
			}
		}
	}
	for _, a := range joined {
		m, ok := accounts[a.Principal.ID]
		if !ok {
			t.Errorf("account %d has no principal_created row from the address", a.Principal.ID)
			continue
		}
		if m.mode != schema.OpenJoiningAccount {
			t.Errorf("account %d was made while the address was %q", a.Principal.ID, m.mode)
		}
		if len(m.channels) == 0 || !maps.Equal(m.channels, m.open) {
			t.Errorf("account %d joined %v; open as it committed: %v", a.Principal.ID, m.channels, m.open)
		}
		answer := map[int64]bool{}
		for _, c := range a.Channels {
			answer[c.ID] = true
		}
		members, err := f.srv.store.ListChannelsForPrincipal(ctx, a.Principal.ID)
		if err != nil {
			t.Fatal(err)
		}
		held := map[int64]bool{}
		for _, c := range members {
			held[c.ID] = true
		}
		if !maps.Equal(answer, m.channels) || !maps.Equal(held, m.channels) {
			t.Errorf("account %d answered channels %v and holds %v; it was made into %v", a.Principal.ID, answer, held, m.channels)
		}
	}
	if len(accounts) != len(joined) {
		t.Errorf("%d accounts were made from the address, %d answered: a refused join made one", len(accounts), len(joined))
	}
}
