package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// PostReplyV2 posts body to channel as a reply to the message rootID. A reply
// takes its root's audience from the server, so the request carries none. The
// server binds the author when the client has a credential.
func (c *Client) PostReplyV2(ctx context.Context, channel string, authorID, rootID int64, body string) (schema.MessageV2, error) {
	request := schema.PostMessageRequestV2{AuthorID: authorID, Body: body, ThreadRootID: rootID}
	var result schema.PostMessageResponseV2
	endpoint := c.resolve("v2", "channels", channel, "messages")
	if err := c.roundTrip(ctx, "post v2 reply", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.MessageV2{}, err
	}
	return result.Message, nil
}

// ListThreadV2 returns one forward page of the thread rootID in channel: the
// root first (on the first page), then its replies, limited to what the caller
// may see.
func (c *Client) ListThreadV2(ctx context.Context, channel string, rootID, after int64, limit int) (schema.ListMessagesResponseV2, error) {
	endpoint := c.resolve("v2", "channels", channel, "messages")
	query := endpoint.Query()
	query.Set("thread", strconv.FormatInt(rootID, 10))
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()
	var result schema.ListMessagesResponseV2
	if err := c.roundTrip(ctx, "list v2 thread", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListMessagesResponseV2{}, err
	}
	return result, nil
}

// threadPage is how many messages `conch thread` asks for at a time.
const threadPage = 100

// runThread is `conch thread [--server <url>] <channel> <id>`. It prints the
// thread whose root is message <id>, the root first and then the replies in
// order, in the format tail prints them. A message that is not there to you,
// or is itself a reply, is the server's one "thread not found" answer.
func runThread(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("thread", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: thread: %w", err)
	}
	if fs.NArg() != 2 {
		return errors.New("cli: thread: expected <channel> <id>")
	}
	root, err := strconv.ParseInt(fs.Arg(1), 10, 64)
	if err != nil || root <= 0 {
		return errors.New("cli: thread: <id> must be a positive message id")
	}
	channel := fs.Arg(0)
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	var names map[int64]string // resolved once, when the first page has messages
	after := int64(0)
	for {
		page, err := client.ListThreadV2(ctx, channel, root, after, threadPage)
		if err != nil {
			return err
		}
		if names == nil && len(page.Messages) > 0 {
			names = netNames(ctx, client, channel)
		}
		for _, message := range page.Messages {
			if _, err := fmt.Fprintln(stdout, messageLine(message, names)); err != nil {
				return err
			}
		}
		// A page that claims another but moves nowhere would loop forever.
		if page.NextAfter <= after {
			return nil
		}
		after = page.NextAfter
	}
}

// netNames maps the ids of the nets the caller can list to their names. A net
// the caller cannot list, or one created later, is shown by id.
func netNames(ctx context.Context, client *Client, channel string) map[int64]string {
	names := make(map[int64]string)
	nets, err := client.ListNets(ctx, channel)
	if err != nil {
		return names
	}
	for _, n := range nets.Nets {
		names[n.ID] = n.Name
	}
	return names
}

// messageLine is one message as tail, search and thread print it, without the
// newline: time, author, the audience marker, a reply marker naming the root,
// then the body. Every server-supplied string goes through scopeMarker's
// safeLine or tailBody (issue #234); the reply marker is a number the program
// formats itself.
func messageLine(message schema.MessageV2, names map[int64]string) string {
	reply := ""
	if message.ThreadRootID > 0 {
		reply = "[reply:" + strconv.FormatInt(message.ThreadRootID, 10) + "] "
	}
	return fmt.Sprintf("%s %d %s%s%s", message.CreatedAt.Time().Format(time.RFC3339Nano), message.AuthorID,
		scopeMarker(message.Audience, names), reply, tailBody(message.Body))
}
