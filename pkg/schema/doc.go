// Package schema is the single source of truth for every wire shape in Conch:
// message envelopes, typed payload schemas, approval objects, and resolution
// events. Nothing elsewhere in the codebase hand-rolls JSON shapes (golden
// rule 2, ADR-000 D8).
//
// Payload schemas are versioned (e.g. leviathan.trade_signal.v1). Breaking
// changes require a version bump and Nick's sign-off; compatibility is
// enforced against golden fixtures in testdata/ by scripts/schema-compat.sh.
package schema
