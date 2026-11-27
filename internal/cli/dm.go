package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

const dmUsage = `  conch dm list [--server <url>] [--archived] [--cursor <cursor>]
  conch dm open [--server <url>] <principal-id>
  conch dm send [--server <url>] [--nonce <nonce>] <conversation-id> <text>
  conch dm history [--server <url>] [--before <message-id>] [--limit <n>] <conversation-id>
  conch dm mark-read [--server <url>] <conversation-id> <message-id>
  conch dm edit [--server <url>] <conversation-id> <message-id> <text>
  conch dm remove [--server <url>] <conversation-id> <message-id>

  For send retries within 24 hours, reuse the same --nonce, conversation and text.
`

// NewDMText creates a text send with a nonce to keep for any retry.
func NewDMText(body string) schema.PostDMMessageRequestV1 {
	return schema.PostDMMessageRequestV1{Body: body, ClientNonce: rand.Text()}
}

func runDM(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("cli: dm requires a subcommand\n%s", dmUsage)
	}
	verb := args[0]
	switch verb {
	case "help", "-h", "--help":
		_, err := io.WriteString(stdout, dmUsage)
		return err
	case "list", "open", "send", "history", "mark-read", "edit", "remove":
	default:
		return fmt.Errorf("cli: unknown dm subcommand %q", verb)
	}
	fs := newFlagSet("dm "+verb, stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, dmUsage) }
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	var before int64
	var limit int
	var archived bool
	var cursor, nonce string
	switch verb {
	case "list":
		fs.BoolVar(&archived, "archived", false, "list archived conversations")
		fs.StringVar(&cursor, "cursor", "", "continue the conversation listing")
	case "history":
		fs.Int64Var(&before, "before", 0, "list messages older than this message ID")
		fs.IntVar(&limit, "limit", schema.DefaultDMPageSize, "maximum messages")
	case "send":
		fs.StringVar(&nonce, "nonce", rand.Text(), "reuse the original nonce when retrying a send")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	pos := fs.Args()
	want := map[string]int{"list": 0, "open": 1, "send": 2, "history": 1, "mark-read": 2, "edit": 3, "remove": 2}[verb]
	if len(pos) != want {
		return fmt.Errorf("cli: dm %s requires %d arguments; see 'conch dm help'", verb, want)
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	if verb == "list" {
		return writeDMList(ctx, client, archived, cursor, stdout)
	}
	id, err := positiveID(pos[0])
	if err != nil {
		return fmt.Errorf("cli: dm %s requires a positive principal or conversation ID", verb)
	}
	switch verb {
	case "open":
		conversation, err := client.OpenDM(ctx, id)
		if err != nil {
			return err
		}
		return writeDMConversation(stdout, conversation)
	case "send":
		request := schema.PostDMMessageRequestV1{Body: pos[1], ClientNonce: nonce}
		if err := request.Validate(); err != nil {
			return err
		}
		message, err := client.SendDM(ctx, id, request)
		if err != nil {
			var refusal *ServerError
			if !errors.As(err, &refusal) || refusal.Status >= http.StatusInternalServerError {
				_, _ = fmt.Fprintf(stderr, "DM delivery could not be confirmed. To retry within 24 hours, use --nonce %s with the same conversation and text.\n", nonce)
			}
		}
		return writeDMResult(stdout, message, err)
	case "history":
		if before < 0 || limit < 1 || limit > schema.MaxDMPageSize {
			return fmt.Errorf("cli: dm history requires a nonnegative --before and a --limit from 1 to %d", schema.MaxDMPageSize)
		}
		return writeDMHistory(ctx, client, id, before, limit, stdout)
	}
	messageID, err := positiveID(pos[1])
	if err != nil {
		return fmt.Errorf("cli: dm %s requires a positive message ID", verb)
	}
	switch verb {
	case "edit":
		message, err := client.EditDM(ctx, id, messageID, pos[2])
		return writeDMResult(stdout, message, err)
	case "remove":
		return client.RemoveDM(ctx, id, messageID)
	default: // mark-read
		state, err := client.MarkDMRead(ctx, id, messageID)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "read through %d (%d unread)\n", state.LastReadID, state.Unread)
		return err
	}
}

func writeDMList(ctx context.Context, client *Client, archived bool, cursor string, w io.Writer) error {
	state := schema.DMStateOpen
	if archived {
		state = schema.DMStateArchived
	}
	page, err := client.ListDMs(ctx, state, cursor)
	if err != nil {
		return err
	}
	for _, conversation := range page.Conversations {
		if err := writeDMConversation(w, conversation); err != nil {
			return err
		}
	}
	if page.NextCursor != "" {
		_, err = fmt.Fprintf(w, "more: --cursor %s\n", page.NextCursor)
	}
	return err
}

func writeDMConversation(w io.Writer, conversation schema.DMConversationV1) error {
	peer := strings.TrimSpace(safeLine(conversation.PeerName))
	if peer == "" {
		peer = "person"
	}
	_, err := fmt.Fprintf(w, "%d  %s (#%d)  %d unread\n", conversation.ID, peer, conversation.PeerID, conversation.Unread)
	return err
}

func writeDMHistory(ctx context.Context, client *Client, id, before int64, limit int, w io.Writer) error {
	conversation, err := client.GetDM(ctx, id)
	if err != nil {
		return err
	}
	page, err := client.ListDMMessages(ctx, id, before, limit)
	if err != nil {
		return err
	}
	if err := writeDMConversation(w, conversation); err != nil {
		return err
	}
	for i := len(page.Messages) - 1; i >= 0; i-- {
		message := page.Messages[i]
		if _, err := fmt.Fprintf(w, "%d  #%d  %s\n", message.ID, message.AuthorID, tailBody(message.Body)); err != nil {
			return err
		}
	}
	if page.NextBefore > 0 {
		_, err = fmt.Fprintf(w, "older: --before %d\n", page.NextBefore)
	}
	return err
}

func writeDMResult(w io.Writer, message schema.DMMessageV1, err error) error {
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, message.ID)
	return err
}
