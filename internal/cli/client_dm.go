package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// dmRequest uses the caller's sign-in. The server binds the participant and
// decides eligibility for each request, including reads.
func (c *Client) dmRequest(ctx context.Context, method, resource string, in, out any, parts ...string) error {
	return c.dmRoundTrip(ctx, method, resource, c.resolve(append([]string{"v1", "dms"}, parts...)...), in, out)
}

func (c *Client) dmRoundTrip(ctx context.Context, method, resource string, endpoint *url.URL, in, out any) error {
	if request, ok := in.(validator); ok {
		if err := request.Validate(); err != nil {
			return err
		}
	}
	if err := c.roundTrip(ctx, resource, method, endpoint, in, out); err != nil {
		return refused(err, dmRefusals)
	}
	if response, ok := out.(validator); ok {
		return response.Validate()
	}
	return nil
}

var dmRefusals = refusalLines{
	schema.ErrorCodeContactUnavailable:      schema.DMContactUnavailableMessage,
	schema.ErrorCodeDMNotFound:              schema.DMNotFoundMessage,
	schema.ErrorCodeDMsOff:                  schema.DMsOffMessage,
	schema.ErrorCodeOwnSignInRequired:       schema.DMOwnSignInRequiredMessage,
	schema.ErrorCodeDMRequireAuth:           schema.DMRequireAuthMessage,
	schema.ErrorCodeDMNonceConflict:         schema.DMNonceConflictMessage,
	schema.ErrorCodeMessageNotFound:         "message not found in this conversation",
	schema.ErrorCodeMessageNotEditable:      "this message cannot be edited",
	schema.ErrorCodeMessageEditWindowClosed: "the edit window has closed",
	schema.ErrorCodeMessageEditLimitReached: "the edit limit has been reached",
}

// OpenDM opens or resumes a conversation by the peer's local principal ID.
func (c *Client) OpenDM(ctx context.Context, peerID int64) (schema.DMConversationV1, error) {
	var response schema.OpenDMResponseV1
	err := c.dmRequest(ctx, http.MethodPost, "open direct message", schema.OpenDMRequestV1{PeerID: peerID}, &response)
	if err == nil && response.Conversation.PeerID != peerID {
		err = fmt.Errorf("cli: open direct message returned a different recipient")
	}
	return response.Conversation, err
}

// ListDMs returns one inbox page. An empty state selects open conversations.
func (c *Client) ListDMs(ctx context.Context, state schema.DMState, cursor string) (schema.ListDMConversationsResponseV1, error) {
	endpoint := c.resolve("v1", "dms")
	query := endpoint.Query()
	if state != "" {
		query.Set("state", string(state))
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	query.Set("limit", strconv.Itoa(schema.MaxDMConversationsPage))
	endpoint.RawQuery = query.Encode()
	var response schema.ListDMConversationsResponseV1
	err := c.dmRoundTrip(ctx, http.MethodGet, "list direct messages", endpoint, nil, &response)
	return response, err
}

// GetDM reads the selected conversation's current metadata and unread count.
func (c *Client) GetDM(ctx context.Context, id int64) (schema.DMConversationV1, error) {
	var response schema.DMConversationV1
	err := c.dmRequest(ctx, http.MethodGet, "get direct message conversation", nil, &response, strconv.FormatInt(id, 10))
	if err == nil && response.ID != id {
		err = fmt.Errorf("cli: server returned a different conversation")
	}
	return response, err
}

// ListDMMessages reads a newest-first history page before an optional message ID.
func (c *Client) ListDMMessages(ctx context.Context, id, before int64, limit int) (schema.ListDMMessagesResponseV1, error) {
	endpoint := c.resolve("v1", "dms", strconv.FormatInt(id, 10), "messages")
	query := endpoint.Query()
	query.Set("order", string(schema.DMOrderDesc))
	if before > 0 {
		query.Set("before", strconv.FormatInt(before, 10))
	}
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()
	var response schema.ListDMMessagesResponseV1
	if err := c.dmRoundTrip(ctx, http.MethodGet, "direct message history", endpoint, nil, &response); err != nil {
		return response, err
	}
	if response.Order != schema.DMOrderDesc || (len(response.Messages) > 0 && before > 0 && response.Messages[0].ID >= before) {
		return response, fmt.Errorf("cli: server returned an unexpected direct message history page")
	}
	// Validate already checks that all messages are from the same conversation.
	if len(response.Messages) > 0 && response.Messages[0].ConversationID != id {
		return response, fmt.Errorf("cli: server returned history for a different conversation")
	}
	return response, nil
}

// SendDM posts text with a client nonce so an identical retry is idempotent.
func (c *Client) SendDM(ctx context.Context, id int64, request schema.PostDMMessageRequestV1) (schema.DMMessageV1, error) {
	var response schema.PostDMMessageResponseV1
	err := c.dmRequest(ctx, http.MethodPost, "send direct message", request, &response, strconv.FormatInt(id, 10), "messages")
	if err == nil {
		err = checkDMMessage(response.Message, id, 0)
	}
	return response.Message, err
}

// EditDM updates the text of one message.
func (c *Client) EditDM(ctx context.Context, id, messageID int64, body string) (schema.DMMessageV1, error) {
	var response schema.EditDMMessageResponseV1
	err := c.dmRequest(ctx, http.MethodPatch, "edit direct message", schema.EditDMMessageRequestV1{Body: body}, &response,
		strconv.FormatInt(id, 10), "messages", strconv.FormatInt(messageID, 10))
	if err == nil {
		err = checkDMMessage(response.Message, id, messageID)
	}
	return response.Message, err
}

func checkDMMessage(msg schema.DMMessageV1, conversationID, messageID int64) error {
	if msg.ConversationID != conversationID || (messageID != 0 && msg.ID != messageID) {
		return fmt.Errorf("cli: server returned a different direct message")
	}
	return msg.Validate()
}

// RemoveDM tombstones an author's own message.
func (c *Client) RemoveDM(ctx context.Context, id, messageID int64) error {
	return c.dmRequest(ctx, http.MethodDelete, "remove direct message", nil, &noContent{},
		strconv.FormatInt(id, 10), "messages", strconv.FormatInt(messageID, 10))
}

// MarkDMRead advances the caller's read mark through a message.
func (c *Client) MarkDMRead(ctx context.Context, id, messageID int64) (schema.DMReadStateV1, error) {
	var response schema.DMReadStateV1
	err := c.dmRequest(ctx, http.MethodPut, "mark direct messages read", schema.MarkReadRequestV1{LastReadID: messageID}, &response,
		strconv.FormatInt(id, 10), "read-state")
	if err == nil && response.ConversationID != id {
		err = fmt.Errorf("cli: server returned a read mark for a different conversation")
	}
	return response, err
}
