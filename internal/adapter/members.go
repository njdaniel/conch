package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// members reads GET /v1/channels/{channel}/members with the adapter's own
// credential. The server gives an agent the list on the same grant as reading
// the channel, which the adapter needs anyway.
func (m mcpAdapter) members(ctx context.Context, channel string) ([]schema.ChannelMemberV1, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.server+"/v1/channels/"+url.PathEscape(channel)+"/members", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := http.DefaultClient.Do(req) // #nosec G107 -- the operator configures the server address
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("members: server answered %s", resp.Status)
	}
	var list schema.ListChannelMembersResponseV1
	// Bounded like the relay's reads from conchd (maxUpstreamBody).
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&list); err != nil {
		return nil, fmt.Errorf("members: decode: %w", err)
	}
	return list.Members, nil
}
