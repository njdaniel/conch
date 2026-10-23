package cli

import (
	"context"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// Invites and their administration (issues #437 and #438), behind
// `conch admin invite`. Each is one request to a public route; the server
// decides who may make it.

// CreateInvite makes an invite. The response carries the invite's code, which
// the server shows only once.
func (c *Client) CreateInvite(ctx context.Context, request schema.CreateInviteRequestV1) (schema.CreateInviteResponseV1, error) {
	var result schema.CreateInviteResponseV1
	if err := c.roundTrip(ctx, "create invite", http.MethodPost, c.resolve("v1", "invites"), request, &result); err != nil {
		return schema.CreateInviteResponseV1{}, err
	}
	return result, nil
}

// InviteListFilter narrows ListInvites. A zero field matches everything.
type InviteListFilter struct {
	Kind      string
	State     string
	CreatedBy int64
}

// ListInvites returns the invites the caller may see, newest first: every
// invite for an operator, their own for anyone else.
func (c *Client) ListInvites(ctx context.Context, filter InviteListFilter) (schema.ListInvitesResponseV1, error) {
	endpoint := c.resolve("v1", "invites")
	query := endpoint.Query()
	if filter.Kind != "" {
		query.Set("kind", filter.Kind)
	}
	if filter.State != "" {
		query.Set("state", filter.State)
	}
	if filter.CreatedBy > 0 {
		query.Set("created_by", strconv.FormatInt(filter.CreatedBy, 10))
	}
	endpoint.RawQuery = query.Encode()
	var result schema.ListInvitesResponseV1
	if err := c.roundTrip(ctx, "list invites", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListInvitesResponseV1{}, err
	}
	return result, nil
}

// CloseInvite closes an invite so it can no longer be used.
func (c *Client) CloseInvite(ctx context.Context, id int64) error {
	endpoint := c.resolve("v1", "invites", strconv.FormatInt(id, 10))
	return c.roundTrip(ctx, "close invite", http.MethodDelete, endpoint, nil, nil)
}

// InviteJoiners returns everyone who joined through an invite.
func (c *Client) InviteJoiners(ctx context.Context, id int64) (schema.InviteJoinersResponseV1, error) {
	endpoint := c.resolve("v1", "invites", strconv.FormatInt(id, 10), "principals")
	var result schema.InviteJoinersResponseV1
	if err := c.roundTrip(ctx, "list invite joiners", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.InviteJoinersResponseV1{}, err
	}
	return result, nil
}

// DisableJoiners disables everyone who joined through an invite, or with a
// dry run says whom it would.
func (c *Client) DisableJoiners(ctx context.Context, id int64, request schema.DisableJoinersRequestV1) (schema.DisableJoinersResponseV1, error) {
	endpoint := c.resolve("v1", "invites", strconv.FormatInt(id, 10), "disable-joiners")
	var result schema.DisableJoinersResponseV1
	if err := c.roundTrip(ctx, "disable joiners", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.DisableJoinersResponseV1{}, err
	}
	return result, nil
}

// DisableInvitees disables the people a principal invited, or everyone under
// them, or with a dry run says whom it would.
func (c *Client) DisableInvitees(ctx context.Context, id int64, request schema.DisableInviteesRequestV1) (schema.DisableJoinersResponseV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(id, 10), "disable-invitees")
	var result schema.DisableJoinersResponseV1
	if err := c.roundTrip(ctx, "disable invitees", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.DisableJoinersResponseV1{}, err
	}
	return result, nil
}

// SetInvitePermission grants or takes away a person's right to invite.
func (c *Client) SetInvitePermission(ctx context.Context, id int64, may bool) error {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(id, 10), "invite-permission")
	return c.roundTrip(ctx, "set invite permission", http.MethodPut, endpoint, schema.InvitePermissionRequestV1{MayInvite: may}, nil)
}

// InviteSettings reads whether invites are paused.
func (c *Client) InviteSettings(ctx context.Context) (schema.InviteSettingsV1, error) {
	var result schema.InviteSettingsV1
	if err := c.roundTrip(ctx, "read invite settings", http.MethodGet, c.resolve("v1", "invites", "settings"), nil, &result); err != nil {
		return schema.InviteSettingsV1{}, err
	}
	return result, nil
}

// PutInviteSettings pauses or resumes every invite.
func (c *Client) PutInviteSettings(ctx context.Context, settings schema.InviteSettingsV1) (schema.InviteSettingsV1, error) {
	var result schema.InviteSettingsV1
	if err := c.roundTrip(ctx, "write invite settings", http.MethodPut, c.resolve("v1", "invites", "settings"), settings, &result); err != nil {
		return schema.InviteSettingsV1{}, err
	}
	return result, nil
}

// CreateSignInLink makes a one-time link that signs a member back into their
// own account (issue #493). The response carries its code, which the server
// shows only once.
func (c *Client) CreateSignInLink(ctx context.Context, principalID int64, request schema.CreateSignInLinkRequestV1) (schema.CreateSignInLinkResponseV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(principalID, 10), "sign-in-links")
	var result schema.CreateSignInLinkResponseV1
	if err := c.roundTrip(ctx, "create sign-in link", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.CreateSignInLinkResponseV1{}, err
	}
	return result, nil
}

// CloseSignInLink closes a sign-in link so it can no longer be used.
func (c *Client) CloseSignInLink(ctx context.Context, id int64) error {
	endpoint := c.resolve("v1", "sign-in-links", strconv.FormatInt(id, 10))
	return c.roundTrip(ctx, "close sign-in link", http.MethodDelete, endpoint, nil, nil)
}

// The joining policy and guest visits (issue #710, #711), behind `conch
// admin join` and `conch admin guest`. Operators only; the server decides.

// JoinSettings reads the joining policy, with the live guest count and the
// warnings that apply.
func (c *Client) JoinSettings(ctx context.Context) (schema.JoinSettingsResponseV1, error) {
	var result schema.JoinSettingsResponseV1
	if err := c.roundTrip(ctx, "read joining settings", http.MethodGet, c.resolve("v1", "join", "settings"), nil, &result); err != nil {
		return schema.JoinSettingsResponseV1{}, err
	}
	return result, nil
}

// PutJoinSettings changes the fields of the joining policy the request gives.
func (c *Client) PutJoinSettings(ctx context.Context, request schema.PutJoinSettingsRequestV1) (schema.JoinSettingsResponseV1, error) {
	var result schema.JoinSettingsResponseV1
	if err := c.roundTrip(ctx, "write joining settings", http.MethodPut, c.resolve("v1", "join", "settings"), request, &result); err != nil {
		return schema.JoinSettingsResponseV1{}, err
	}
	return result, nil
}

// GuestListFilter narrows ListGuests. A zero field is the server's default:
// live visits of every origin.
type GuestListFilter struct {
	State  string
	Origin string
	// Before and Limit page the list; zero leaves them to the server.
	Before int64
	Limit  int
}

// ListGuests returns one page of guest visits, newest first.
func (c *Client) ListGuests(ctx context.Context, filter GuestListFilter) (schema.ListGuestsResponseV1, error) {
	endpoint := c.resolve("v1", "guests")
	query := endpoint.Query()
	if filter.State != "" {
		query.Set("state", filter.State)
	}
	if filter.Origin != "" {
		query.Set("origin", filter.Origin)
	}
	if filter.Before > 0 {
		query.Set("before", strconv.FormatInt(filter.Before, 10))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	endpoint.RawQuery = query.Encode()
	var result schema.ListGuestsResponseV1
	if err := c.roundTrip(ctx, "list guests", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListGuestsResponseV1{}, err
	}
	return result, nil
}

// EndGuest ends one guest visit.
func (c *Client) EndGuest(ctx context.Context, id int64) (schema.EndGuestResponseV1, error) {
	endpoint := c.resolve("v1", "guests", strconv.FormatInt(id, 10), "end")
	var result schema.EndGuestResponseV1
	if err := c.roundTrip(ctx, "end guest visit", http.MethodPost, endpoint, nil, &result); err != nil {
		return schema.EndGuestResponseV1{}, err
	}
	return result, nil
}

// EndGuests ends every live visit the request selects, or with a dry run
// says which it would.
func (c *Client) EndGuests(ctx context.Context, request schema.EndGuestsRequestV1) (schema.EndGuestsResponseV1, error) {
	var result schema.EndGuestsResponseV1
	if err := c.roundTrip(ctx, "end guest visits", http.MethodPost, c.resolve("v1", "guests", "end"), request, &result); err != nil {
		return schema.EndGuestsResponseV1{}, err
	}
	return result, nil
}

// CreateConversionPermit lets one live guest create an account although its
// entry point's policy is closed. The answer carries the permit's code,
// shown this once.
func (c *Client) CreateConversionPermit(ctx context.Context, id int64, request schema.CreateConversionPermitRequestV1) (schema.CreateSignInLinkResponseV1, error) {
	endpoint := c.resolve("v1", "guests", strconv.FormatInt(id, 10), "conversion-permit")
	var result schema.CreateSignInLinkResponseV1
	if err := c.roundTrip(ctx, "create conversion permit", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.CreateSignInLinkResponseV1{}, err
	}
	return result, nil
}
