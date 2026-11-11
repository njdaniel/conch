// The preview shape of pkg/schema/preview.go (issue #1007,
// docs/design/permission-preview.md §6), generated into generated.ts: the
// answer of GET /v1/preview, the previewed member's standing and effective
// permissions, which the page feeds to the same controls it keys on whoami.
// simulation, when present, says the answer is nobody's view (§8).
export { previewResponseV1, type PreviewResponseV1, type PreviewSimulation } from './generated';
