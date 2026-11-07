package store

import "context"

// IsCommunityMember reports whether principalID is a member of the community
// in the sense of isMemberTx (design §1.1): a human, enabled, not hidden, with
// no guest visit active or ended. An unknown id is not a member. It is a
// read, used by preview to decide who may be read as (issue #1515); it grants
// nothing.
func (s *Store) IsCommunityMember(ctx context.Context, principalID int64) (bool, error) {
	return isMemberTx(ctx, s.db, principalID)
}
