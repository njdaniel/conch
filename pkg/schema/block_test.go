package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBlockGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"block-v1.json", func() any { return new(BlockV1) }},
		{"list-blocks-response-v1.json", func() any { return new(ListBlocksResponseV1) }},
		{"list-blocks-response-v1-empty.json", func() any { return new(ListBlocksResponseV1) }},
		{"blocks-changed-v1.json", func() any { return new(BlocksChangedV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// Every fixture is a document the schema accepts.
func TestBlockFixturesValidate(t *testing.T) {
	var b BlockV1
	decodeFixture(t, "block-v1.json", &b)
	if err := b.Validate(); err != nil {
		t.Errorf("block-v1.json: Validate = %v", err)
	}
	for _, file := range []string{"list-blocks-response-v1.json", "list-blocks-response-v1-empty.json"} {
		var r ListBlocksResponseV1
		decodeFixture(t, file, &r)
		if err := r.Validate(); err != nil {
			t.Errorf("%s: Validate = %v", file, err)
		}
		if r.Limit != MaxBlocks {
			t.Errorf("%s: limit = %d, want MaxBlocks %d", file, r.Limit, MaxBlocks)
		}
	}
	var c BlocksChangedV1
	decodeFixture(t, "blocks-changed-v1.json", &c)
	if err := c.Validate(); err != nil {
		t.Errorf("blocks-changed-v1.json: Validate = %v", err)
	}
}

func TestBlockV1Validate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	ok := BlockV1{PrincipalID: 3, Name: "wren", Kind: PrincipalHuman, CreatedAt: at}
	tests := []struct {
		name    string
		mutate  func(*BlockV1)
		wantErr string
	}{
		{"valid", func(*BlockV1) {}, ""},
		{"an agent", func(b *BlockV1) { b.Kind = PrincipalAgent }, ""},
		{"no id", func(b *BlockV1) { b.PrincipalID = 0 }, "principal_id must be positive"},
		{"negative id", func(b *BlockV1) { b.PrincipalID = -1 }, "principal_id must be positive"},
		{"no name", func(b *BlockV1) { b.Name = "" }, "name is required"},
		{"unknown kind", func(b *BlockV1) { b.Kind = "robot" }, "not human or agent"},
		{"no time", func(b *BlockV1) { b.CreatedAt = Timestamp{} }, "created_at is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := ok
			tt.mutate(&b)
			err := b.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestListBlocksResponseV1Validate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	one := BlockV1{PrincipalID: 3, Name: "wren", Kind: PrincipalHuman, CreatedAt: at}
	two := BlockV1{PrincipalID: 4, Name: "kit", Kind: PrincipalAgent, CreatedAt: at}
	tests := []struct {
		name    string
		resp    ListBlocksResponseV1
		wantErr string
	}{
		{"empty", ListBlocksResponseV1{Blocks: []BlockV1{}, Limit: MaxBlocks}, ""},
		{"two", ListBlocksResponseV1{Blocks: []BlockV1{one, two}, Limit: MaxBlocks}, ""},
		{"null list", ListBlocksResponseV1{Limit: MaxBlocks}, "must be a list"},
		{"no limit", ListBlocksResponseV1{Blocks: []BlockV1{}}, "limit must be positive"},
		{"over the limit", ListBlocksResponseV1{Blocks: []BlockV1{one, two}, Limit: 1}, "over its limit"},
		{"twice", ListBlocksResponseV1{Blocks: []BlockV1{one, one}, Limit: MaxBlocks}, "listed twice"},
		{"a bad entry", ListBlocksResponseV1{Blocks: []BlockV1{{PrincipalID: 3}}, Limit: MaxBlocks}, "blocks[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.resp.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// The signal carries its schema and nothing else: it names nobody.
func TestBlocksChangedV1(t *testing.T) {
	if BlocksChangedSchemaV1 != "conch.blocks_changed.v1" {
		t.Errorf("BlocksChangedSchemaV1 = %q", BlocksChangedSchemaV1)
	}
	tests := []struct {
		name    string
		doc     BlocksChangedV1
		wantErr string
	}{
		{"valid", BlocksChangedV1{Schema: BlocksChangedSchemaV1}, ""},
		{"empty", BlocksChangedV1{}, `got ""`},
		{"the directory signal", BlocksChangedV1{Schema: DirectoryChangedSchemaV1}, `got "conch.directory_changed.v1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.doc.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want %q", err, tt.wantErr)
			}
		})
	}
	raw, err := json.Marshal(BlocksChangedV1{Schema: BlocksChangedSchemaV1})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"schema":"conch.blocks_changed.v1"}` {
		t.Errorf("wire = %s", raw)
	}
}

// Approval requests and admin actions are decisions: never hidden by a block.
// Every other payload, and none, follows the block.
func TestBlockExemptPayload(t *testing.T) {
	tests := []struct {
		schema string
		want   bool
	}{
		{ApprovalV1Name, true},
		{AdminActionV1Name, true},
		{"", false},
		{"conch.approval.v2", false},
		{"conch.chart.v1", false},
	}
	for _, tt := range tests {
		if got := BlockExemptPayload(tt.schema); got != tt.want {
			t.Errorf("BlockExemptPayload(%q) = %v, want %v", tt.schema, got, tt.want)
		}
	}
}
