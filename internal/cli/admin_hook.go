package cli

import (
	"context"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin hook list` and `conch admin hook revoke` (issue #1228), fronting
// GET /v1/hooks and DELETE /v1/hooks/{id}. Both need manage_members. A hook
// posts as a person, so what is shown here is only what identifies one: its
// token is shown once, when it is made, and the list route never sends it;
// nothing here prints one either.

// ListHooks returns every hook the caller may see, revoked ones included. The
// route does not page.
func (c *Client) ListHooks(ctx context.Context) (schema.ListHooksResponseV1, error) {
	var result hooksDoc
	if err := c.roundTrip(ctx, "list hooks", http.MethodGet, c.resolve("v1", "hooks"), nil, &result); err != nil {
		return schema.ListHooksResponseV1{}, err
	}
	if result.Hooks == nil {
		result.Hooks = []schema.HookV1{}
	}
	return result.ListHooksResponseV1, nil
}

// RevokeHook revokes one hook by its id. Revoking one already revoked
// succeeds.
func (c *Client) RevokeHook(ctx context.Context, id int64) error {
	endpoint := c.resolve("v1", "hooks", strconv.FormatInt(id, 10))
	return c.roundTrip(ctx, "revoke hook", http.MethodDelete, endpoint, nil, nil)
}

var hookRefusals = refusalLines{
	// A member or an agent can never hold manage_members; an operator who lacks
	// it is told which permission (refusalLine).
	"forbidden":      operatorOnly,
	"hook_not_found": "no such hook, or you may not revoke it",
}

// adminHookList prints
// `<id> <active|revoked <t>> channel <name> principal <id> created <t> <label|->`
// per hook, newest first, as the server sends them. The label is last because
// it may contain spaces. A last line says how many hooks the server left out
// because they post in channels the caller is not in.
func adminHookList(ctx context.Context, r *adminRun) error {
	resp, err := r.client.ListHooks(ctx)
	if err != nil {
		return refused(err, hookRefusals)
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	for _, h := range resp.Hooks {
		state := "active"
		if h.RevokedAt != nil {
			state = "revoked " + formatTimestamp(*h.RevokedAt)
		}
		if err := r.say("%d %s channel %s principal %d created %s %s\n", h.ID, state, safeLine(h.Channel), h.Principal,
			formatTimestamp(h.CreatedAt), dashIfEmpty(safeLine(h.Label))); err != nil {
			return err
		}
	}
	if resp.OtherHooks > 0 {
		return r.say("%d more hook(s) post in channels you are not in and are not shown\n", resp.OtherHooks)
	}
	return nil
}

func adminHookRevoke(ctx context.Context, r *adminRun) error {
	if err := r.client.RevokeHook(ctx, r.id); err != nil {
		return refused(err, hookRefusals)
	}
	return r.say("revoked hook %d\n", r.id)
}
