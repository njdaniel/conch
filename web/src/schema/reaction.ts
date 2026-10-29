// The reaction shapes of pkg/schema/reaction.go, generated into generated.ts
// (issue #953, docs/design/message-reactions.md §5): a message's summary,
// which the events socket also sends, the batch read, one page of the
// principals holding one emoji, and the catalog version and bounds the page
// must agree with the server on. The catalog itself is web/src/emoji/catalog.json.
export {
  DEFAULT_REACTOR_LIMIT,
  EMOJI_VERSION,
  MAX_REACTION_BATCH_MESSAGES,
  MAX_REACTION_KEY_BYTES,
  MAX_REACTION_KEY_CODE_POINTS,
  MAX_REACTOR_LIMIT,
  listReactorsResponseV1,
  reactionSummariesResponseV1,
  reactionSummaryV1,
  reactionSummaryV1Schema as reactionSummarySchema,
  reactionV1,
  type ListReactorsResponseV1,
  type ReactionCountV1,
  type ReactionSummariesResponseV1,
  type ReactionSummaryV1,
  type ReactionV1,
} from './generated';
