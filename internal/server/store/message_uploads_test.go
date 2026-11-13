package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// Files a message brings with it (issue #1798) are recorded in the
// transaction that inserts the message.

// uploadsOf is n files to record, and a count of how often Place ran.
func uploadsOf(n int, placeErr error) (*MessageUploads, *int) {
	placed := new(int)
	u := &MessageUploads{Place: func() error {
		*placed++
		return placeErr
	}}
	for i := range n {
		u.Files = append(u.Files, NewFile{
			// The channel and the uploader are not the caller's to choose.
			ChannelID: 999, UploaderID: 999,
			Filename: fmt.Sprintf("shot-%d.png", i), MediaType: "image/png", Size: int64(10 + i), SHA256: fmt.Sprintf("%064x", 4242+i),
		})
	}
	return u, placed
}

func attachedIDs(t *testing.T, m Message) []int64 {
	t.Helper()
	if m.Payload == nil || m.Payload.Schema != schema.FileAttachmentV1Name {
		t.Fatalf("the message carries payload %+v, want a %s", m.Payload, schema.FileAttachmentV1Name)
	}
	var att schema.FileAttachmentV1
	if err := json.Unmarshal(m.Payload.Data, &att); err != nil || att.Validate() != nil {
		t.Fatalf("payload %s is not a valid attachment: %v", m.Payload.Data, err)
	}
	return att.FileIDs
}

// Each kind of post stores its files and its message together: the rows are
// the author's, in the post's channel, in the order given, audited as
// uploads and as attachments, and readable by exactly the message's readers.
func TestMessageUploadsAreStoredWithTheirMessage(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	root := f.wide(t, f.bob, "how is it going?")
	tests := []struct {
		name   string
		post   func(u *MessageUploads) (Message, error)
		sees   Principal
		cannot Principal
	}{
		{"channel-wide", func(u *MessageUploads) (Message, error) {
			return f.s.InsertMessageWithUploads(ctx, f.ch.ID, f.alice.ID, "progress", u)
		}, f.erin, f.carol},
		{"to a net", func(u *MessageUploads) (Message, error) {
			m, _, err := f.s.InsertScopedMessage(ctx, ScopedPost{ChannelID: f.ch.ID, AuthorID: f.alice.ID, Body: "progress", Uploads: u,
				Audience: schema.Audience{Kind: schema.AudienceKindNet, NetID: f.n1.ID}})
			return m, err
		}, f.dave, f.erin},
		{"a whisper", func(u *MessageUploads) (Message, error) {
			m, _, err := f.s.InsertScopedMessage(ctx, ScopedPost{ChannelID: f.ch.ID, AuthorID: f.alice.ID, Body: "progress", Uploads: u,
				Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{f.dave.ID}}.Normalize(f.alice.ID)})
			return m, err
		}, f.dave, f.erin},
		{"a reply", func(u *MessageUploads) (Message, error) {
			m, _, err := f.s.InsertReplyMessage(ctx, ReplyPost{ChannelID: f.ch.ID, AuthorID: f.alice.ID, RootID: root, Body: "progress", Uploads: u},
				Reader{f.alice.ID, true})
			return m, err
		}, f.erin, f.carol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, err := f.s.ListAuditEvents(ctx, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			u, placed := uploadsOf(2, nil)
			m, err := tt.post(u)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			if *placed != 1 {
				t.Errorf("Place ran %d times, want once", *placed)
			}
			ids := attachedIDs(t, m)
			if len(ids) != 2 {
				t.Fatalf("the message attaches %v, want two files", ids)
			}
			for i, id := range ids {
				file, err := f.s.FileVisibleTo(ctx, id, Reader{tt.sees.ID, true})
				if err != nil {
					t.Fatalf("%s, who can see the message, reading file %d: %v", tt.sees.Name, id, err)
				}
				if file.ChannelID != f.ch.ID || file.UploaderID != f.alice.ID || file.Filename != u.Files[i].Filename ||
					file.MediaType != "image/png" || file.Size != u.Files[i].Size || file.SHA256 != u.Files[i].SHA256 {
					t.Errorf("file %d = %+v, want %+v in the post's channel by its author", i, file, u.Files[i])
				}
				if _, err := f.s.FileVisibleTo(ctx, id, Reader{tt.cannot.ID, true}); !errors.Is(err, ErrNotFound) {
					t.Errorf("%s, who cannot see the message, reading file %d = %v, want ErrNotFound", tt.cannot.Name, id, err)
				}
			}
			after, err := f.s.ListAuditEvents(ctx, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, e := range after[len(before):] {
				counts[e.Action]++
				if e.Actor != fmt.Sprintf("principal:%d", f.alice.ID) {
					t.Errorf("audit row %+v is not the author's", e)
				}
				if e.Action == AuditFileUploaded && (strings.Contains(e.Detail, "shot-") || !strings.Contains(e.Detail, fmt.Sprintf("channel=%d ", f.ch.ID))) {
					t.Errorf("file_uploaded detail %q", e.Detail)
				}
				if e.Action == AuditFileAttached && e.Detail != fmt.Sprintf("message=%d", m.ID) {
					t.Errorf("file_attached detail %q, want message=%d", e.Detail, m.ID)
				}
			}
			if counts[AuditFileUploaded] != 2 || counts[AuditFileAttached] != 2 || counts["message.post"] != 1 {
				t.Errorf("audit rows of the post = %v, want two uploads, two attachments and one message", counts)
			}
		})
	}
}

// A post that is refused, or whose bytes cannot be put in place, stores
// nothing: no file row, no message and no audit row. Place is the last step,
// so a post refused for any other reason has placed nothing.
func TestRefusedMessageUploadsStoreNothing(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	root := f.wide(t, f.bob, "how is it going?")
	errPlace := errors.New("the disk is full")
	wide := func(author Principal) func(u *MessageUploads) error {
		return func(u *MessageUploads) error {
			_, err := f.s.InsertMessageWithUploads(ctx, f.ch.ID, author.ID, "progress", u)
			return err
		}
	}
	net := func(author Principal, payload *schema.Payload) func(u *MessageUploads) error {
		return func(u *MessageUploads) error {
			_, _, err := f.s.InsertScopedMessage(ctx, ScopedPost{ChannelID: f.ch.ID, AuthorID: author.ID, Body: "progress", Payload: payload, Uploads: u,
				Audience: schema.Audience{Kind: schema.AudienceKindNet, NetID: f.n1.ID}})
			return err
		}
	}
	reply := func(rootID int64, payload *schema.Payload) func(u *MessageUploads) error {
		return func(u *MessageUploads) error {
			_, _, err := f.s.InsertReplyMessage(ctx, ReplyPost{ChannelID: f.ch.ID, AuthorID: f.alice.ID, RootID: rootID, Body: "progress", Payload: payload, Uploads: u},
				Reader{f.alice.ID, true})
			return err
		}
	}
	other := &schema.Payload{Schema: "test.thing.v1", Data: json.RawMessage(`{}`)}
	tests := []struct {
		name     string
		files    int
		placeErr error
		post     func(u *MessageUploads) error
		wantErr  error // nil for an error that is the caller's own mistake
		// wantPlaced is whether the post got as far as its last step.
		wantPlaced bool
	}{
		{"channel-wide, the bytes cannot be placed", 2, errPlace, wide(f.alice), errPlace, true},
		{"to a net, the bytes cannot be placed", 2, errPlace, net(f.alice, nil), errPlace, true},
		{"a reply, the bytes cannot be placed", 2, errPlace, reply(root, nil), errPlace, true},
		{"an author who is not in the channel", 1, nil, wide(f.carol), ErrNotChannelMember, false},
		{"a net the author is not on", 1, nil, net(f.erin, nil), ErrNetNotFound, false},
		{"a net the author only monitors", 1, nil, net(f.bob, nil), ErrNetMonitorOnly, false},
		{"a reply to a message that does not exist", 1, nil, reply(99999, nil), ErrThreadNotFound, false},
		{"a payload as well, to a net", 1, nil, net(f.alice, other), nil, false},
		{"a payload as well, in a reply", 1, nil, reply(root, other), nil, false},
		{"no files", 0, nil, wide(f.alice), ErrFileAttachment, false},
		{"more files than a message may attach", schema.MaxFileAttachments + 1, nil, wide(f.alice), ErrFileAttachment, false},
	}
	messages, err := f.s.CountMessages(ctx, f.ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.s.ListAuditEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, placed := uploadsOf(tt.files, tt.placeErr)
			err := tt.post(u)
			if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if (*placed == 1) != tt.wantPlaced || *placed > 1 {
				t.Errorf("Place ran %d times, want placed = %v", *placed, tt.wantPlaced)
			}
			if n, _ := f.s.CountFiles(ctx); n != 0 {
				t.Errorf("%d file row(s) after a refused post", n)
			}
			if n, _ := f.s.CountMessages(ctx, f.ch.ID); n != messages {
				t.Errorf("%d message(s) stored by a refused post", n-messages)
			}
			after, err := f.s.ListAuditEvents(ctx, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Errorf("a refused post left %d audit row(s)", len(after)-len(before))
			}
		})
	}
}
