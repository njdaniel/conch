// The sign-in link shapes of pkg/schema/sign_in_link.go, generated into
// generated.ts: the landing page of a link an operator made asks whose
// account it opens, then uses it (issue #493), or replaces a forgotten
// password with it (issue #714). The page makes one kind itself: a device
// link, for the person's own account (Add a device, issue #714). The browser
// suite's operator makes the others, as `conch admin principal sign-in-link`
// does, with the create shapes.
export {
  createSignInLinkResponseV1,
  listOwnSignInLinksResponseV1,
  redeemSignInLinkRequestV1,
  redeemSignInLinkResponseV1,
  signInLinkPreviewRequestV1,
  signInLinkPreviewV1,
  type CreateDeviceLinkRequestV1,
  type CreateSignInLinkRequestV1,
  type CreateSignInLinkResponseV1,
  type ListOwnSignInLinksResponseV1,
  type RedeemSignInLinkRequestV1,
  type RedeemSignInLinkResponseV1,
  type SignInLinkPreviewRequestV1,
  type SignInLinkPreviewV1,
  type SignInLinkPurpose,
  type SignInLinkV1,
} from './generated';
