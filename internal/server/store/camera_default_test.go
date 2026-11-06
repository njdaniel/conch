package store

import (
	"context"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestCameraDefaultMigration: use_camera is on by default from issue #1361,
// but an upgrade turns nothing on (ADR-014). Migration 44 stores the camera
// off on a database with history, keeps a cell an owner already set, and
// writes nothing on a new database.
func TestCameraDefaultMigration(t *testing.T) {
	people := `INSERT INTO principals (id, kind, name, name_key, name_skel, role, owner, voice_allowed, may_invite, created_at) VALUES
		(1, 'human', 'owner', 'owner', 'owner', 'operator', 1, 1, 0, 1),
		(2, 'human', 'operator-1', 'operator-1', 'operator-1', 'operator', 0, 1, 1, 2),
		(3, 'human', 'member-1', 'member-1', 'member-1', 'member', 0, 1, 1, 3)`
	cameraOn := `INSERT INTO role_permissions (role, permission, granted, updated_at) VALUES
		('member', 'use_camera', 1, 4), ('operator', 'use_camera', 1, 4)`
	for _, tt := range []struct {
		name             string
		seed             []string
		member, operator bool // hold use_camera after the upgrade
	}{
		{"an existing server with no camera cell keeps cameras off", []string{people}, false, false},
		{"an existing server whose owner turned cameras on keeps them on", []string{people, cameraOn}, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, path := migrateTo(t, cameraDefaultMigration-1, tt.seed)
			check := func(pass int) {
				t.Helper()
				ctx := context.Background()
				var version int
				if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
					t.Errorf("pass %d: user_version = %d, %v; want %d", pass, version, err, len(migrations))
				}
				for _, c := range []struct {
					id   int64
					want bool
				}{{1, true}, {2, tt.operator}, {3, tt.member}} {
					if got, err := s.Permitted(ctx, c.id, schema.PermUseCamera); err != nil || got != c.want {
						t.Errorf("pass %d: principal %d holds use_camera = %v, %v; want %v", pass, c.id, got, err, c.want)
					}
				}
				// What anyone holds did not change, so neither did the
				// revision, and nothing was audited for it.
				doc, err := s.Permissions(ctx)
				if err != nil || doc.Revision != 1 {
					t.Errorf("pass %d: document revision = %d, %v; want 1", pass, doc.Revision, err)
				}
				if n := countRows(t, s, "role_permissions"); n != 2 {
					t.Errorf("pass %d: %d role_permissions rows, want the two camera cells", pass, n)
				}
			}
			check(1)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			if s, err = Open(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			check(2)
		})
	}

	// A new server stores nothing, so the new default applies: members and
	// operators hold the camera, and the document is the group preset.
	t.Run("a new server has cameras on", func(t *testing.T) {
		s := openFreshTestStore(t)
		if n := countRows(t, s, "role_permissions"); n != 0 {
			t.Errorf("a new database has %d role_permissions rows, want none", n)
		}
		doc, err := s.Permissions(context.Background())
		if err != nil || doc.Preset != schema.PresetGroup {
			t.Fatalf("document = %+v, %v; want the group preset", doc, err)
		}
		for _, row := range doc.Rows {
			if row.Permission == schema.PermUseCamera && (!row.Member || !row.Operator) {
				t.Errorf("use_camera row = %+v, want on for members and operators", row)
			}
		}
	})
}
