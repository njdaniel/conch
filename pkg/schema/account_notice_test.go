package schema

import "testing"

func TestAccountNoticeFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"account-notice-v1.json", func() any { return new(AccountNoticeV1) }},
		{"list-account-notices-response-v1.json", func() any { return new(ListAccountNoticesResponseV1) }},
		{"list-account-notices-response-v1-empty.json", func() any { return new(ListAccountNoticesResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) { assertGoldenFixture(t, tt.file, tt.new) })
	}
}

func TestAccountNoticeValidate(t *testing.T) {
	ok := AccountNoticeV1{ID: 1, Kind: AccountNoticeKindPasswordSetByLink, ByName: "admin"}
	tests := []struct {
		name    string
		mutate  func(*AccountNoticeV1)
		wantErr bool
	}{
		{"valid", func(*AccountNoticeV1) {}, false},
		{"zero id", func(n *AccountNoticeV1) { n.ID = 0 }, true},
		{"unknown kind", func(n *AccountNoticeV1) { n.Kind = "other" }, true},
		{"empty kind", func(n *AccountNoticeV1) { n.Kind = "" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := ok
			tt.mutate(&n)
			if err := n.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
