package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// Personal blocks (issue #1832, docs/design/blocks.md): the caller's own
// list, through the same REST routes the browser uses.
//
//	conch block list
//	conch block add <principal id>
//	conch block remove <principal id>
//
// A block is the caller's preference: the blocked person's messages are
// collapsed in the caller's clients and raise no alert for them. It removes
// nobody from anything, and the person blocked is not told.

// ListBlocks reads the caller's block list.
func (c *Client) ListBlocks(ctx context.Context) (schema.ListBlocksResponseV1, error) {
	var out schema.ListBlocksResponseV1
	if err := c.roundTrip(ctx, "list blocks", http.MethodGet, c.resolve("v1", "blocks"), nil, &out); err != nil {
		return schema.ListBlocksResponseV1{}, err
	}
	if err := out.Validate(); err != nil {
		return schema.ListBlocksResponseV1{}, fmt.Errorf("cli: list blocks: %w", err)
	}
	return out, nil
}

// Block adds principalID to the caller's list; a repeat answers the entry
// as it was.
func (c *Client) Block(ctx context.Context, principalID int64) (schema.BlockV1, error) {
	if principalID <= 0 {
		return schema.BlockV1{}, errors.New("cli: block: principal id must be positive")
	}
	var out schema.BlockV1
	if err := c.roundTrip(ctx, "block", http.MethodPut, c.resolve("v1", "blocks", strconv.FormatInt(principalID, 10)), nil, &out); err != nil {
		return schema.BlockV1{}, err
	}
	return out, out.Validate()
}

// Unblock removes principalID from the caller's list; removing one that is
// not there is not an error.
func (c *Client) Unblock(ctx context.Context, principalID int64) error {
	if principalID <= 0 {
		return errors.New("cli: unblock: principal id must be positive")
	}
	return c.roundTrip(ctx, "unblock", http.MethodDelete, c.resolve("v1", "blocks", strconv.FormatInt(principalID, 10)), nil, nil)
}

const blockUsage = "cli: block: usage: conch block list|add <principal id>|remove <principal id> [--server <url>] [--json]"

func runBlock(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("block", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	asJSON := fs.Bool("json", false, "print the server's response document")
	// Flags may follow the words, as they may for rename.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("cli: block: %w", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) == 0 {
		return errors.New(blockUsage)
	}
	var id int64
	switch pos[0] {
	case "list":
		if len(pos) != 1 {
			return errors.New(blockUsage)
		}
	case "add", "remove":
		if len(pos) != 2 {
			return errors.New(blockUsage)
		}
		n, err := strconv.ParseInt(pos[1], 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("cli: block: %q is not a principal id", pos[1])
		}
		id = n
	default:
		return errors.New(blockUsage)
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	switch pos[0] {
	case "list":
		list, err := client.ListBlocks(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(list)
		}
		if len(list.Blocks) == 0 {
			_, err = fmt.Fprintln(stdout, "you have not blocked anyone")
			return err
		}
		for _, b := range list.Blocks {
			if _, err := fmt.Fprintf(stdout, "%d  %s  %s  blocked %s\n", b.PrincipalID, b.Kind, safeLine(b.Name), b.CreatedAt.Time().Format("2006-01-02 15:04")); err != nil {
				return err
			}
		}
		return nil
	case "add":
		b, err := client.Block(ctx, id)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(b)
		}
		_, err = fmt.Fprintf(stdout, "blocked %s (%d): their messages are collapsed and raise no alert for you; they are not told\n", safeLine(b.Name), b.PrincipalID)
		return err
	default:
		if err := client.Unblock(ctx, id); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "unblocked principal %d\n", id)
		return err
	}
}
