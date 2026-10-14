package store

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func runIDs(runs []Run) []int64 {
	ids := make([]int64, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}

// Newest-first pages walk the same runs backwards under the same rule: a run
// the reader may not see neither fills a page nor moves the cursor, before
// pages back from the smallest id held, and session= narrows (issue #317).
func TestListVisibleRunsNewestFirst(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	// frank sees wide and wideMsg of the fixture, then one in three of these;
	// the whisper and net runs between them are invisible to him.
	franks := []int64{f.runWide, f.runWideMsg}
	for i := 0; i < 5; i++ {
		franks = append(franks, f.mustRun(t, f.ch.ID, 0, fmt.Sprintf("visible %d", i)))
		f.mustRun(t, f.ch.ID, f.whisperMsg, "hidden")
		f.mustRun(t, f.ch.ID, f.netMsg, "hidden")
	}
	newestFirst := make([]int64, len(franks))
	for i, id := range franks {
		newestFirst[len(franks)-1-i] = id
	}
	frank := Reader{f.frank.ID, true}

	for _, limit := range []int{1, 2, 3, 100} {
		var got []int64
		before := int64(0)
		for pages := 0; ; pages++ {
			if pages > 100 {
				t.Fatalf("limit %d: paging did not terminate", limit)
			}
			page, err := f.s.ListVisibleRuns(ctx, f.ch.ID, frank, 0, RunPage{Before: before, NewestFirst: true}, limit+1)
			if err != nil {
				t.Fatal(err)
			}
			more := len(page) > limit
			if more {
				page = page[:limit]
			}
			for _, r := range page {
				got = append(got, r.ID)
				before = r.ID
			}
			if !more {
				break
			}
		}
		if !reflect.DeepEqual(got, newestFirst) {
			t.Errorf("limit %d: paged %v, want %v", limit, got, newestFirst)
		}
	}

	// The cursor is exclusive, and a cursor at the oldest run is an empty page.
	page, err := f.s.ListVisibleRuns(ctx, f.ch.ID, frank, 0, RunPage{Before: franks[2], NewestFirst: true}, 100)
	if want := []int64{franks[1], franks[0]}; err != nil || !reflect.DeepEqual(runIDs(page), want) {
		t.Errorf("before %d = %v, %v; want %v", franks[2], runIDs(page), err, want)
	}
	if page, err := f.s.ListVisibleRuns(ctx, f.ch.ID, frank, 0, RunPage{Before: franks[0], NewestFirst: true}, 100); err != nil || len(page) != 0 {
		t.Errorf("before the oldest = %v, %v; want none", runIDs(page), err)
	}
	// A reader with no verified identity sees nothing.
	if page, err := f.s.ListVisibleRuns(ctx, f.ch.ID, Reader{}, 0, RunPage{NewestFirst: true}, 100); err != nil || len(page) != 0 {
		t.Errorf("an anonymous reader read %v, %v", runIDs(page), err)
	}

	// session= narrows a newest-first read too.
	other, err := f.s.CreateSession(ctx, principalActor(f.agt.ID), SessionParams{PrincipalID: f.agt.ID, CredentialID: f.agtCred, Client: "adapter"}, runT0)
	if err != nil {
		t.Fatal(err)
	}
	var onOther []int64
	for i := 0; i < 3; i++ {
		r, err := f.s.CreateRun(ctx, "system", RunParams{SessionID: other.ID, CredentialID: f.agtCred, ChannelID: f.ch.ID, Task: "second session"}, f.agtReader(), runT0)
		if err != nil {
			t.Fatal(err)
		}
		onOther = append([]int64{r.ID}, onOther...)
	}
	if got, err := f.s.ListVisibleRuns(ctx, f.ch.ID, frank, other.ID, RunPage{NewestFirst: true}, 2); err != nil || !reflect.DeepEqual(runIDs(got), onOther[:2]) {
		t.Errorf("session filter = %v, %v; want %v", runIDs(got), err, onOther[:2])
	}

	// A cursor that does not belong to the direction is a mistake, not a guess.
	for _, p := range []RunPage{{After: 1, NewestFirst: true}, {Before: 1}} {
		if _, err := f.s.ListVisibleRuns(ctx, f.ch.ID, frank, 0, p, 10); err == nil {
			t.Errorf("ListVisibleRuns(%+v) = nil error", p)
		}
	}
}

// The run list must be served by an index in either direction, with no temp
// B-tree for the ORDER BY, with session= and without it (issue #317).
func TestListVisibleRunsPlan(t *testing.T) {
	f := newRunFixture(t)
	for _, session := range []int64{0, 7} {
		for _, scoped := range []bool{true, false} {
			for _, desc := range []bool{true, false} {
				t.Run(fmt.Sprintf("session=%d scoped=%v desc=%v", session, scoped, desc), func(t *testing.T) {
					_, visible, _ := runVisibility(Reader{PrincipalID: f.frank.ID, Scoped: scoped})
					args := append([]any{f.ch.ID, int64(0), int64(math.MaxInt64), session, session}, visible...)
					rows, err := f.s.db.Query("EXPLAIN QUERY PLAN "+runListSQL(scoped, desc), append(args, 11)...)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = rows.Close() }()
					var plan []string
					for rows.Next() {
						var id, parent, unused int
						var detail string
						if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
							t.Fatal(err)
						}
						plan = append(plan, detail)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					text := strings.Join(plan, "\n")
					t.Log("\n" + text)
					if strings.Contains(text, "TEMP B-TREE") {
						t.Errorf("the plan sorts in a temp B-tree:\n%s", text)
					}
					if !strings.Contains(text, "USING INDEX") && !strings.Contains(text, "USING COVERING INDEX") && !strings.Contains(text, "PRIMARY KEY") {
						t.Errorf("the plan uses no index:\n%s", text)
					}
				})
			}
		}
	}
}
