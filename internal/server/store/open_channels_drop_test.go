package store

import (
	"testing"
)

// openChannelsDropMigration is the migration that drops open_channels
// (issue #998, design I13).
const openChannelsDropMigration = 46

// TestOpenChannelsDropMigration: a database at I2's version (40) with rows in
// open_channels upgrades to the current version with the table gone and
// channels, default marks and settings exactly as they were.
func TestOpenChannelsDropMigration(t *testing.T) {
	if len(migrations) < openChannelsDropMigration {
		t.Fatalf("%d migrations, the open_channels drop is %d", len(migrations), openChannelsDropMigration)
	}
	seed := []string{
		`INSERT INTO principals (kind, name, name_key, name_skel, created_at) VALUES ('human', 'owner', 'owner', 'owner', 0)`,
		`INSERT INTO channels (name, name_key, name_skel, created_at, visibility, is_default, default_listed_by)
			VALUES ('general', 'general', 'general', 0, 'public', 1, 1), ('belgium', 'belgium', 'belgium', 0, 'private', 0, NULL)`,
		`INSERT INTO open_channels (channel_id, added_by, added_at) VALUES (1, 1, 0)`,
		`INSERT INTO server_settings (key, value, updated_at) VALUES ('open_joining', 'account', 0)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
	}
	s, _ := migrateTo(t, 40, seed)
	t.Cleanup(func() { _ = s.Close() })

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"open_channels table is gone", `SELECT COUNT(*) FROM sqlite_master WHERE name = 'open_channels'`, 0},
		{"two channels remain", `SELECT COUNT(*) FROM channels`, 2},
		{"general is still the one default", `SELECT COUNT(*) FROM channels WHERE name = 'general' AND is_default = 1 AND visibility = 'public' AND default_listed_by = 1`, 1},
		{"belgium is still private and not default", `SELECT COUNT(*) FROM channels WHERE name = 'belgium' AND is_default = 0 AND visibility = 'private'`, 1},
		{"the joining setting is unchanged", `SELECT COUNT(*) FROM server_settings WHERE key = 'open_joining' AND value = 'account'`, 1},
		{"the owner is unchanged", `SELECT COUNT(*) FROM principals WHERE name = 'owner'`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n int
			if err := s.db.QueryRow(tt.query).Scan(&n); err != nil {
				t.Fatalf("%s: %v", tt.query, err)
			}
			if n != tt.want {
				t.Errorf("%s = %d, want %d", tt.query, n, tt.want)
			}
		})
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
}
