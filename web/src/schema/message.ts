// The wire types of pkg/schema/message_v2.go and payload.go, generated into generated.ts.
export { audience, listMessagesResponseV2, messageV2, payload, postMessageRequestV2, postMessageResponseV2, searchMessagesResponseV2, type Audience, type ListMessagesResponseV2, type MessageV2, type NetAudience, type Payload, type PostMessageRequestV2, type PostMessageResponseV2, type PrincipalsAudience, type SearchMessagesResponseV2 } from './generated';
// The message editing shapes of pkg/schema/message_edit.go (issue #845,
// docs/design/message-editing.md §11): the edit and its answer, the author's
// earlier versions, the events socket's edit frame, and the window and limit
// the page uses only to stop offering Edit (the server alone decides).
export {
  MAX_MESSAGE_EDITS,
  MESSAGE_EDIT_WINDOW_SECONDS,
  editMessageRequestV1,
  editMessageResponseV1,
  listMessageRevisionsResponseV1,
  messageEditedV1,
  messageEditedV1Schema,
  messageRevisionV1,
  type EditMessageRequestV1,
  type EditMessageResponseV1,
  type ListMessageRevisionsResponseV1,
  type MessageEditedV1,
  type MessageRevisionV1,
} from './generated';
// The most bytes of one post request, the cap the page keeps a stored draft to.
export { MAX_POST_REQUEST_BYTES } from './generated';
