package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

func bindPreviewAs(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.previewAs, "preview-as", "", "read as this member (name or principal id)")
}

func resolvePreviewMember(ctx context.Context, client *Client, member string) (int64, error) {
	if id, err := strconv.ParseInt(member, 10, 64); err == nil {
		if id <= 0 {
			return 0, errors.New("cli: preview member id must be a positive integer")
		}
		return id, nil
	}
	if member == "" {
		return 0, errors.New("cli: preview member must be a name or positive principal id")
	}
	var after int64
	for page := 0; ; page++ {
		if page == maxDirectoryPages {
			return 0, errors.New("cli: preview member lookup: the server's pages do not end")
		}
		resp, err := client.DirectoryMembers(ctx, after, schema.MaxListPrincipalsLimit)
		if err != nil {
			return 0, err
		}
		var found int64
		for _, candidate := range resp.Members {
			if candidate.Name != member {
				continue
			}
			if found != 0 {
				return 0, fmt.Errorf("cli: preview member %s is listed more than once", safeLine(member))
			}
			found = candidate.ID
		}
		if found != 0 {
			return found, nil
		}
		if resp.After == 0 {
			return 0, fmt.Errorf("cli: no member %s in the directory", safeLine(strconv.Quote(member)))
		}
		if resp.After <= after {
			return 0, errors.New("cli: preview member lookup: the server's pages do not move forward")
		}
		after = resp.After
	}
}

func adminPreview(ctx context.Context, r *adminRun) error {
	id, err := resolvePreviewMember(ctx, r.client, r.pos[0])
	if err != nil {
		return err
	}
	preview, err := r.client.Preview(ctx, id)
	if err != nil {
		return err
	}
	channels, err := r.client.ListChannelsPreview(ctx, id)
	if err != nil {
		return err
	}
	directory, err := readDirectoryMembers(ctx, r, id)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		enc := json.NewEncoder(r.stdout)
		for _, document := range []any{preview, channels, directory} {
			if err := enc.Encode(document); err != nil {
				return err
			}
		}
		return nil
	}
	return writeAdminPreview(r.stdout, preview, channels, directory)
}

func writeAdminPreview(w io.Writer, preview schema.PreviewResponseV1, channels schema.ListChannelsResponse, directory schema.ServerMembersResponseV1) error {
	if _, err := fmt.Fprintf(w, "member: %s (id %d)\nrole: %s\nowner: %t\n", safeLine(preview.Name), preview.ID, safeLine(string(preview.Role)), preview.Owner); err != nil {
		return err
	}
	permissions := make([]string, len(preview.Permissions))
	for i, permission := range preview.Permissions {
		permissions[i] = safeLine(string(permission))
	}
	if _, err := fmt.Fprintf(w, "permissions: %s\nchannels:\n", dashIfEmpty(strings.Join(permissions, ", "))); err != nil {
		return err
	}
	for _, channel := range channels.Channels {
		if _, err := fmt.Fprintf(w, "%d %s %s %s %s\n", channel.ID, safeLine(string(channel.Visibility)),
			either(channel.Manages, "manages", "-"), either(channel.Default, "default", "-"), safeLine(channel.Name)); err != nil {
			return err
		}
	}
	scope := directory.Scope
	if scope == "" {
		scope = schema.DirectoryScopeEveryone
	}
	if _, err := fmt.Fprintf(w, "directory: %s\nmembers:\n", safeLine(string(scope))); err != nil {
		return err
	}
	return writeDirectoryMembers(w, directory.Members)
}
