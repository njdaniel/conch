package cli

import (
	"context"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// Joining, guest visits, passwords, operator sign-in and device links
// (ADR-009, issues #710–#712 and #707–#709), behind `conch join`, `conch
// account`, `conch end-visit`, `conch login --password`, `conch password`,
// `conch setup` and `conch device-link`. Each is one request to a public
// route; the server decides who may make it. A request or response that
// carries a password, a code or a token is never printed or logged here.

// JoinInfo reads the bare address's front door: what it offers now.
func (c *Client) JoinInfo(ctx context.Context) (schema.JoinInfoV1, error) {
	var result schema.JoinInfoV1
	if err := c.roundTrip(ctx, "read join info", http.MethodGet, c.resolve("v1", "join"), nil, &result); err != nil {
		return schema.JoinInfoV1{}, err
	}
	return result, nil
}

// JoinAccount creates an account from the bare address, name and password
// together.
func (c *Client) JoinAccount(ctx context.Context, request schema.JoinAccountRequestV1) (schema.JoinAccountResponseV1, error) {
	var result schema.JoinAccountResponseV1
	if err := c.roundTrip(ctx, "create account", http.MethodPost, c.resolve("v1", "join", "account"), request, &result); err != nil {
		return schema.JoinAccountResponseV1{}, err
	}
	return result, nil
}

// RedeemInviteV2 creates an account through an invite, with its password.
// The v1 route makes no account once passwords exist (password_required).
func (c *Client) RedeemInviteV2(ctx context.Context, request schema.RedeemInviteRequestV2) (schema.RedeemInviteResponseV1, error) {
	var result schema.RedeemInviteResponseV1
	if err := c.roundTrip(ctx, "create account", http.MethodPost, c.resolve("v2", "invites", "redeem"), request, &result); err != nil {
		return schema.RedeemInviteResponseV1{}, err
	}
	return result, nil
}

// ConvertGuest makes the caller's live guest visit an account.
func (c *Client) ConvertGuest(ctx context.Context, request schema.ConvertGuestRequestV1) (schema.ConvertGuestResponseV1, error) {
	var result schema.ConvertGuestResponseV1
	if err := c.roundTrip(ctx, "create account", http.MethodPost, c.resolve("v1", "guest", "convert"), request, &result); err != nil {
		return schema.ConvertGuestResponseV1{}, err
	}
	return result, nil
}

// EndVisit ends the caller's own guest visit. The answer is the server's
// confirmation that it ended.
func (c *Client) EndVisit(ctx context.Context) (schema.EndVisitResponseV1, error) {
	var result schema.EndVisitResponseV1
	if err := c.roundTrip(ctx, "end visit", http.MethodPost, c.resolve("v1", "guest", "end"), nil, &result); err != nil {
		return schema.EndVisitResponseV1{}, err
	}
	return result, nil
}

// Login signs in with a name and password.
func (c *Client) Login(ctx context.Context, request schema.LoginRequestV1) (schema.LoginResponseV1, error) {
	var result schema.LoginResponseV1
	if err := c.roundTrip(ctx, "sign in", http.MethodPost, c.resolve("v1", "login"), request, &result); err != nil {
		return schema.LoginResponseV1{}, err
	}
	return result, nil
}

// LoginTOTP completes an operator's sign-in with the authenticator code.
func (c *Client) LoginTOTP(ctx context.Context, request schema.TOTPLoginRequestV1) (schema.LoginResponseV1, error) {
	var result schema.LoginResponseV1
	if err := c.roundTrip(ctx, "sign in", http.MethodPost, c.resolve("v1", "login", "totp"), request, &result); err != nil {
		return schema.LoginResponseV1{}, err
	}
	return result, nil
}

// SetPassword sets the caller's first password, or on a setup credential an
// operator's new one.
func (c *Client) SetPassword(ctx context.Context, request schema.SetPasswordRequestV1) (schema.PasswordUpdatedResponseV1, error) {
	var result schema.PasswordUpdatedResponseV1
	if err := c.roundTrip(ctx, "set password", http.MethodPost, c.resolve("v1", "password", "mine"), request, &result); err != nil {
		return schema.PasswordUpdatedResponseV1{}, err
	}
	return result, nil
}

// ChangePassword replaces the caller's password, given the current one.
func (c *Client) ChangePassword(ctx context.Context, request schema.ChangePasswordRequestV1) (schema.PasswordUpdatedResponseV1, error) {
	var result schema.PasswordUpdatedResponseV1
	if err := c.roundTrip(ctx, "change password", http.MethodPut, c.resolve("v1", "password", "mine"), request, &result); err != nil {
		return schema.PasswordUpdatedResponseV1{}, err
	}
	return result, nil
}

// ResetPassword replaces a member's forgotten password with a reset link's
// code. It issues no credential.
func (c *Client) ResetPassword(ctx context.Context, request schema.ResetPasswordRequestV1) (schema.PasswordUpdatedResponseV1, error) {
	var result schema.PasswordUpdatedResponseV1
	if err := c.roundTrip(ctx, "reset password", http.MethodPost, c.resolve("v1", "password", "reset"), request, &result); err != nil {
		return schema.PasswordUpdatedResponseV1{}, err
	}
	return result, nil
}

// Reauth proves the caller's password again (and an operator's code), for
// the actions that need a recent proof.
func (c *Client) Reauth(ctx context.Context, request schema.ReauthRequestV1) (schema.ReauthResponseV1, error) {
	var result schema.ReauthResponseV1
	if err := c.roundTrip(ctx, "re-authenticate", http.MethodPost, c.resolve("v1", "reauth"), request, &result); err != nil {
		return schema.ReauthResponseV1{}, err
	}
	return result, nil
}

// RedeemSetupToken spends a host-made setup token for a setup-scoped
// credential.
func (c *Client) RedeemSetupToken(ctx context.Context, request schema.RedeemSetupTokenRequestV1) (schema.RedeemSetupTokenResponseV1, error) {
	var result schema.RedeemSetupTokenResponseV1
	if err := c.roundTrip(ctx, "redeem setup token", http.MethodPost, c.resolve("v1", "setup", "redeem"), request, &result); err != nil {
		return schema.RedeemSetupTokenResponseV1{}, err
	}
	return result, nil
}

// EnrollTOTP starts an authenticator enrolment on a setup credential. The
// answer carries the secret, shown this once.
func (c *Client) EnrollTOTP(ctx context.Context) (schema.TOTPEnrollmentV1, error) {
	var result schema.TOTPEnrollmentV1
	if err := c.roundTrip(ctx, "enrol authenticator", http.MethodPost, c.resolve("v1", "totp", "enroll"), nil, &result); err != nil {
		return schema.TOTPEnrollmentV1{}, err
	}
	return result, nil
}

// ConfirmTOTP proves the enrolment with a code, completing setup.
func (c *Client) ConfirmTOTP(ctx context.Context, request schema.ConfirmTOTPRequestV1) error {
	return c.roundTrip(ctx, "confirm authenticator", http.MethodPost, c.resolve("v1", "totp", "confirm"), request, nil)
}

// PreviewSignInLink asks whose account a link opens and what it is for,
// without using it.
func (c *Client) PreviewSignInLink(ctx context.Context, request schema.SignInLinkPreviewRequestV1) (schema.SignInLinkPreviewV1, error) {
	var result schema.SignInLinkPreviewV1
	if err := c.roundTrip(ctx, "preview link", http.MethodPost, c.resolve("v1", "sign-in-links", "preview"), request, &result); err != nil {
		return schema.SignInLinkPreviewV1{}, err
	}
	return result, nil
}

// RedeemSignInLink uses a sign-in or device link for a new credential.
func (c *Client) RedeemSignInLink(ctx context.Context, request schema.RedeemSignInLinkRequestV1) (schema.RedeemSignInLinkResponseV1, error) {
	var result schema.RedeemSignInLinkResponseV1
	if err := c.roundTrip(ctx, "use sign-in link", http.MethodPost, c.resolve("v1", "sign-in-links", "redeem"), request, &result); err != nil {
		return schema.RedeemSignInLinkResponseV1{}, err
	}
	return result, nil
}

// CreateDeviceLink makes a link that signs the caller's own account in on
// another device. The answer carries its code, shown this once.
func (c *Client) CreateDeviceLink(ctx context.Context, request schema.CreateDeviceLinkRequestV1) (schema.CreateSignInLinkResponseV1, error) {
	var result schema.CreateSignInLinkResponseV1
	if err := c.roundTrip(ctx, "create device link", http.MethodPost, c.resolve("v1", "sign-in-links", "mine"), request, &result); err != nil {
		return schema.CreateSignInLinkResponseV1{}, err
	}
	return result, nil
}

// ListOwnDeviceLinks returns the caller's open device links, without codes.
func (c *Client) ListOwnDeviceLinks(ctx context.Context) (schema.ListOwnSignInLinksResponseV1, error) {
	var result schema.ListOwnSignInLinksResponseV1
	if err := c.roundTrip(ctx, "list device links", http.MethodGet, c.resolve("v1", "sign-in-links", "mine"), nil, &result); err != nil {
		return schema.ListOwnSignInLinksResponseV1{}, err
	}
	return result, nil
}

// CloseOwnDeviceLink closes one of the caller's device links.
func (c *Client) CloseOwnDeviceLink(ctx context.Context, id int64) error {
	endpoint := c.resolve("v1", "sign-in-links", "mine", strconv.FormatInt(id, 10))
	return c.roundTrip(ctx, "close device link", http.MethodDelete, endpoint, nil, nil)
}
