// The password and operator sign-in shapes of pkg/schema/auth.go, generated
// into generated.ts: a member signing in again with a name and a password
// (issue #713); an operator's second step, a code; the recent proof of the
// password (and code) that some actions ask for; changing, setting and
// resetting a password; and an operator's setup from a host-made token, with
// the authenticator's enrolment (issue #714).
export {
  loginResponseV1,
  passwordUpdatedResponseV1,
  reauthResponseV1,
  redeemSetupTokenResponseV1,
  totpEnrollmentV1,
  type AuthStateV1,
  type ChangePasswordRequestV1,
  type ConfirmTOTPRequestV1,
  type LoginRequestV1,
  type LoginResponseV1,
  type PasswordUpdatedResponseV1,
  type ReauthRequestV1,
  type ReauthResponseV1,
  type RedeemSetupTokenRequestV1,
  type RedeemSetupTokenResponseV1,
  type ResetPasswordRequestV1,
  type SetPasswordRequestV1,
  type TOTPEnrollmentV1,
  type TOTPLoginRequestV1,
} from './generated';
