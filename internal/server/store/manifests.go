package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Errors returned by the manifest store, in addition to ErrNotFound (no
// manifest stored for the principal).
var (
	// ErrPrincipalNotFound is returned when the manifest's principal does not exist.
	ErrPrincipalNotFound = errors.New("store: principal not found")
	// ErrPrincipalNotAgent is returned when the manifest's principal is not an agent.
	ErrPrincipalNotAgent = errors.New("store: principal is not an agent")
	// ErrInvalidManifest is returned when a stored manifest row cannot be
	// decoded or fails schema validation. The row is never returned as a
	// usable manifest (fail closed).
	ErrInvalidManifest = errors.New("store: stored manifest is invalid")
	// ErrAdminChannelNotGranted is a manifest whose admin approval channel is
	// not among its channel grants with read (issue #1326, ai-administrators
	// §3.1): the agent could not watch its own approvals there.
	ErrAdminChannelNotGranted = errors.New("store: the approval channel must be one of the manifest's channels, granted read")
)

// The audit actions of an AI administrator's grant (issue #1326,
// ai-administrators §3.7). admin_grant_changed is written on every change of
// the grant or its admin settings; ai_admin_created when an agent that held
// no grant gains one, ai_admin_revoked when one that held a grant loses it.
const (
	AuditAdminGrantChanged = "admin_grant_changed"
	AuditAIAdminCreated    = "ai_admin_created"
	AuditAIAdminRevoked    = "ai_admin_revoked"
)

// ChannelNotFoundError reports a channel grant naming a channel that does not exist.
type ChannelNotFoundError struct{ ChannelID int64 }

func (e *ChannelNotFoundError) Error() string {
	return fmt.Sprintf("store: channel %d not found", e.ChannelID)
}

// PutAgentManifest creates or fully replaces the manifest of an agent
// principal and appends a manifest_created or manifest_replaced audit event in
// the same transaction. Lists are stored exactly as given. On replacement
// created_at is preserved and updated_at strictly advances. It reports whether
// the manifest was newly created.
//
// It returns ErrPrincipalNotFound, ErrPrincipalNotAgent, *ChannelNotFoundError,
// or the request's validation error without writing anything.
func (s *Store) PutAgentManifest(ctx context.Context, actor string, principalID int64, req schema.PutAgentManifestRequestV1) (schema.AgentManifestV1, bool, error) {
	if principalID <= 0 {
		return schema.AgentManifestV1{}, false, fmt.Errorf("store: put manifest: principal id must be positive, got %d", principalID)
	}
	if err := req.Validate(); err != nil {
		return schema.AgentManifestV1{}, false, err
	}
	capsJSON, err := marshalList(req.Capabilities)
	if err != nil {
		return schema.AgentManifestV1{}, false, fmt.Errorf("store: put manifest: %w", err)
	}
	channelsJSON, err := marshalList(req.Channels)
	if err != nil {
		return schema.AgentManifestV1{}, false, fmt.Errorf("store: put manifest: %w", err)
	}
	limitsJSON, err := marshalList(req.RateLimits)
	if err != nil {
		return schema.AgentManifestV1{}, false, fmt.Errorf("store: put manifest: %w", err)
	}
	permsJSON, err := marshalList(req.Permissions)
	if err != nil {
		return schema.AgentManifestV1{}, false, fmt.Errorf("store: put manifest: %w", err)
	}
	adminJSON := ""
	if req.Admin != nil {
		b, err := json.Marshal(req.Admin)
		if err != nil {
			return schema.AgentManifestV1{}, false, fmt.Errorf("store: put manifest: %w", err)
		}
		adminJSON = string(b)
	}
	if req.Admin != nil && !grantsRead(req.Channels, req.Admin.ApprovalChannelID) {
		return schema.AgentManifestV1{}, false, ErrAdminChannelNotGranted
	}

	var out schema.AgentManifestV1
	var created bool
	err = s.withImmediateTx(ctx, func(tx execer) error {
		var kind string
		switch err := tx.QueryRowContext(ctx, "SELECT kind FROM principals WHERE id = ?", principalID).Scan(&kind); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrPrincipalNotFound
		case err != nil:
			return fmt.Errorf("store: put manifest: find principal: %w", err)
		}
		if PrincipalKind(kind) != PrincipalAgent {
			return ErrPrincipalNotAgent
		}
		// Granting permissions is an owner's act (issue #1326,
		// ai-administrators §3.1; ADR-010 D4 as amended): the actor is
		// re-read here, under the write lock. Emptying a grant is any
		// manage_members holder's, which the route already decided. The host
		// (system, offline) may grant: conchd ai-admin create runs there.
		if len(req.Permissions) > 0 && actor != "system" {
			ok, err := actorIsOwnerTx(ctx, tx, actor)
			if err != nil {
				return err
			}
			if !ok {
				return ErrNotOwner
			}
		}
		for _, g := range req.Channels {
			var one int
			switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM channels WHERE id = ?", g.ChannelID).Scan(&one); {
			case errors.Is(err, sql.ErrNoRows):
				return &ChannelNotFoundError{ChannelID: g.ChannelID}
			case err != nil:
				return fmt.Errorf("store: put manifest: find channel %d: %w", g.ChannelID, err)
			}
		}

		now := time.Now().Truncate(time.Millisecond)
		createdAt, updatedAt := now, now
		var prevCreated, prevUpdated int64
		prevPerms, prevAdmin := "[]", ""
		switch err := tx.QueryRowContext(ctx,
			"SELECT created_at, updated_at, permissions, admin FROM agent_manifests WHERE principal_id = ?", principalID,
		).Scan(&prevCreated, &prevUpdated, &prevPerms, &prevAdmin); {
		case errors.Is(err, sql.ErrNoRows):
			created = true
		case err != nil:
			return fmt.Errorf("store: put manifest: read existing: %w", err)
		default:
			createdAt = time.UnixMilli(prevCreated)
			if !updatedAt.After(time.UnixMilli(prevUpdated)) {
				updatedAt = time.UnixMilli(prevUpdated + 1)
			}
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agent_manifests
			 (principal_id, display_name, tier, capabilities, channels, rate_limits, permissions, admin, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (principal_id) DO UPDATE SET
			   display_name = excluded.display_name, tier = excluded.tier,
			   capabilities = excluded.capabilities, channels = excluded.channels,
			   rate_limits = excluded.rate_limits, permissions = excluded.permissions,
			   admin = excluded.admin, updated_at = excluded.updated_at`,
			principalID, req.DisplayName, string(req.Tier), capsJSON, channelsJSON, limitsJSON, permsJSON, adminJSON,
			createdAt.UnixMilli(), updatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("store: put manifest: %w", err)
		}
		action := "manifest_replaced"
		if created {
			action = "manifest_created"
		}
		if err := appendAuditEventTx(ctx, tx, actor, action, principalActor(principalID), "", now); err != nil {
			return err
		}
		if prevPerms != permsJSON || prevAdmin != adminJSON {
			var channel int64
			if req.Admin != nil {
				channel = req.Admin.ApprovalChannelID
			}
			detail := fmt.Sprintf("permissions=%s->%s approval_channel=%d", prevPerms, permsJSON, channel)
			if err := appendAuditEventTx(ctx, tx, actor, AuditAdminGrantChanged, principalActor(principalID), detail, now); err != nil {
				return err
			}
			switch had, has := prevPerms != "[]", permsJSON != "[]"; {
			case !had && has:
				err = appendAuditEventTx(ctx, tx, actor, AuditAIAdminCreated, principalActor(principalID), "", now)
			case had && !has:
				err = appendAuditEventTx(ctx, tx, actor, AuditAIAdminRevoked, principalActor(principalID), "", now)
			}
			if err != nil {
				return err
			}
		}

		out = schema.AgentManifestV1{
			Schema:       schema.AgentManifestSchemaV1,
			PrincipalID:  principalID,
			DisplayName:  req.DisplayName,
			Tier:         req.Tier,
			Capabilities: emptyToNil(req.Capabilities),
			Channels:     emptyToNil(req.Channels),
			RateLimits:   emptyToNil(req.RateLimits),
			Permissions:  emptyToNil(req.Permissions),
			Admin:        req.Admin,
			CreatedAt:    schema.NewTimestamp(createdAt),
			UpdatedAt:    schema.NewTimestamp(updatedAt),
		}
		return nil
	})
	if err != nil {
		return schema.AgentManifestV1{}, false, err
	}
	return out, created, nil
}

// AgentManifestByPrincipal returns the manifest of an agent principal. It
// returns ErrNotFound when none is stored, and an error wrapping
// ErrInvalidManifest when the stored row does not decode or fails Validate;
// an invalid row is never returned as a usable manifest.
func (s *Store) AgentManifestByPrincipal(ctx context.Context, principalID int64) (schema.AgentManifestV1, error) {
	return agentManifestTx(ctx, s.db, principalID)
}

// agentManifestTx is AgentManifestByPrincipal inside tx, so a transaction
// deciding a permission reads the manifest it decides by (issue #1326).
func agentManifestTx(ctx context.Context, tx execer, principalID int64) (schema.AgentManifestV1, error) {
	var (
		displayName, tier, capsJSON, channelsJSON, limitsJSON, permsJSON, adminJSON string
		createdAt, updatedAt                                                        int64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT display_name, tier, capabilities, channels, rate_limits, permissions, admin, created_at, updated_at
		 FROM agent_manifests WHERE principal_id = ?`, principalID,
	).Scan(&displayName, &tier, &capsJSON, &channelsJSON, &limitsJSON, &permsJSON, &adminJSON, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return schema.AgentManifestV1{}, fmt.Errorf("store: find manifest for principal %d: %w", principalID, ErrNotFound)
	}
	if err != nil {
		return schema.AgentManifestV1{}, fmt.Errorf("store: find manifest for principal %d: %w", principalID, err)
	}

	m := schema.AgentManifestV1{
		Schema:      schema.AgentManifestSchemaV1,
		PrincipalID: principalID,
		DisplayName: displayName,
		Tier:        schema.AgentTier(tier),
		CreatedAt:   schema.NewTimestamp(time.UnixMilli(createdAt)),
		UpdatedAt:   schema.NewTimestamp(time.UnixMilli(updatedAt)),
	}
	if err := unmarshalList(capsJSON, &m.Capabilities); err != nil {
		return schema.AgentManifestV1{}, invalidManifest(principalID, "capabilities", err)
	}
	if err := unmarshalList(channelsJSON, &m.Channels); err != nil {
		return schema.AgentManifestV1{}, invalidManifest(principalID, "channels", err)
	}
	if err := unmarshalList(limitsJSON, &m.RateLimits); err != nil {
		return schema.AgentManifestV1{}, invalidManifest(principalID, "rate_limits", err)
	}
	if err := unmarshalList(permsJSON, &m.Permissions); err != nil {
		return schema.AgentManifestV1{}, invalidManifest(principalID, "permissions", err)
	}
	if adminJSON != "" {
		var a schema.AgentAdminV1
		if err := unmarshalStrict(adminJSON, &a); err != nil {
			return schema.AgentManifestV1{}, invalidManifest(principalID, "admin", err)
		}
		m.Admin = &a
	}
	if err := m.Validate(); err != nil {
		return schema.AgentManifestV1{}, fmt.Errorf("store: manifest for principal %d: %w: %w", principalID, ErrInvalidManifest, err)
	}
	return m, nil
}

// grantsRead reports whether grants give channelID the read permission.
func grantsRead(grants []schema.ChannelGrant, channelID int64) bool {
	for _, g := range grants {
		if g.ChannelID != channelID {
			continue
		}
		for _, p := range g.Permissions {
			if p == schema.ChannelPermissionRead {
				return true
			}
		}
	}
	return false
}

// actorIsOwnerTx reports whether the audit actor ("principal:<id>") is,
// read inside tx, an enabled, visible human owner. Any other actor is not.
func actorIsOwnerTx(ctx context.Context, tx execer, actor string) (bool, error) {
	idText, ok := strings.CutPrefix(actor, "principal:")
	if !ok {
		return false, nil
	}
	// An actor that does not parse names nobody, so it is not an owner.
	id, perr := strconv.ParseInt(idText, 10, 64)
	if perr != nil || id <= 0 {
		return false, nil //nolint:nilerr // a malformed actor is refused, not an error
	}
	p, found, err := queryPrincipal(ctx, tx, id)
	if err != nil || !found {
		return false, err
	}
	return p.Kind == PrincipalHuman && p.DisabledAt == nil && p.HiddenAt == nil && !p.guestTarget() && p.IsOwner(), nil
}

func invalidManifest(principalID int64, column string, err error) error {
	return fmt.Errorf("store: manifest for principal %d: %w: decode %s: %w", principalID, ErrInvalidManifest, column, err)
}

// marshalList encodes a policy list, spelling an empty list "[]" rather than "null".
func marshalList[T any](list []T) (string, error) {
	if len(list) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalList strictly decodes a stored policy list; unknown fields and
// trailing data are errors.
func unmarshalList[T any](text string, dst *[]T) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	var list []T
	if err := dec.Decode(&list); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	*dst = emptyToNil(list)
	return nil
}

// unmarshalStrict strictly decodes one stored JSON object; unknown fields
// and trailing data are errors.
func unmarshalStrict[T any](text string, dst *T) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	var v T
	if err := dec.Decode(&v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	*dst = v
	return nil
}

func emptyToNil[T any](list []T) []T {
	if len(list) == 0 {
		return nil
	}
	return list
}

// CountAgentsWithoutManifest returns how many agent principals have no
// manifest row. Such an agent is denied everything once enforcement applies.
func (s *Store) CountAgentsWithoutManifest(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM principals p
		 WHERE p.kind = 'agent' AND NOT EXISTS (SELECT 1 FROM agent_manifests m WHERE m.principal_id = p.id)`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count agents without a manifest: %w", err)
	}
	return n, nil
}
