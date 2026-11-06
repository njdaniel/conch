package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// cameraDefaultMigration is the schema version that keeps cameras off on a
// server that existed before use_camera became a default (issue #1361).
const cameraDefaultMigration = 44

// pinCameraOff runs in migration 44. use_camera's default changed from off
// to on for members and operators (Nick, 2026-11-05; ADR-015 amended), and a
// default is only the value of a cell with no stored row. Without this step,
// every existing server would offer cameras the moment it upgraded, which
// ADR-014's rule for permissions forbids: an upgrade turns nothing on.
//
// So on a database with history (the test initializePermissions uses), each
// use_camera cell with no stored row is stored as off. A cell an owner
// already set keeps its value. A new database stores nothing, so the new
// default applies. What anyone holds is the same before and after, so the
// revision does not move and no audit row is written.
func pinCameraOff(ctx context.Context, tx *sql.Tx) error {
	var history bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM principals) OR EXISTS (SELECT 1 FROM audit_events)
		     OR EXISTS (SELECT 1 FROM server_settings)`).Scan(&history); err != nil {
		return fmt.Errorf("read whether the database is new: %w", err)
	}
	if !history {
		return nil
	}
	now := credentialNow().Truncate(time.Millisecond).UnixMilli()
	for _, role := range []schema.PermissionRole{schema.PermissionRoleMember, schema.PermissionRoleOperator} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO role_permissions (role, permission, granted, updated_at) VALUES (?, ?, 0, ?)
			 ON CONFLICT (role, permission) DO NOTHING`,
			string(role), string(schema.PermUseCamera), now); err != nil {
			return fmt.Errorf("pin %s.%s off: %w", role, schema.PermUseCamera, err)
		}
	}
	return nil
}
