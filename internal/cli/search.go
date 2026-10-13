package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// SearchMessagesV2 searches one channel's messages for the words, as the
// caller may see them, newest first. before is the next_before of the previous
// page (0 for the newest) and a limit of 0 leaves the page size to the server.
// The words are sent as one q; the server decides what they match, and treats
// them as plain words.
func (c *Client) SearchMessagesV2(ctx context.Context, channel, words string, before int64, limit int) (schema.SearchMessagesResponseV2, error) {
	endpoint := c.resolve("v2", "channels", channel, "messages", "search")
	query := url.Values{"q": {words}}
	if before > 0 {
		query.Set("before", strconv.FormatInt(before, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	endpoint.RawQuery = query.Encode()
	var result schema.SearchMessagesResponseV2
	if err := c.roundTrip(ctx, "search v2 messages", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.SearchMessagesResponseV2{}, err
	}
	return result, nil
}

// runSearch is `conch search [--server <url>] [--limit <n>] [--before <id>]
// <channel> <words…>`. It prints the matches in the format tail prints them,
// newest first; no match prints nothing and succeeds.
func runSearch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("search", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	limit := fs.Int("limit", 0, "most matches to print (1 to 100; the server's default when omitted)")
	before := fs.Int64("before", 0, "only messages with a smaller id: the next page of an earlier search")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: search: %w", err)
	}
	if fs.NArg() < 2 {
		return errors.New("cli: search: expected <channel> <words>")
	}
	if *limit < 0 || *before < 0 {
		return errors.New("cli: search: --limit and --before must not be negative")
	}
	channel, words := fs.Arg(0), strings.Join(fs.Args()[1:], " ")
	if strings.TrimSpace(words) == "" {
		return errors.New("cli: search: expected <channel> <words>")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	result, err := client.SearchMessagesV2(ctx, channel, words, *before, *limit)
	if err != nil {
		return err
	}
	// As tail does: a net the caller cannot list is shown by id.
	names := make(map[int64]string)
	if len(result.Messages) > 0 {
		nets, err := client.ListNets(ctx, channel)
		if errors.Is(err, ErrUnauthenticated) {
			return err
		}
		if err == nil {
			for _, n := range nets.Nets {
				names[n.ID] = n.Name
			}
		}
	}
	for _, message := range result.Messages {
		if _, err := fmt.Fprintln(stdout, messageLine(message, names)); err != nil {
			return err
		}
	}
	if result.NextBefore > 0 {
		_, _ = fmt.Fprintf(stderr, "conch: more matches; continue with --before %d\n", result.NextBefore)
	}
	return nil
}
