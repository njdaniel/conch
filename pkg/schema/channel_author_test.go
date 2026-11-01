package schema

import (
	"encoding/json"
	"testing"
)

func TestChannelAuthorGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"channel-author-v1.json", func() any { return new(ChannelAuthorV1) }},
		{"channel-authors-response-v1.json", func() any { return new(ChannelAuthorsResponseV1) }},
		{"channel-authors-response-v1-empty.json", func() any { return new(ChannelAuthorsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// An author is a name and a kind and nothing more (community-model §13.5).
func TestChannelAuthorV1Keys(t *testing.T) {
	raw, err := json.Marshal(ChannelAuthorV1{PrincipalID: 4, Name: "bob", Kind: PrincipalHuman})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"principal_id":4,"name":"bob","kind":"human"}`; string(raw) != want {
		t.Errorf("author = %s, want %s", raw, want)
	}
	if MaxChannelAuthorIDs != 100 {
		t.Errorf("MaxChannelAuthorIDs = %d, want 100", MaxChannelAuthorIDs)
	}
}

func TestChannelAuthorsResponseV1EmptyEncodesArray(t *testing.T) {
	got, err := json.Marshal(ChannelAuthorsResponseV1{Authors: []ChannelAuthorV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"authors":[]}` {
		t.Fatalf("got %s", got)
	}
}
