package schema

import (
	"errors"
	"fmt"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Files (issue #23). A file is uploaded to a channel and attached to at most
// one message; it is fetchable by its uploader and by whoever the message it
// is attached to is visible to. The bytes are not part of any wire shape here:
// they are the body of the upload and the download. These are the metadata
// shape and the payload a message uses to attach files.

const (
	// FileSchemaV1 is the schema name of FileV1.
	FileSchemaV1 = "conch.file.v1"
	// FileAttachmentV1Name is the registered payload name a message uses to
	// attach files. Attaching is a payload, not an envelope field, so
	// MessageV2 is unchanged.
	FileAttachmentV1Name = "conch.file_attachment.v1"

	// MaxFileNameBytes is the longest file name, in bytes.
	MaxFileNameBytes = 255
	// MaxFileAttachments is the most files one message may attach.
	MaxFileAttachments = 10
	// MaxMediaTypeBytes bounds a stored media type.
	MaxMediaTypeBytes = 255
	// FileDownloadMediaType is the only Content-Type a download is ever
	// served with, whatever media type the uploader declared: a stored file
	// is never rendered.
	FileDownloadMediaType = "application/octet-stream"
)

// FileV1 is the metadata of an uploaded file.
//
// Filename and MediaType are what the uploader said (the media type reduced to
// its type/subtype). Neither is trusted: the filename is never part of a path
// on the server and the media type is never a response header.
type FileV1 struct {
	Schema     string `json:"schema"`
	ID         int64  `json:"id"`
	ChannelID  int64  `json:"channel_id"`
	UploaderID int64  `json:"uploader_id"`
	// Filename is the uploader's name for the file, valid under ValidateFileName.
	Filename string `json:"filename"`
	// MediaType is the declared media type, "type/subtype" in lower case with
	// no parameters; application/octet-stream when none was declared.
	MediaType string `json:"media_type"`
	// Size is the number of bytes.
	Size int64 `json:"size"`
	// SHA256 is the lowercase hex SHA-256 of the bytes.
	SHA256    string    `json:"sha256"`
	CreatedAt Timestamp `json:"created_at"`
}

// Validate reports whether the metadata is well-formed.
func (f FileV1) Validate() error {
	switch {
	case f.Schema != FileSchemaV1:
		return fmt.Errorf("schema: file schema must be %q, got %q", FileSchemaV1, f.Schema)
	case f.ID <= 0:
		return errors.New("schema: file id must be positive")
	case f.ChannelID <= 0:
		return errors.New("schema: file channel_id must be positive")
	case f.UploaderID <= 0:
		return errors.New("schema: file uploader_id must be positive")
	case f.Size < 0:
		return errors.New("schema: file size must not be negative")
	case !ValidSHA256Hex(f.SHA256):
		return errors.New("schema: file sha256 must be 64 lowercase hex characters")
	case f.CreatedAt.IsZero():
		return errors.New("schema: file created_at is required")
	}
	if err := ValidateFileName(f.Filename); err != nil {
		return err
	}
	return ValidateMediaType(f.MediaType)
}

// ValidSHA256Hex reports whether s is 64 lowercase hexadecimal characters.
func ValidSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidateFileName reports whether name is acceptable as the name of an
// uploaded file: 1 to MaxFileNameBytes bytes of valid UTF-8, not "." or "..",
// and no "/", "\", NUL or other control character, line or paragraph
// separator or bidi control. The name is shown to people and written to disk
// by clients, so a name that could climb out of a directory or rewrite what a
// terminal shows is refused at the door. Errors name the rule, never the input.
func ValidateFileName(name string) error {
	switch {
	case name == "":
		return errors.New("schema: file name is required")
	case len(name) > MaxFileNameBytes:
		return fmt.Errorf("schema: file name must be at most %d bytes", MaxFileNameBytes)
	case !utf8.ValidString(name):
		return errors.New("schema: file name must be valid UTF-8")
	case name == "." || name == "..":
		return errors.New("schema: file name must not be a dot or double-dot")
	}
	for _, c := range name {
		if c == '/' || c == '\\' || unicode.IsControl(c) ||
			unicode.Is(unicode.Zl, c) || unicode.Is(unicode.Zp, c) || isStrongBidiControl(c) {
			return errors.New("schema: file name must not contain a path separator, control character or bidi control")
		}
	}
	return nil
}

// NormalizeMediaType reduces a Content-Type header value to the "type/subtype"
// the server records, in lower case without parameters. A missing or
// unparseable value is application/octet-stream.
func NormalizeMediaType(header string) string {
	mt, _, err := mime.ParseMediaType(header)
	if err != nil || ValidateMediaType(mt) != nil {
		return FileDownloadMediaType
	}
	return mt
}

// ValidateMediaType reports whether mt is a bare "type/subtype".
func ValidateMediaType(mt string) error {
	if mt == "" || len(mt) > MaxMediaTypeBytes {
		return fmt.Errorf("schema: file media_type must be 1 to %d bytes", MaxMediaTypeBytes)
	}
	parsed, params, err := mime.ParseMediaType(mt)
	if err != nil || parsed != mt || len(params) != 0 || !strings.Contains(mt, "/") {
		return errors.New("schema: file media_type must be a bare type/subtype in lower case")
	}
	return nil
}

// FileAttachmentV1 is the data of the payload "conch.file_attachment.v1": the
// files a message attaches, by id. It carries ids only; a client reads the
// rest from the metadata route.
type FileAttachmentV1 struct {
	FileIDs []int64 `json:"file_ids"`
}

// Validate reports whether the attachment is well-formed: one to
// MaxFileAttachments positive, distinct ids.
func (a FileAttachmentV1) Validate() error {
	if len(a.FileIDs) == 0 || len(a.FileIDs) > MaxFileAttachments {
		return fmt.Errorf("schema: a file attachment names 1 to %d files", MaxFileAttachments)
	}
	seen := make(map[int64]struct{}, len(a.FileIDs))
	for _, id := range a.FileIDs {
		if id <= 0 {
			return errors.New("schema: file_ids must be positive")
		}
		if _, dup := seen[id]; dup {
			return errors.New("schema: file_ids must be distinct")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func init() {
	RegisterPayload(PayloadSchema{
		Name: FileAttachmentV1Name,
		New:  func() any { return new(FileAttachmentV1) },
	})
}
