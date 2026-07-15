# Design: MCP SDK selection

- **Status:** Proposal for Nick's sign-off (governing decisions: D4, ADR-001; dep gate: ADR-000 authority table)
- **Owner:** protocol-designer (proposes), Nick (Tier-H, approves the `go.mod` dependency)
- **Issue:** #11 (P1: MCP endpoint in conchd)

`conchd` serves a native MCP endpoint (D4, ADR-001). That needs one Go MCP
library. New `go.mod` dependencies require Nick's sign-off and a
`deps-allowlist.txt` entry (ADR-000); `deps-allowlist.txt` currently reserves
the slot: *"MCP SDK: to be selected by protocol-designer in P1 and approved by
Nick before being added here."* This document is that selection. It ends with a
single recommendation and the exact allowlist line to add.

## 1. Requirements

Ranked by how much they bind us:

1. **Single-binary invariant (ADR-002, D3).** The SDK must be a pure-Go library
   linked into `conchd` — no sidecar, no bridge process, no external runtime,
   no cgo (pure-Go cross-compile is what makes single-binary distribution real,
   same reason `modernc.org/sqlite` was chosen over the C driver).
2. **Server-side streamable HTTP transport.** The issue prefers streamable HTTP
   (the current MCP spec transport). `conchd` is already an `net/http` server;
   the MCP handler must mount as an ordinary route, not own the process.
3. **Schema-first fit (D8).** Tool input/output JSON Schemas are a projection of
   `pkg/schema` (ADR-001: "never a second source of truth"). An SDK that
   generates JSON Schema from Go structs lets the tool schemas track
   `pkg/schema` instead of being hand-maintained twice.
4. **Maintenance and API stability.** This is core infrastructure for the whole
   agent front-end. A post-1.0 stability commitment is worth more than raw
   activity, because churn here ripples into every tool definition.
5. **Dependency footprint.** Transitive deps are a supply-chain surface and
   `go.sum` weight. Note the gate's actual behaviour: `scripts/depgate.sh` only
   checks **direct** requires against the allowlist ("transitive deps follow the
   direct ones"), so only the SDK's own module path needs a Nick sign-off line.
   Footprint still matters for review and audit, just not for the gate.

## 2. Candidates

| Candidate | Latest | Status | Notes |
|---|---|---|---|
| `github.com/modelcontextprotocol/go-sdk` | v1.6.1 (2026-05-22) | **post-1.0, stable** | Canonical SDK, co-maintained with the MCP spec (Google + Anthropic). |
| `github.com/mark3labs/mcp-go` | v0.56.0 (2026-07-08) | pre-1.0, very active | De-facto community SDK that predated the official one; no API-stability guarantee yet. |

Both were checked against their published `go.mod` (via `proxy.golang.org`), not
from memory. Both are pure-Go and support stdio **and** streamable HTTP, so both
clear requirements 1 and 2. The decision turns on 3–5.

## 3. Dependency footprint (verified)

Direct requires of each SDK's published `go.mod`. Test-only deps (pulled into
`go.sum` but not the built binary unless imported) are marked *(test/build)*.

**`modelcontextprotocol/go-sdk` v1.6.1**

| Module | Role |
|---|---|
| `github.com/google/jsonschema-go` v0.4.3 | JSON Schema generation (its only dep is go-cmp *(test)*) |
| `github.com/yosida95/uritemplate/v3` v3.0.2 | RFC 6570 URI templates (resources) |
| `github.com/segmentio/encoding` v0.5.4 | Fast JSON; pulls `github.com/segmentio/asm` v1.1.3 |
| `golang.org/x/oauth2` v0.35.0 | OAuth client; transitively pulls `cloud.google.com/go/compute/metadata` (download), but isn’t linked unless the SDK auth subpackages are imported |
| `github.com/golang-jwt/jwt/v5` v5.3.1 | JWT support used by the SDK auth subpackages; downloaded via `go.mod`, but isn’t linked unless those packages are imported |
| `github.com/google/go-cmp` v0.7.0 *(test/build)* | |
| `golang.org/x/tools` v0.42.0 *(test/build)* | |

**`mark3labs/mcp-go` v0.56.0**

| Module | Role |
|---|---|
| `github.com/google/jsonschema-go` v0.4.2 | JSON Schema generation (same lib as the official SDK) |
| `github.com/yosida95/uritemplate/v3` v3.0.2 | URI templates (same lib as the official SDK) |
| `github.com/santhosh-tekuri/jsonschema/v6` v6.0.2 | JSON Schema validation |
| `github.com/spf13/cast` v1.7.1 | loose type coercion of tool args |
| `github.com/google/uuid` v1.6.0 | already in conch's graph (indirect via sqlite) |
| `github.com/stretchr/testify` v1.11.1 *(test)* | pulls go-spew, go-difflib, yaml.v3, etc. |

Honest reading:

- **Neither touches the single-binary invariant.** Both link in as pure Go. No
  external process. No cgo. `segmentio/asm` is hand-written **Go assembly with
  pure-Go fallbacks**, not cgo — it still cross-compiles with `GOARCH`/`GOOS`
  and needs no C toolchain. It is a supply-chain surface, not an ADR-002
  violation. State this plainly so the `asm` dep is not mistaken for a blocker.
- **The official SDK's *extra* footprint over mark3labs is** `segmentio/encoding`
  (+`asm`) unconditionally, plus `golang.org/x/oauth2` (+`cloud.google.com/go/compute/metadata`)
  and `golang-jwt/jwt/v5` **only if we import its auth subpackage.** P1 uses a
  simple bearer token (issue #11) and does **not** need the OAuth/JWT
  machinery, so those two need not enter the build graph in P1.
- **Both now depend on the same `google/jsonschema-go` and `uritemplate/v3`.**
  The community SDK converged on the official schema library, so the
  schema-generation surface is effectively shared.
- The prior (rejected) implementation attempt pulled `jsonschema-go`,
  `segmentio/asm`, `segmentio/encoding`, `uritemplate`, and `x/oauth2` — that is
  exactly the v1.6.1 require set above, confirming the footprint is understood
  and not larger than reported.

## 4. Decision table

| Criterion | `modelcontextprotocol/go-sdk` | `mark3labs/mcp-go` |
|---|---|---|
| Single-binary / pure-Go / no cgo | Pass | Pass |
| Streamable HTTP server transport | Yes (spec-canonical) | Yes |
| Schema generation from Go structs | Yes (`jsonschema-go`) | Yes (`jsonschema-go`) |
| API stability | **v1.6.1 — post-1.0 commitment** | v0.56.0 — pre-1.0, may break |
| Spec-tracking / protocol drift risk | Lowest (co-maintained with spec) | Follows spec, community-paced |
| Runtime dep footprint | Slightly heavier (`segmentio/*`; oauth2/jwt only if auth imported) | Slightly leaner |
| `deps-allowlist.txt` cost | 1 direct line | 1 direct line |

## 5. Recommendation

**Recommended: `github.com/modelcontextprotocol/go-sdk`, pinned at v1.6.1.**

It is now post-1.0 with an API-stability commitment, it is the canonical
implementation co-maintained with the MCP specification (lowest risk of
transport/protocol drift for a project whose whole pitch is being MCP-native),
it serves the streamable-HTTP transport the issue prefers, and its schema
generation via `google/jsonschema-go` fits Conch's schema-first rule. Its
heavier dependency footprint is real but bounded and does not touch the
single-binary invariant — everything is pure Go, no external process, no cgo —
and the `oauth2`/`jwt` portion only enters the build if we import the auth
subpackage, which P1's simple bearer scheme lets us skip. `mark3labs/mcp-go` is
a credible, marginally leaner fallback, but its pre-1.0 status means
breaking-change churn we would rather not carry through the core agent surface;
reconsider it only if the official SDK's streamable-HTTP server proves unworkable
in the #11 implementation.

## 6. What Nick is signing off

Approving this proposal authorizes exactly one direct dependency. On sign-off,
the #11 implementation PR adds to `deps-allowlist.txt`:

```
# MCP SDK (issue #11): selected by protocol-designer, approved by Nick <date>.
github.com/modelcontextprotocol/go-sdk
```

and pins `require github.com/modelcontextprotocol/go-sdk v1.6.1` in `go.mod`.
Transitive deps ride along without individual allowlist lines (`depgate` gates
direct requires only), but the implementation PR's diff should surface the
resulting `go.sum` additions for review.

## 7. Explicitly out of scope

- The endpoint, transport wiring, auth, and tool schemas — see
  [mcp-endpoint.md](mcp-endpoint.md).
- OAuth/JWT agent auth. P1 is a simple bearer token (issue #11); manifests and
  capability enforcement are P2 (#20). Revisit importing the SDK's auth
  subpackage then.
- Any actual `go.mod`/`go.sum`/`deps-allowlist.txt` edit. This document is the
  prerequisite sign-off; the edit lands in the #11 implementation PR.
