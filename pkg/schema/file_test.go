package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFileGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"file-v1.json", func() any { return &FileV1{} }},
		{"file-attachment-v1.json", func() any { return &FileAttachmentV1{} }},
		{"post-message-request-v2-file-attachment.json", func() any { return &PostMessageRequestV2{} }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) { assertGoldenFixture(t, tt.file, tt.new) })
	}
}

func TestFileAttachmentPayloadIsRegistered(t *testing.T) {
	p := Payload{Schema: FileAttachmentV1Name, Data: json.RawMessage(`{"file_ids":[1,2]}`)}
	v, known, err := p.Decode()
	if err != nil || !known {
		t.Fatalf("Decode known=%v err=%v", known, err)
	}
	if got := v.(*FileAttachmentV1).FileIDs; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("decoded ids = %v", got)
	}
	// An unexpected field fails loudly rather than being dropped.
	bad := Payload{Schema: FileAttachmentV1Name, Data: json.RawMessage(`{"file_ids":[1],"extra":true}`)}
	if _, _, err := bad.Decode(); err == nil {
		t.Error("Decode accepted an unknown field")
	}
}

func TestFileV1Validate(t *testing.T) {
	valid := func() FileV1 {
		return FileV1{
			Schema: FileSchemaV1, ID: 7, ChannelID: 2, UploaderID: 3, Filename: "plan.pdf",
			MediaType: "application/pdf", Size: 48213, SHA256: strings.Repeat("ab", 32),
			CreatedAt: NewTimestamp(time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)),
		}
	}
	tests := []struct {
		name    string
		mutate  func(*FileV1)
		wantErr bool
	}{
		{"valid", func(*FileV1) {}, false},
		{"empty file", func(f *FileV1) { f.Size = 0 }, false},
		{"wrong schema", func(f *FileV1) { f.Schema = "conch.file.v2" }, true},
		{"zero id", func(f *FileV1) { f.ID = 0 }, true},
		{"zero channel", func(f *FileV1) { f.ChannelID = 0 }, true},
		{"zero uploader", func(f *FileV1) { f.UploaderID = 0 }, true},
		{"negative size", func(f *FileV1) { f.Size = -1 }, true},
		{"short hash", func(f *FileV1) { f.SHA256 = "ab" }, true},
		{"upper-case hash", func(f *FileV1) { f.SHA256 = strings.Repeat("AB", 32) }, true},
		{"non-hex hash", func(f *FileV1) { f.SHA256 = strings.Repeat("zz", 32) }, true},
		{"zero time", func(f *FileV1) { f.CreatedAt = Timestamp{} }, true},
		{"bad name", func(f *FileV1) { f.Filename = "../x" }, true},
		{"media type with parameters", func(f *FileV1) { f.MediaType = "text/plain; charset=utf-8" }, true},
		{"media type not a type", func(f *FileV1) { f.MediaType = "plain" }, true},
		{"empty media type", func(f *FileV1) { f.MediaType = "" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := valid()
			tt.mutate(&f)
			if err := f.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateFileName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain", "plan.pdf", false},
		{"dot file", ".bashrc", false},
		{"unicode", "山田 太郎.txt", false},
		{"inner dots", "a..b", false},
		{"255 bytes", strings.Repeat("a", 255), false},
		{"256 bytes", strings.Repeat("a", 256), true},
		{"empty", "", true},
		{"dot", ".", true},
		{"dotdot", "..", true},
		{"parent", "../x", true},
		{"absolute", "/etc/passwd", true},
		{"backslash", `a\b`, true},
		{"newline", "a\nb", true},
		{"carriage return", "a\rb", true},
		{"NUL", "a\x00b", true},
		{"escape", "a\x1b[31m", true},
		{"DEL", "a\x7fb", true},
		{"C1 control", "a\u0085b", true},
		{"line separator", "a b", true},
		{"bidi override", "a\u202eb", true},
		{"invalid UTF-8", "a\xffb", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateFileName(tt.in); (err != nil) != tt.wantErr {
				t.Errorf("ValidateFileName(%q) = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeMediaType(t *testing.T) {
	tests := []struct{ in, want string }{
		{"application/pdf", "application/pdf"},
		{"Text/HTML; charset=utf-8", "text/html"},
		{"image/svg+xml", "image/svg+xml"},
		{"", FileDownloadMediaType},
		{"nonsense", FileDownloadMediaType},
		{"a/b; =", FileDownloadMediaType},
		{strings.Repeat("a", 300) + "/b", FileDownloadMediaType},
	}
	for _, tt := range tests {
		if got := NormalizeMediaType(tt.in); got != tt.want {
			t.Errorf("NormalizeMediaType(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFileAttachmentV1Validate(t *testing.T) {
	ids := func(n int) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(i + 1)
		}
		return out
	}
	tests := []struct {
		name    string
		in      FileAttachmentV1
		wantErr bool
	}{
		{"one", FileAttachmentV1{FileIDs: []int64{1}}, false},
		{"ten", FileAttachmentV1{FileIDs: ids(10)}, false},
		{"eleven", FileAttachmentV1{FileIDs: ids(11)}, true},
		{"none", FileAttachmentV1{}, true},
		{"zero id", FileAttachmentV1{FileIDs: []int64{0}}, true},
		{"negative id", FileAttachmentV1{FileIDs: []int64{-1}}, true},
		{"duplicate", FileAttachmentV1{FileIDs: []int64{3, 3}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.in.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
