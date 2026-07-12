## Issue

Closes #<!-- issue number — every PR references exactly one issue (CLAUDE.md rule 5) -->

## What changed

## Invariant checklist

- [ ] No required external process introduced (single-binary invariant, ADR-002)
- [ ] All wire shapes come from `pkg/schema`; schema changes followed the `schema-change` skill (ADR-000 D8)
- [ ] API parity holds — no CLI/TUI/MCP capability without a REST/WS equivalent (ADR-001)
- [ ] No failing test skipped or deleted without being flagged below
- [ ] Scope limited to the referenced issue

## Approval path

- [ ] **This PR touches the approval path (request → notify → resolve → audit).** If checked: the `approval-path` label is applied, the full-chain E2E test is included, and **Nick merges this PR personally.**

## Tests

<!-- What was run, what it covers. Note any skipped/flaky tests here. -->
