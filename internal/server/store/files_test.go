package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func (f *audienceFixture) file(t *testing.T, uploader Principal, name string) File {
	t.Helper()
	sum := fmt.Sprintf("%064x", len(name)*7919+int(uploader.ID))
	got, err := f.s.InsertFile(context.Background(), NewFile{
		ChannelID: f.ch.ID, UploaderID: uploader.ID, Filename: name, MediaType: "text/plain", Size: 3, SHA256: sum,
	})
	if err != nil {
		t.Fatalf("InsertFile %s: %v", name, err)
	}
	return got
}

func attachment(ids ...int64) *schema.Payload {
	data, _ := json.Marshal(schema.FileAttachmentV1{FileIDs: ids})
	return &schema.Payload{Schema: schema.FileAttachmentV1Name, Data: data}
}

func (f *audienceFixture) attachWide(author Principal, ids ...int64) error {
	_, err := f.s.InsertMessageV1(context.Background(), f.ch.ID, author.ID, "files", attachment(ids...))
	return err
}

func (f *audienceFixture) attachNet(author Principal, n Net, ids ...int64) error {
	_, _, err := f.s.InsertScopedMessage(context.Background(), ScopedPost{
		ChannelID: f.ch.ID, AuthorID: author.ID, Body: "files", Payload: attachment(ids...),
		Audience: schema.Audience{Kind: schema.AudienceKindNet, NetID: n.ID},
	})
	return err
}

func (f *audienceFixture) attachWhisper(author, to Principal, ids ...int64) error {
	_, _, err := f.s.InsertScopedMessage(context.Background(), ScopedPost{
		ChannelID: f.ch.ID, AuthorID: author.ID, Body: "files", Payload: attachment(ids...),
		Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{to.ID}}.Normalize(author.ID),
	})
	return err
}

// FileVisibleTo answers with the message's own rule: for each reader, exactly
// the files whose message the same reader could list.
func TestFileVisibleToExactSets(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	wide := f.file(t, f.alice, "wide")
	net := f.file(t, f.alice, "net")
	whisper := f.file(t, f.alice, "whisper")
	loose := f.file(t, f.alice, "loose")
	for name, err := range map[string]error{
		"wide":    f.attachWide(f.alice, wide.ID),
		"net":     f.attachNet(f.alice, f.n1, net.ID),
		"whisper": f.attachWhisper(f.alice, f.dave, whisper.ID),
	} {
		if err != nil {
			t.Fatalf("attach %s: %v", name, err)
		}
	}
	all := map[string]File{"wide": wide, "net": net, "whisper": whisper, "loose": loose}
	tests := []struct {
		name   string
		reader Reader
		want   []string
	}{
		{"uploader", Reader{f.alice.ID, true}, []string{"wide", "net", "whisper", "loose"}},
		{"uploader on a v1 reader", Reader{f.alice.ID, false}, []string{"wide", "net", "whisper", "loose"}},
		{"net member and whisper recipient", Reader{f.dave.ID, true}, []string{"wide", "net", "whisper"}},
		{"net monitor", Reader{f.bob.ID, true}, []string{"wide", "net"}},
		{"member in no audience", Reader{f.frank.ID, true}, []string{"wide"}},
		{"member of the other net only", Reader{f.erin.ID, true}, []string{"wide"}},
		{"net member, v0/v1 reader", Reader{f.dave.ID, false}, []string{"wide"}},
		{"non-member", Reader{f.carol.ID, true}, nil},
		{"no verified reader", Reader{0, true}, nil},
		{"channel-wide only", ChannelWideOnly, nil},
		{"unknown principal", Reader{9999, true}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, name := range []string{"wide", "net", "whisper", "loose"} {
				_, err := f.s.FileVisibleTo(ctx, all[name].ID, tt.reader)
				switch {
				case err == nil:
					got = append(got, name)
				case !errors.Is(err, ErrNotFound):
					t.Fatalf("%s: %v", name, err)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("visible files = %v, want %v", got, tt.want)
			}
		})
	}
	if _, err := f.s.FileVisibleTo(ctx, 424242, Reader{f.alice.ID, true}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown file = %v", err)
	}

	t.Run("disabled principal", func(t *testing.T) {
		if _, err := f.s.DisablePrincipal(ctx, "system", f.dave.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.FileVisibleTo(ctx, wide.ID, Reader{f.dave.ID, true}); !errors.Is(err, ErrNotFound) {
			t.Errorf("a disabled principal fetched a file: %v", err)
		}
	})
	t.Run("removed from the channel", func(t *testing.T) {
		if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.alice.ID); err != nil {
			t.Fatal(err)
		}
		for name, file := range all {
			if _, err := f.s.FileVisibleTo(ctx, file.ID, Reader{f.alice.ID, true}); !errors.Is(err, ErrNotFound) {
				t.Errorf("a removed uploader fetched %s: %v", name, err)
			}
		}
	})
}

// A post whose attachment fails stores nothing: no message, no audit row, and
// the files it did name are still unattached.
func TestAttachmentFailureRollsBackThePost(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	mine := f.file(t, f.alice, "mine")
	theirs := f.file(t, f.bob, "theirs")
	otherSum := fmt.Sprintf("%064x", 1)
	elsewhereFile := NewFile{ChannelID: f.other.ID, UploaderID: f.alice.ID, Filename: "x", MediaType: "text/plain", Size: 1, SHA256: otherSum}
	if _, err := f.s.InsertFile(ctx, elsewhereFile); !errors.Is(err, ErrNotChannelMember) {
		t.Fatalf("upload to a channel the uploader is not in = %v", err)
	}
	if _, err := f.s.AddChannelMember(ctx, "system", f.other.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	elsewhere, err := f.s.InsertFile(ctx, elsewhereFile)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := f.s.ListAuditEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		do   func() error
	}{
		{"someone else's file after a good one", func() error { return f.attachWide(f.alice, mine.ID, theirs.ID) }},
		{"another channel's file", func() error { return f.attachWide(f.alice, mine.ID, elsewhere.ID) }},
		{"unknown id", func() error { return f.attachNet(f.alice, f.n1, mine.ID, 99999) }},
		{"whisper with a bad file", func() error { return f.attachWhisper(f.alice, f.dave, mine.ID, theirs.ID) }},
		{"malformed payload", func() error {
			_, err := f.s.InsertMessageV1(ctx, f.ch.ID, f.alice.ID, "x", &schema.Payload{Schema: schema.FileAttachmentV1Name, Data: json.RawMessage(`{"file_ids":"1"}`)})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.do(); !errors.Is(err, ErrFileAttachment) {
				t.Fatalf("err = %v, want ErrFileAttachment", err)
			}
		})
	}
	if n, _ := f.s.CountMessages(ctx, f.ch.ID); n != 0 {
		t.Errorf("%d messages stored by refused posts", n)
	}
	auditAfter, err := f.s.ListAuditEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditAfter) != len(auditBefore) {
		t.Errorf("refused posts left %d audit rows", len(auditAfter)-len(auditBefore))
	}
	// "mine" was named first in several of them and must still be free.
	if err := f.attachWide(f.alice, mine.ID); err != nil {
		t.Errorf("a file named by a refused post could not be attached afterwards: %v", err)
	}
}

// Many transactions race for one file; the database lets exactly one in.
func TestConcurrentAttachExactlyOneWins(t *testing.T) {
	f := newAudienceFixture(t)
	for round := 0; round < 10; round++ {
		file := f.file(t, f.alice, fmt.Sprintf("race%d", round))
		const racers = 6
		errs := make([]error, racers)
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					errs[i] = f.attachWide(f.alice, file.ID)
				} else {
					errs[i] = f.attachNet(f.alice, f.n1, file.ID)
				}
			}()
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, ErrFileAttachment):
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d posts attached the file, errors %v", round, won, errs)
		}
	}
}

func TestInsertFileAuditsInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	file := f.file(t, f.alice, "plan")
	if err := f.attachWide(f.alice, file.ID); err != nil {
		t.Fatal(err)
	}
	events, err := f.s.ListAuditEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var uploaded, attached int
	for _, e := range events {
		if e.Subject != fmt.Sprintf("file:%d", file.ID) {
			continue
		}
		switch e.Action {
		case AuditFileUploaded:
			uploaded++
			if strings.Contains(e.Detail, "plan") || !strings.Contains(e.Detail, "sha256=") {
				t.Errorf("detail %q", e.Detail)
			}
		case AuditFileAttached:
			attached++
		}
	}
	if uploaded != 1 || attached != 1 {
		t.Errorf("uploaded=%d attached=%d audit rows", uploaded, attached)
	}
	// A non-member cannot add a file row, and a refused one writes no audit row.
	if _, err := f.s.InsertFile(ctx, NewFile{ChannelID: f.ch.ID, UploaderID: f.carol.ID, Filename: "x", MediaType: "a/b", Size: 0, SHA256: strings.Repeat("0", 64)}); !errors.Is(err, ErrNotChannelMember) {
		t.Errorf("non-member upload = %v", err)
	}
	if n, _ := f.s.CountFiles(ctx); n != 1 {
		t.Errorf("files = %d, want 1", n)
	}
}

// Only audience.go may link a file to a message: the attaching UPDATE and the
// join through which a reader reaches a file are the visibility question.
func TestOnlyAudienceGoTouchesTheFileMessageLink(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range files {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "audience.go" || name == "store.go" {
			continue
		}
		data, err := os.ReadFile(name) // #nosec G304 -- a source file of this package, from the listing above
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ToLower(string(data))
		for _, forbidden := range []string{"f.message_id", "files.message_id", "set message_id", "files_by_message"} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s contains %q; the link between a file and a message is touched only in audience.go", name, forbidden)
			}
		}
	}
}
