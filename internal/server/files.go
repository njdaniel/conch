package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// File uploads (issue #23).
//
// Two things make this code security-sensitive. The first is who may read a
// file: it is a second way to read what a message carries, so it is fetchable
// only by its uploader and by those the message it is attached to is visible
// to, decided by the store (FileVisibleTo, in audience.go, with the message
// visibility rule). Every refusal is one and the same 404.
//
// The second is that the server stores and returns bytes it did not write,
// under a name it did not choose. Nothing a caller sends is ever part of a
// path: the file on disk is <data>/files/<sha256[:2]>/<sha256>, where the hash
// is computed here from the bytes. The filename and the media type are
// metadata, kept in the database and returned as data, and a download is never
// rendered: it is application/octet-stream, an attachment, nosniff.

const (
	// DefaultMaxUploadBytes is the upload limit conchd uses when none is set.
	DefaultMaxUploadBytes = 10 << 20

	// filesDirName is the directory of file bytes under the data directory.
	filesDirName = "files"
	// uploadTempPrefix names the temporary file an upload is written to. It
	// is a constant of ours; the random part comes from os.CreateTemp.
	uploadTempPrefix = ".upload-"

	// denyAgentFiles is an agent refused on a file route. The manifest has no
	// capability for files, so agents cannot upload or fetch (issue #23).
	denyAgentFiles = "agents_no_files"
)

var (
	// errInvalidFileAttachment is the one answer to a post whose
	// conch.file_attachment.v1 payload cannot be honoured, whichever way it
	// fails: malformed, an unknown file, another channel's, someone else's,
	// or already attached.
	errInvalidFileAttachment = &schema.Error{Code: "invalid_file_attachment", Message: "every attached file must be one you uploaded to this channel and have not yet attached"}
	errFileNotFound          = &schema.Error{Code: "file_not_found", Message: "file not found"}
	// errFilesNeedAuth answers every file route when there is no verified
	// caller: with authentication off, nobody can be shown to be a member or
	// an uploader.
	errFilesNeedAuth = &schema.Error{Code: "files_require_auth", Message: "files require authentication"}
)

// filesDir is the directory the bytes of uploaded files live in, or "" when
// the server has no data directory to put them in.
func (s *Server) filesDir() string {
	if s.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.DataDir, filesDirName)
}

// blobPath is where the bytes whose SHA-256 is sum live. sum is checked to be
// 64 lowercase hex characters, so it cannot contain a separator or a dot
// segment; it is the whole of what reaches the path.
func (s *Server) blobPath(sum string) (string, error) {
	if !schema.ValidSHA256Hex(sum) || s.filesDir() == "" {
		return "", errors.New("server: not a file hash")
	}
	return filepath.Join(s.filesDir(), sum[:2], sum), nil
}

// removeStaleUploads deletes temporary files an interrupted upload left in the
// files directory. It runs at start, before anything can be uploading.
func (s *Server) removeStaleUploads() {
	dir := s.filesDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Error("files: list the files directory failed", "error", err)
		}
		return
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), uploadTempPrefix) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil { // #nosec G122 -- a name read from our own directory
				slog.Error("files: remove stale upload failed", "error", err)
			}
		}
	}
}

func writeFileNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, errFileNotFound.Code, errFileNotFound.Message)
}

// fileToWire is the wire form of a stored file's metadata.
func fileToWire(f store.File) schema.FileV1 {
	return schema.FileV1{
		Schema: schema.FileSchemaV1, ID: f.ID, ChannelID: f.ChannelID, UploaderID: f.UploaderID,
		Filename: f.Filename, MediaType: f.MediaType, Size: f.Size, SHA256: f.SHA256,
		CreatedAt: schema.NewTimestamp(f.CreatedAt),
	}
}

// handleUploadFile serves POST /v2/channels/{channel}/files?filename=<name>:
// the request body is the file. Humans only (the route is wrapped in
// humansOnlyFiles).
//
// The order of the checks is part of the design. A caller who is not a member
// of the channel is told it does not exist before anything else is said, so
// the disabled and size answers are never a way to probe a channel. Everything
// that can be refused without reading the body is refused before it is read.
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeError(w, http.StatusBadRequest, errFilesNeedAuth.Code, errFilesNeedAuth.Message)
		return
	}
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesPost, schema.ChannelPermissionPost)
	if !ok {
		return
	}
	limit := s.cfg.MaxUploadBytes
	if limit <= 0 || s.filesDir() == "" {
		writeError(w, http.StatusForbidden, "uploads_disabled", "file uploads are disabled on this server")
		return
	}
	names := r.URL.Query()["filename"]
	if len(names) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_filename", "exactly one filename is required")
		return
	}
	if err := schema.ValidateFileName(names[0]); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filename", err.Error())
		return
	}
	tooLarge := func() {
		// Close the connection after answering. Otherwise the server would
		// wait for the rest of a body it has already refused, and a client
		// that promises more than the limit and never sends it would be
		// answered only when it gave up.
		w.Header().Set("Connection", "close")
		writeError(w, http.StatusRequestEntityTooLarge, "file_too_large",
			fmt.Sprintf("a file may be at most %d bytes", limit))
	}
	if r.ContentLength > limit {
		tooLarge()
		return
	}

	// The limit is enforced while reading, whatever the request claims about
	// its own length, and the bytes are hashed as they are written.
	sum, size, err := s.storeBlob(http.MaxBytesReader(w, r.Body, limit))
	var over *http.MaxBytesError
	switch {
	case errors.As(err, &over):
		tooLarge()
		return
	case errors.Is(err, errUploadRead):
		// The client went away or sent a broken body. Nothing was kept.
		writeError(w, http.StatusBadRequest, "upload_interrupted", "the upload was not received in full")
		return
	case err != nil:
		slog.ErrorContext(ctx, "files: store blob failed", "error", err)
		writeInternalError(w)
		return
	}

	// The row is recorded only after the bytes are in place, so a row never
	// points at nothing.
	stored, err := s.store.InsertFile(ctx, store.NewFile{
		ChannelID: channel.ID, UploaderID: caller.ID, Filename: names[0],
		MediaType: schema.NormalizeMediaType(r.Header.Get("Content-Type")), Size: size, SHA256: sum,
	})
	switch {
	case errors.Is(err, store.ErrNotChannelMember):
		// Removed from the channel while the body was arriving.
		writeChannelNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "files: record upload failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusCreated, fileToWire(stored))
}

// errUploadRead marks a failure to read the request body, as opposed to
// writing to our own disk.
var errUploadRead = errors.New("server: read upload")

// storeBlob copies body into the files directory and returns the SHA-256 and
// size of what it read. The bytes go to a temporary file in the files directory
// (the same filesystem as the destination, which is what makes the rename
// atomic; the destination directory is not known until the hash is), are synced
// and then renamed to <sha256[:2]>/<sha256>. A crash at any point leaves at
// worst a ".upload-" file, which the next start removes, and never a partial
// file under a hash name. Identical content is stored once.
//
// On any error nothing is left behind.
func (s *Server) storeBlob(body io.Reader) (sum string, size int64, err error) {
	root := s.filesDir()
	if err := os.MkdirAll(root, 0o750); err != nil { // #nosec G301,G703 -- under the operator's data directory
		return "", 0, fmt.Errorf("server: create files directory: %w", err)
	}
	tmp, err := os.CreateTemp(root, uploadTempPrefix+"*") // #nosec G304,G703 -- fixed prefix under the operator's data directory
	if err != nil {
		return "", 0, fmt.Errorf("server: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		// io.Copy does not say which side failed. A write error is ours, but
		// the limit and a dropped connection arrive here too, and the caller
		// must tell a MaxBytesError apart: keep it wrapped.
		if isWriteError(err) {
			return "", 0, fmt.Errorf("server: write upload: %w", err)
		}
		return "", 0, fmt.Errorf("%w: %w", errUploadRead, err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("server: sync upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("server: close upload: %w", err)
	}
	sum = hex.EncodeToString(h.Sum(nil))
	dest, err := s.blobPath(sum)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil { // #nosec G301,G703 -- derived from the hash
		return "", 0, fmt.Errorf("server: create hash directory: %w", err)
	}
	if st, serr := os.Lstat(dest); serr == nil && st.Mode().IsRegular() && st.Size() == size {
		// Already stored. The temporary file is removed by the defer.
		return sum, size, nil
	}
	if err := s.renameFile(tmpName, dest); err != nil {
		return "", 0, fmt.Errorf("server: place upload: %w", err)
	}
	renamed = true
	syncDir(filepath.Dir(dest))
	return sum, size, nil
}

// isWriteError reports whether err came from writing the temporary file
// rather than from reading the request body: the os package wraps those in
// *fs.PathError (or a syscall error).
func isWriteError(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe)
}

// syncDir makes a rename durable. A failure is not an upload failure: the file
// is in place, and only its survival of a power cut right now is in doubt.
func syncDir(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- derived from the hash
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// fileForRequest resolves {id} for a fetch. It writes the one not-found
// answer and returns false for everything that is not a visible file: a bad
// id, an unknown id, a file in a channel the caller is not in, a file attached
// to a message the caller cannot see. Only the store's answer decides.
func (s *Server) fileForRequest(w http.ResponseWriter, r *http.Request) (store.File, bool) {
	ctx := r.Context()
	if _, ok := callerFrom(ctx); !ok {
		writeError(w, http.StatusBadRequest, errFilesNeedAuth.Code, errFilesNeedAuth.Message)
		return store.File{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeFileNotFound(w)
		return store.File{}, false
	}
	f, err := s.store.FileVisibleTo(ctx, id, readerFor(r, apiV2))
	if errors.Is(err, store.ErrNotFound) {
		writeFileNotFound(w)
		return store.File{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "files: read file failed", "error", err)
		writeInternalError(w)
		return store.File{}, false
	}
	return f, true
}

// handleGetFileMeta serves GET /v2/files/{id}/meta.
func (s *Server) handleGetFileMeta(w http.ResponseWriter, r *http.Request) {
	f, ok := s.fileForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, fileToWire(f))
}

// handleGetFile serves GET /v2/files/{id}: the bytes, never rendered. The
// stored media type is information in the metadata and is not used here; the
// stored name is a parameter of Content-Disposition, encoded by the mime
// package, and nothing else.
func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	f, ok := s.fileForRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	path, err := s.blobPath(f.SHA256)
	if err != nil {
		slog.ErrorContext(ctx, "files: stored hash is unusable", "file", f.ID)
		writeInternalError(w)
		return
	}
	blob, err := os.Open(path) // #nosec G304 -- derived from the stored hash alone
	if err != nil {
		slog.ErrorContext(ctx, "files: open stored file failed", "file", f.ID, "error", err)
		writeInternalError(w)
		return
	}
	defer func() { _ = blob.Close() }()
	if st, err := blob.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() != f.Size {
		slog.ErrorContext(ctx, "files: stored file does not match its record", "file", f.ID)
		writeInternalError(w)
		return
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename})
	if disposition == "" {
		disposition = "attachment"
	}
	h := w.Header()
	h.Set("Content-Type", schema.FileDownloadMediaType)
	h.Set("Content-Disposition", disposition)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.CopyN(w, blob, f.Size); err != nil {
		slog.WarnContext(ctx, "files: download interrupted", "file", f.ID, "error", err)
	}
}
