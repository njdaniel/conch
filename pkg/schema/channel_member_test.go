package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChannelMemberGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"channel-member-v1.json", func() any { return new(ChannelMemberV1) }},
		{"channel-member-v1-no-adder.json", func() any { return new(ChannelMemberV1) }},
		{"list-channel-members-response-v1.json", func() any { return new(ListChannelMembersResponseV1) }},
		{"list-channel-members-response-v1-empty.json", func() any { return new(ListChannelMembersResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestListChannelMembersEmptyIsArray(t *testing.T) {
	raw, err := json.Marshal(ListChannelMembersResponseV1{Members: []ChannelMemberV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"members":[]`) {
		t.Errorf("empty list = %s, want members:[]", raw)
	}
}
