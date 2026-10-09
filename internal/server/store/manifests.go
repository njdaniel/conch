package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		switch err := tx.QueryRowContext(ctx,
			"SELECT created_at, updated_at FROM agent_manifests WHERE principal_id = ?", principalID,
		).Scan(&prevCreated, &prevUpdated); {
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
			 (principal_id, display_name, tier, capabilities, channels, rate_limits, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (principal_id) DO UPDATE SET
			   display_name = excluded.display_name, tier = excluded.tier,
			   capabilities = excluded.capabilities, channels = excluded.channels,
			   rate_limits = excluded.rate_limits, updated_at = excluded.updated_at`,
			principalID, req.DisplayName, string(req.Tier), capsJSON, channelsJSON, limitsJSON,
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

		out = schema.AgentManifestV1{
			Schema:       schema.AgentManifestSchemaV1,
			PrincipalID:  principalID,
			DisplayName:  req.DisplayName,
			Tier:         req.Tier,
			Capabilities: emptyToNil(req.Capabilities),
			Channels:     emptyToNil(req.Channels),
			RateLimits:   emptyToNil(req.RateLimits),
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
	var (
		displayName, tier, capsJSON, channelsJSON, limitsJSON string
		createdAt, updatedAt                                  int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT display_name, tier, capabilities, channels, rate_limits, created_at, updated_at
		 FROM agent_manifests WHERE principal_id = ?`, principalID,
	).Scan(&displayName, &tier, &capsJSON, &channelsJSON, &limitsJSON, &createdAt, &updatedAt)
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
	if err := m.Validate(); err != nil {
		return schema.AgentManifestV1{}, fmt.Errorf("store: manifest for principal %d: %w: %w", principalID, ErrInvalidManifest, err)
	}
	return m, nil
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
