package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// UploadFile sends body (size bytes) to channel as a file called filename and
// returns its metadata. The media type is only what the uploader declares: the
// server records it and never serves a download with it. A size of -1 sends
// the body chunked.
func (c *Client) UploadFile(ctx context.Context, channel, filename, mediaType string, body io.Reader, size int64) (schema.FileV1, error) {
	endpoint := c.resolve("v2", "channels", channel, "files")
	endpoint.RawQuery = url.Values{"filename": {filename}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return schema.FileV1{}, fmt.Errorf("cli: create upload request: %w", err)
	}
	if size >= 0 {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	resp, err := c.do(req)
	if err != nil {
		return schema.FileV1{}, fmt.Errorf("cli: upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return schema.FileV1{}, c.decodeError(resp)
	}
	var meta schema.FileV1
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return schema.FileV1{}, fmt.Errorf("cli: decode upload response: %w", err)
	}
	return meta, nil
}

// FileMeta returns the metadata of file id. What it holds is whatever the
// server sent: nothing in it is checked here beyond decoding, and a caller must
// not use its filename as a path without safeDownloadName.
func (c *Client) FileMeta(ctx context.Context, id int64) (schema.FileV1, error) {
	var meta schema.FileV1
	endpoint := c.resolve("v2", "files", strconv.FormatInt(id, 10), "meta")
	if err := c.roundTrip(ctx, "file metadata", http.MethodGet, endpoint, nil, &meta); err != nil {
		return schema.FileV1{}, err
	}
	return meta, nil
}

// DownloadFile copies file id's bytes to dst. At most size bytes are accepted:
// a server that sends more than the metadata promised is cut off and reported,
// so a hostile or broken server cannot make the client write without bound. It
// returns the SHA-256 (lowercase hex) of what it copied.
func (c *Client) DownloadFile(ctx context.Context, id, size int64, dst io.Writer) (string, error) {
	endpoint := c.resolve("v2", "files", strconv.FormatInt(id, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("cli: create download request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return "", fmt.Errorf("cli: download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", c.decodeError(resp)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, size+1))
	if err != nil {
		return "", fmt.Errorf("cli: download: %w", err)
	}
	if n > size {
		return "", errors.New("cli: download: the server sent more bytes than the file's size")
	}
	if n < size {
		return "", fmt.Errorf("cli: download: the server sent %d bytes of %d", n, size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PostFileMessageV2 posts a channel-wide message that attaches the files.
func (c *Client) PostFileMessageV2(ctx context.Context, channel string, authorID int64, body string, fileIDs []int64) (schema.MessageV2, error) {
	data, err := json.Marshal(schema.FileAttachmentV1{FileIDs: fileIDs})
	if err != nil {
		return schema.MessageV2{}, fmt.Errorf("cli: encode file attachment: %w", err)
	}
	request := schema.PostMessageRequestV2{AuthorID: authorID, Body: body,
		Payload: &schema.Payload{Schema: schema.FileAttachmentV1Name, Data: data}}
	var result schema.PostMessageResponseV2
	endpoint := c.resolve("v2", "channels", channel, "messages")
	if err := c.roundTrip(ctx, "post v2 file message", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.MessageV2{}, err
	}
	return result.Message, nil
}

// runUpload is `conch upload [--server <url>] [--message <text>] <channel>
// <path>`: it uploads the file, posts one channel-wide message attaching it,
// and prints the file id.
func runUpload(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("upload", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	message := fs.String("message", "", "text of the message that attaches the file (default: the file's name)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: upload: %w", err)
	}
	if fs.NArg() != 2 {
		return errors.New("cli: upload: expected <channel> <path>")
	}
	channel, path := fs.Arg(0), fs.Arg(1)
	name := filepath.Base(path)
	if err := schema.ValidateFileName(name); err != nil {
		return fmt.Errorf("cli: upload: %q cannot be uploaded under its name: %w", name, err)
	}
	file, err := os.Open(path) // #nosec G304 -- the user named this file on their own command line
	if err != nil {
		return fmt.Errorf("cli: upload: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("cli: upload: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cli: upload: %q is not a regular file", path)
	}
	client, hasCredential, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	authorID, err := resolveAuthor(ctx, client, hasCredential, "", "upload", stderr)
	if err != nil {
		return err
	}
	meta, err := client.UploadFile(ctx, channel, name, declaredMediaType(name), file, info.Size())
	if err != nil {
		return err
	}
	text := *message
	if text == "" {
		text = name
	}
	if _, err := client.PostFileMessageV2(ctx, channel, authorID, text, []int64{meta.ID}); err != nil {
		_, _ = fmt.Fprintf(stderr, "conch: file %d was uploaded but is not attached to any message\n", meta.ID)
		return err
	}
	_, err = fmt.Fprintln(stdout, meta.ID)
	return err
}

// declaredMediaType is the media type the client declares for name, from its
// extension. It is information for the metadata and nothing more.
func declaredMediaType(name string) string {
	if mt, _, err := mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(name))); err == nil {
		return mt
	}
	return schema.FileDownloadMediaType
}

// safeDownloadName returns name if it is safe to use as the name of a new file
// in the current directory, and an error otherwise. The name came from a
// server, so it is checked as if hostile: it must be a valid file name by the
// protocol's own rule, and must be exactly its own base name, so it carries no
// directory part on any platform.
func safeDownloadName(name string) (string, error) {
	if err := schema.ValidateFileName(name); err != nil {
		return "", err
	}
	if filepath.Base(name) != name || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", errors.New("the name has a directory part")
	}
	return name, nil
}

// runDownload is `conch download [--server <url>] [-o <path>] <file-id>`. It
// writes to -o, or to the stored file name in the current directory. It never
// overwrites an existing file, and a name the server sent is never allowed to
// reach outside the current directory: such a name is refused and -o must be
// given.
func runDownload(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("download", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	output := fs.String("o", "", "write to this path instead of the file's own name in the current directory")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: download: %w", err)
	}
	if fs.NArg() != 1 {
		return errors.New("cli: download: expected <file-id>")
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		return errors.New("cli: download: the file id must be a positive integer")
	}
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "o" })
	if given && *output == "" {
		return errors.New("cli: download: -o needs a path")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	meta, err := client.FileMeta(ctx, id)
	if err != nil {
		return err
	}
	if meta.Size < 0 || !schema.ValidSHA256Hex(meta.SHA256) {
		return errors.New("cli: download: the server sent unusable metadata")
	}
	out, shown, done, err := createDownload(*output, meta.Filename)
	if err != nil {
		return err
	}
	// From here the file is ours: a failure removes what was written.
	ok := false
	defer func() {
		_ = out.Close()
		done(!ok)
	}()
	sum, err := client.DownloadFile(ctx, id, meta.Size, out)
	if err != nil {
		return err
	}
	if sum != meta.SHA256 {
		return errors.New("cli: download: the bytes do not match the file's sha256")
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("cli: download: %w", err)
	}
	ok = true
	_, err = fmt.Fprintln(stdout, shown)
	return err
}

// createDownload creates the file a download is written to, and refuses to
// overwrite one that exists (O_EXCL also refuses a symbolic link in its place,
// so nothing is followed). It returns the file, the path to report, and a
// function to call when finished: it removes the file again if the download
// failed, and releases the directory handle either way.
//
// With output, the path is the user's own and is used as given. Without it the
// destination is the stored name, which a server chose: it is checked by
// safeDownloadName, and the file is created through an os.Root on the current
// directory, which cannot be made to leave it by any name or any symbolic link.
func createDownload(output, storedName string) (*os.File, string, func(failed bool), error) {
	const flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if output != "" {
		f, err := os.OpenFile(output, flags, 0o600) // #nosec G304 -- the user's own -o path
		if err != nil {
			return nil, "", nil, downloadCreateError(err, output)
		}
		return f, output, func(failed bool) {
			if failed {
				_ = os.Remove(output) // #nosec G122 -- the file just created
			}
		}, nil
	}
	name, err := safeDownloadName(storedName)
	if err != nil {
		return nil, "", nil, fmt.Errorf("cli: download: the server sent a file name that cannot be used safely (%w); choose a path with -o", err)
	}
	root, err := os.OpenRoot(".")
	if err != nil {
		return nil, "", nil, fmt.Errorf("cli: download: %w", err)
	}
	f, err := root.OpenFile(name, flags, 0o600)
	if err != nil {
		_ = root.Close()
		return nil, "", nil, downloadCreateError(err, name)
	}
	return f, name, func(failed bool) {
		if failed {
			_ = root.Remove(name)
		}
		_ = root.Close()
	}, nil
}

func downloadCreateError(err error, path string) error {
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("cli: download: %q already exists; it is not overwritten (choose another path with -o)", path)
	}
	return fmt.Errorf("cli: download: %w", err)
}
