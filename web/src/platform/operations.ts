import { disablePrincipalResponseV1, listChannelBansResponseV1, revokeIssuedResponseV1 } from '../schema/generated';
import { reactionSummaryV1, reactionSummariesResponseV1, listReactorsResponseV1 } from '../schema/reaction';
// The operation table: every request the client makes of conchd, keyed by a
// stable id. Pure data and generated validators, with no DOM and no window, so
// that the browser transport here and the desktop main (issue #770, D2-3) read
// the same table and cannot drift: each builds its request from an entry, and
// neither accepts a URL, a header map or a method from its caller.
//
// An entry names the method and the path template, whether the call carries
// the bearer credential, the request and response validators of
// web/src/schema, whether the desktop renderer may invoke it directly, and
// which response fields are credentials that the desktop main consumes.
import {
  addChannelMemberRequestV1,
  changePasswordRequestV1,
  channelMemberV1,
  confirmTOTPRequestV1,
  convertGuestRequestV1,
  convertGuestResponseV1,
  createChannelRequestV1,
  createChannelResponseV1,
  channelDirectoryResponseV1,
  channelAuthorsResponseV1,
  joinChannelResponseV1,
  serverMembersResponseV1,
  createInterventionRequestV1,
  createInviteRequestV2,
  createInviteResponseV2,
  disableJoinersRequestV1,
  disableJoinersResponseV1,
  invitePreviewV2,
  createProblemReportRequestV1,
  createProblemReportResponseV1,
  listProblemReportsResponseV1,
  problemReportV1,
  updateProblemReportRequestV1,
  createSessionRequestV1,
  createSignInLinkResponseV1,
  createCredentialRequestV1,
  createCredentialResponseV1,
  createDeviceLinkRequestV1,
  createHookRequest,
  createPrincipalRequest,
  createPrincipalResponse,
  getAgentManifestResponseV1,
  putAgentManifestRequestV1,
  castDecisionRequestV1,
  listApprovalsResponseV1,
  createHookResponse,
  createSignInLinkRequestV1,
  hidePrincipalResponseV1,
  invitePermissionRequestV1,
  roleChangeRequestV1,
  roleChangeResponseV1,
  voicePermissionRequestV1,
  voicePermissionResponseV1,
  editMessageRequestV1,
  editMessageResponseV1,
  endGuestsRequestV1,
  endGuestsResponseV1,
  fileV1,
  health,
  interventionV1,
  inviteSettingsV1,
  inviteJoinersResponseV1,
  listInvitesResponseV2,
  permissionsResponseV1,
  previewResponseV1,
  putPermissionsRequestV1,
  invitePreviewRequestV1,
  invitePreviewV1,
  joinAccountRequestV1,
  joinAccountResponseV1,
  joinGuestRequestV1,
  joinGuestResponseV1,
  joinInfoV1,
  joinSettingsResponseV1,
  listActivityResponseV1,
  listChannelMembersResponseV1,
  listChannelsResponse,
  listGuestsResponseV1,
  listInterventionsResponseV1,
  listInvitesResponseV1,
  listMessageRevisionsResponseV1,
  listMessagesResponseV2,
  listNetsResponseV1,
  listOwnCredentialsResponseV1,
  listOwnSignInLinksResponseV1,
  listReadStateResponseV1,
  listRunsResponseV1,
  loginRequestV1,
  loginResponseV1,
  markReadRequestV1,
  passwordUpdatedResponseV1,
  postMessageRequestV2,
  postMessageResponseV2,
  putChannelMemberRequestV1,
  putJoinSettingsRequestV1,
  reauthRequestV1,
  reauthResponseV1,
  redeemInviteGuestRequestV1,
  redeemInviteRequestV2,
  redeemInviteResponseV1,
  redeemSetupTokenRequestV1,
  redeemSetupTokenResponseV1,
  redeemSignInLinkRequestV1,
  redeemSignInLinkResponseV1,
  channelSettingsRequestV1,
  channelSettingsResponseV1,
  renameChannelRequestV1,
  renameChannelResponseV1,
  renamePrincipalRequestV1,
  renamePrincipalResponseV1,
  readStateV1,
  resetPasswordRequestV1,
  revokeAllCredentialsResponseV1,
  runV1,
  searchMessagesResponseV2,
  sessionV1,
  setPasswordRequestV1,
  signInLinkPreviewRequestV1,
  signInLinkPreviewV1,
  totpEnrollmentV1,
  totpLoginRequestV1,
  voiceCameraV1,
  voiceCompanionRequestV1,
  voiceCompanionResponseV1,
  voiceOccupancyV1,
  voicePresenceV1,
  voiceSessionResponseV1,
  voiceShareV1,
  voiceTransmitReportV1,
  whoAmIResponseV1,
  wsTicketV1,
} from '../schema/generated';
import type { Validator } from '../schema/validate';

// How a call is authenticated. bearer sends the signed-in credential as the
// Authorization header; none sends no credential at all.
export type Auth = 'bearer' | 'none';

export interface Operation {
  readonly method: 'GET' | 'POST' | 'PUT' | 'DELETE';
  // The path template: {name} is replaced by the encoded parameter of that
  // name. A query is not part of the template; a call names it.
  readonly path: string;
  readonly auth: Auth;
  // The request body's validator, when the call has a JSON body with a schema
  // type; the response body's, when the answer is a document.
  readonly request?: Validator<never>;
  readonly response?: Validator<never>;
  // A secret route takes a password or a code with a working credential, and
  // answers a wrong one 401 auth_failed: that 401 does not end the sign-in.
  readonly secret?: true;
  // False when the desktop renderer must not invoke the operation itself: its
  // answer holds a credential, or a grant that only main may hold. The browser
  // is unaffected; it holds both itself.
  readonly renderer: boolean;
  // The response fields that are credentials, which the desktop main consumes
  // and never returns to the renderer.
  readonly credentials?: readonly string[];
  // True for a WebSocket upgrade: GET, opened with a single-use ticket.
  readonly socket?: true;
  // True for the five reads the server answers as a named member when a call
  // carries previewAs (Conch-Preview-As, docs/design/permission-preview.md §4).
  // The server refuses the header on every other route; the client refuses it
  // first (previewCall).
  readonly previewable?: true;
}

// op builds an entry with the renderer allowed unless said otherwise.
function op(method: Operation['method'], path: string, auth: Auth, rest: Omit<Operation, 'method' | 'path' | 'auth' | 'renderer'> & { readonly renderer?: boolean } = {}): Operation {
  const { renderer = true, ...fields } = rest;
  return { method, path, auth, renderer, ...fields };
}

// The response field a credential-bearing answer holds the token in.
const token = ['token'] as const;

// check widens a validator to the table's type. A validator of T is a function
// of unknown; the table does not use T.
const check = <T>(validator: Validator<T>): Validator<never> => validator as unknown as Validator<never>;

export const operations = {
  health: op('GET', '/healthz', 'none', { response: check(health) }),
  whoami: op('GET', '/v1/whoami', 'bearer', { response: check(whoAmIResponseV1) }),
  // Preview as member (issue #1007): the target's standing, read by an
  // administrator who sends the header. The four reads below it that are
  // marked previewable are the others the header may ride on.
  preview: op('GET', '/v1/preview', 'bearer', { response: check(previewResponseV1), previewable: true }),

  // Sign-in and the credentials it gives. Every answer that holds a token is
  // kept out of the desktop renderer's hands.
  login: op('POST', '/v1/login', 'none', { request: check(loginRequestV1), response: check(loginResponseV1), renderer: false, credentials: token }),
  loginTotp: op('POST', '/v1/login/totp', 'none', { request: check(totpLoginRequestV1), response: check(loginResponseV1), renderer: false, credentials: token }),
  logout: op('DELETE', '/v1/credentials/current', 'bearer'),
  revokeAll: op('POST', '/v1/principals/{principal}/credentials/revoke-all', 'bearer', { response: check(revokeAllCredentialsResponseV1) }),
  listOwnCredentials: op('GET', '/v1/credentials/mine', 'bearer', { response: check(listOwnCredentialsResponseV1) }),
  endOwnCredential: op('DELETE', '/v1/credentials/mine/{id}', 'bearer'),
  // An operator revokes a credential or a hook issued to someone else
  // (manage_members), as the permissions console's issued_to_others list does.
  revokeCredential: op('DELETE', '/v1/credentials/{id}', 'bearer'),
  revokeHook: op('DELETE', '/v1/hooks/{id}', 'bearer'),
  reauth: op('POST', '/v1/reauth', 'bearer', { request: check(reauthRequestV1), response: check(reauthResponseV1), secret: true, renderer: false }),
  changePassword: op('PUT', '/v1/password/mine', 'bearer', {
    request: check(changePasswordRequestV1),
    response: check(passwordUpdatedResponseV1),
    secret: true,
    renderer: false,
    credentials: token,
  }),
  setPassword: op('POST', '/v1/password/mine', 'bearer', {
    request: check(setPasswordRequestV1),
    response: check(passwordUpdatedResponseV1),
    secret: true,
    renderer: false,
    credentials: token,
  }),
  resetPassword: op('POST', '/v1/password/reset', 'none', { request: check(resetPasswordRequestV1), response: check(passwordUpdatedResponseV1), renderer: false, credentials: token }),
  setupRedeem: op('POST', '/v1/setup/redeem', 'none', { request: check(redeemSetupTokenRequestV1), response: check(redeemSetupTokenResponseV1), renderer: false, credentials: token }),
  // The authenticator's seed and its otpauth URI: a second factor in the clear.
  totpEnroll: op('POST', '/v1/totp/enroll', 'bearer', { response: check(totpEnrollmentV1), renderer: false, credentials: ['secret', 'uri'] }),
  totpConfirm: op('POST', '/v1/totp/confirm', 'bearer', { request: check(confirmTOTPRequestV1), secret: true }),

  // Devices.
  createSignInLink: op('POST', '/v1/sign-in-links/mine', 'bearer', {
    request: check(createDeviceLinkRequestV1),
    // The one-use link's code signs an account in on another device.
    response: check(createSignInLinkResponseV1),
    renderer: false,
    credentials: ['code'],
  }),
  listSignInLinks: op('GET', '/v1/sign-in-links/mine', 'bearer', { response: check(listOwnSignInLinksResponseV1) }),
  closeSignInLink: op('DELETE', '/v1/sign-in-links/mine/{id}', 'bearer'),
  previewSignInLink: op('POST', '/v1/sign-in-links/preview', 'none', { request: check(signInLinkPreviewRequestV1), response: check(signInLinkPreviewV1) }),
  redeemSignInLink: op('POST', '/v1/sign-in-links/redeem', 'none', {
    request: check(redeemSignInLinkRequestV1),
    response: check(redeemSignInLinkResponseV1),
    renderer: false,
    credentials: token,
  }),

  // Joining, invites and guests. Guests are removed for now (issue #1005):
  // the server refuses joinGuest and redeemInviteGuest (403 guests_disabled)
  // and no page calls them. They stay, renderer: false, because the desktop
  // bridge's functions joinAsGuest and redeemInviteAsGuest name them
  // (desktop/shared/bridge-names.ts, desktop/main/ipc.ts).
  joinInfo: op('GET', '/v1/join', 'none', { response: check(joinInfoV1) }),
  joinGuest: op('POST', '/v1/join/guest', 'none', { request: check(joinGuestRequestV1), response: check(joinGuestResponseV1), renderer: false, credentials: token }),
  joinAccount: op('POST', '/v1/join/account', 'none', { request: check(joinAccountRequestV1), response: check(joinAccountResponseV1), renderer: false, credentials: token }),
  joinSettings: op('GET', '/v1/join/settings', 'bearer', { response: check(joinSettingsResponseV1) }),
  putJoinSettings: op('PUT', '/v1/join/settings', 'bearer', { request: check(putJoinSettingsRequestV1), response: check(joinSettingsResponseV1) }),
  inviteSettings: op('GET', '/v1/invites/settings', 'bearer', { response: check(inviteSettingsV1) }),
  putInviteSettings: op('PUT', '/v1/invites/settings', 'bearer', { request: check(inviteSettingsV1), response: check(inviteSettingsV1) }),
  listGuests: op('GET', '/v1/guests', 'bearer', { response: check(listGuestsResponseV1) }),
  endGuests: op('POST', '/v1/guests/end', 'bearer', { request: check(endGuestsRequestV1), response: check(endGuestsResponseV1) }),
  endVisit: op('POST', '/v1/guest/end', 'bearer'),
  convertGuest: op('POST', '/v1/guest/convert', 'bearer', {
    request: check(convertGuestRequestV1),
    response: check(convertGuestResponseV1),
    renderer: false,
    credentials: token,
  }),
  // The invite's code grants an account, once, to whoever holds it.
  listInvites: op('GET', '/v1/invites', 'bearer', { response: check(listInvitesResponseV1) }),
  // Member links and the permissions document (ADR-014).
  listMemberLinks: op('GET', '/v2/invites', 'bearer', { response: check(listInvitesResponseV2) }),
  memberLinkJoiners: op('GET', '/v2/invites/{id}/joiners', 'bearer', { response: check(inviteJoinersResponseV1) }),
  // A member link's code is in the answer of its creation, as an invite's was.
  createMemberLink: op('POST', '/v2/invites', 'bearer', { request: check(createInviteRequestV2), response: check(createInviteResponseV2), renderer: false, credentials: ['code'] }),
  previewMemberLink: op('POST', '/v2/invites/preview', 'none', { request: check(invitePreviewRequestV1), response: check(invitePreviewV2) }),
  disableJoiners: op('POST', '/v1/invites/{id}/disable-joiners', 'bearer', { request: check(disableJoinersRequestV1), response: check(disableJoinersResponseV1) }),
  getPermissions: op('GET', '/v1/permissions', 'bearer', { response: check(permissionsResponseV1) }),
  putPermissions: op('PUT', '/v1/permissions', 'bearer', { request: check(putPermissionsRequestV1), response: check(permissionsResponseV1) }),
  closeInvite: op('DELETE', '/v1/invites/{id}', 'bearer'),
  previewInvite: op('POST', '/v1/invites/preview', 'none', { request: check(invitePreviewRequestV1), response: check(invitePreviewV1) }),
  redeemInvite: op('POST', '/v2/invites/redeem', 'none', { request: check(redeemInviteRequestV2), response: check(redeemInviteResponseV1), renderer: false, credentials: token }),
  redeemInviteGuest: op('POST', '/v1/invites/redeem-guest', 'none', {
    request: check(redeemInviteGuestRequestV1),
    response: check(joinGuestResponseV1),
    renderer: false,
    credentials: token,
  }),

  // People and channels.
  // The Members section's actions on a person (ADR-014 design §10.2, issue
  // #1020). The answers that hold a link's code or a token are kept out of the
  // desktop renderer's hands, as every credential is.
  setRole: op('PUT', '/v1/principals/{id}/role', 'bearer', { request: check(roleChangeRequestV1), response: check(roleChangeResponseV1) }),
  setVoicePermission: op('PUT', '/v1/principals/{id}/voice-permission', 'bearer', { request: check(voicePermissionRequestV1), response: check(voicePermissionResponseV1) }),
  setInvitePermission: op('PUT', '/v1/principals/{id}/invite-permission', 'bearer', { request: check(invitePermissionRequestV1) }),
  hidePrincipal: op('POST', '/v1/principals/{id}/hide', 'bearer', { response: check(hidePrincipalResponseV1) }),
  // 200 with what the person issued to others (issue #1464), or 204 from an older server.
  disablePrincipal: op('POST', '/v1/principals/{id}/disable', 'bearer', { response: check(disablePrincipalResponseV1) }),
  // The owner's one call to end what a person issued to others (ADR-014 O13).
  revokeIssued: op('POST', '/v1/principals/{id}/issued/revoke-all', 'bearer', { response: check(revokeIssuedResponseV1) }),
  enablePrincipal: op('POST', '/v1/principals/{id}/enable', 'bearer'),
  // Add an AI administrator (issue #1334, docs/design/ai-administrators.md
  // §4.2): the same routes as `conch admin ai-admin create`.
  createPrincipal: op('POST', '/v0/principals', 'bearer', { request: check(createPrincipalRequest), response: check(createPrincipalResponse) }),
  getManifest: op('GET', '/v1/principals/{id}/manifest', 'bearer', { response: check(getAgentManifestResponseV1) }),
  putManifest: op('PUT', '/v1/principals/{id}/manifest', 'bearer', { request: check(putAgentManifestRequestV1) }),
  // A decision on an approval, cast from an admin-action card.
  listApprovals: op('GET', '/v1/approvals', 'bearer', { response: check(listApprovalsResponseV1) }),
  castDecision: op('POST', '/v1/approvals/{id}/decisions', 'bearer', { request: check(castDecisionRequestV1) }),
  createPrincipalSignInLink: op('POST', '/v1/principals/{id}/sign-in-links', 'bearer', {
    request: check(createSignInLinkRequestV1),
    response: check(createSignInLinkResponseV1),
    renderer: false,
    credentials: ['code'],
  }),
  createPrincipalCredential: op('POST', '/v1/principals/{id}/credentials', 'bearer', {
    request: check(createCredentialRequestV1),
    response: check(createCredentialResponseV1),
    renderer: false,
    credentials: token,
  }),
  createHook: op('POST', '/v1/hooks', 'bearer', { request: check(createHookRequest), response: check(createHookResponse), renderer: false, credentials: token }),
  renamePrincipal: op('PUT', '/v1/principals/{id}/name', 'bearer', { request: check(renamePrincipalRequestV1), response: check(renamePrincipalResponseV1) }),
  listChannels: op('GET', '/v1/channels', 'bearer', { response: check(listChannelsResponse), previewable: true }),
  createChannel: op('POST', '/v1/channels', 'bearer', { request: check(createChannelRequestV1), response: check(createChannelResponseV1) }),
  // The directory, self-join and leave (issues #982, #991), and the
  // community's member list (issue #983).
  channelDirectory: op('GET', '/v1/directory/channels', 'bearer', { response: check(channelDirectoryResponseV1), previewable: true }),
  joinChannel: op('POST', '/v1/channels/{channel}/join', 'bearer', { response: check(joinChannelResponseV1) }),
  leaveChannel: op('POST', '/v1/channels/{channel}/leave', 'bearer'),
  directoryMembers: op('GET', '/v1/directory/members', 'bearer', { response: check(serverMembersResponseV1), previewable: true }),
  renameChannel: op('PUT', '/v1/channels/{id}/name', 'bearer', { request: check(renameChannelRequestV1), response: check(renameChannelResponseV1) }),
  channelSettings: op('PUT', '/v1/channels/{id}/settings', 'bearer', { request: check(channelSettingsRequestV1), response: check(channelSettingsResponseV1) }),
  listMembers: op('GET', '/v1/channels/{channel}/members', 'bearer', { response: check(listChannelMembersResponseV1), previewable: true }),
  channelAuthors: op('GET', '/v1/channels/{channel}/authors', 'bearer', { response: check(channelAuthorsResponseV1) }),
  addMember: op('POST', '/v1/channels/{channel}/members', 'bearer', { request: check(addChannelMemberRequestV1), response: check(channelMemberV1) }),
  putMember: op('PUT', '/v1/channels/{channel}/members/{principal}', 'bearer', { request: check(putChannelMemberRequestV1) }),
  listBans: op('GET', '/v1/channels/{channel}/bans', 'bearer', { response: check(listChannelBansResponseV1) }),
  banMember: op('PUT', '/v1/channels/{channel}/bans/{principal}', 'bearer'),
  unbanMember: op('DELETE', '/v1/channels/{channel}/bans/{principal}', 'bearer'),
  kickVoice: op('DELETE', '/v1/channels/{channel}/voice/participants/{principal}', 'bearer'),
  removeMessage: op('DELETE', '/v2/channels/{channel}/messages/{id}', 'bearer'),
  removeMember: op('DELETE', '/v1/channels/{channel}/members/{principal}', 'bearer'),
  listNets: op('GET', '/v1/channels/{channel}/nets', 'bearer', { response: check(listNetsResponseV1) }),

  // Messages and files.
  listMessages: op('GET', '/v2/channels/{channel}/messages', 'bearer', { response: check(listMessagesResponseV2) }),
  postMessage: op('POST', '/v2/channels/{channel}/messages', 'bearer', { request: check(postMessageRequestV2), response: check(postMessageResponseV2) }),
  searchMessages: op('GET', '/v2/channels/{channel}/messages/search', 'bearer', { response: check(searchMessagesResponseV2) }),
  // The author's own recent text (issue #845): the edit, and the earlier
  // versions only the author may read.
  editMessage: op('PUT', '/v2/channels/{channel}/messages/{id}/body', 'bearer', { request: check(editMessageRequestV1), response: check(editMessageResponseV1) }),
  readReactions: op('GET', '/v2/channels/{channel}/reactions', 'bearer', { response: check(reactionSummariesResponseV1) }),
  listReactors: op('GET', '/v2/channels/{channel}/messages/{id}/reactions/{emoji}', 'bearer', { response: check(listReactorsResponseV1) }),
  setReaction: op('PUT', '/v2/channels/{channel}/messages/{id}/reactions/{emoji}', 'bearer', { response: check(reactionSummaryV1) }),
  removeReaction: op('DELETE', '/v2/channels/{channel}/messages/{id}/reactions/{emoji}', 'bearer', { response: check(reactionSummaryV1) }),
  moderateReaction: op('DELETE', '/v2/channels/{channel}/messages/{id}/reactions/{emoji}/{principal}', 'bearer', { response: check(reactionSummaryV1) }),
  removeReactor: op('DELETE', '/v2/channels/{channel}/messages/{id}/reactors/{principal}', 'bearer', { response: check(reactionSummaryV1) }),
  messageRevisions: op('GET', '/v2/channels/{channel}/messages/{id}/revisions', 'bearer', { response: check(listMessageRevisionsResponseV1) }),
  uploadFile: op('POST', '/v2/channels/{channel}/files', 'bearer', { response: check(fileV1) }),
  fileMeta: op('GET', '/v2/files/{id}/meta', 'bearer', { response: check(fileV1) }),
  fileBytes: op('GET', '/v2/files/{id}', 'bearer'),
  listReadState: op('GET', '/v1/read-state', 'bearer', { response: check(listReadStateResponseV1) }),
  markRead: op('PUT', '/v1/channels/{channel}/read-state', 'bearer', { request: check(markReadRequestV1), response: check(readStateV1) }),

  // Live feeds. A ticket is a single-use credential for one socket: main asks
  // for it and opens the socket itself, so the renderer never holds one.
  wsTicket: op('POST', '/v1/ws-tickets', 'bearer', { response: check(wsTicketV1), renderer: false, credentials: ['ticket'] }),
  messagesSocket: op('GET', '/v2/ws', 'bearer', { socket: true }),
  eventsSocket: op('GET', '/v1/events/ws', 'bearer', { socket: true }),
  voiceSocket: op('GET', '/v1/voice/ws', 'bearer', { socket: true }),
  occupancySocket: op('GET', '/v1/voice/occupancy/ws', 'bearer', { socket: true }),

  // The browser's own session, and agent runs.
  createSession: op('POST', '/v1/sessions', 'bearer', { request: check(createSessionRequestV1), response: check(sessionV1) }),
  endSession: op('DELETE', '/v1/sessions/{id}', 'bearer'),
  listRuns: op('GET', '/v1/runs', 'bearer', { response: check(listRunsResponseV1) }),
  getRun: op('GET', '/v1/runs/{id}', 'bearer', { response: check(runV1) }),
  listActivity: op('GET', '/v1/runs/{id}/activity', 'bearer', { response: check(listActivityResponseV1) }),
  listInterventions: op('GET', '/v1/runs/{id}/interventions', 'bearer', { response: check(listInterventionsResponseV1) }),
  createIntervention: op('POST', '/v1/runs/{id}/interventions', 'bearer', { request: check(createInterventionRequestV1), response: check(interventionV1) }),

  // Voice. The session grant holds the join token of the person's own voice:
  // the desktop renderer never holds it (D1-2), so it cannot ask for one.
  voiceSession: op('POST', '/v1/channels/{channel}/voice/session', 'bearer', { response: check(voiceSessionResponseV1), renderer: false, credentials: ['rooms'] }),
  voicePresence: op('GET', '/v1/channels/{channel}/voice', 'bearer', { response: check(voicePresenceV1) }),
  voiceTransmit: op('POST', '/v1/channels/{channel}/voice/transmit', 'bearer', { request: check(voiceTransmitReportV1) }),
  voiceOccupancy: op('GET', '/v1/voice/occupancy', 'bearer', { response: check(voiceOccupancyV1) }),
  claimShare: op('POST', '/v1/channels/{channel}/voice/share', 'bearer', { response: check(voiceShareV1) }),
  stopShare: op('DELETE', '/v1/channels/{channel}/voice/share', 'bearer'),
  // The viewer (issue #936, voice-control-plane.md §13): the second
  // participant, p<id>.view, that lets a tab watch beside the voice of
  // another client. Its answer holds a join token, so the renderer never
  // invokes it; the DELETE removes it and answers 204.
  voiceViewer: op('POST', '/v1/channels/{channel}/voice/companion', 'bearer', { request: check(voiceCompanionRequestV1), response: check(voiceCompanionResponseV1), renderer: false, credentials: token }),
  voiceViewerEnd: op('DELETE', '/v1/channels/{channel}/voice/companion', 'bearer'),
  // The camera claim (ADR-015): claimed by the person's camera button, or
  // continued by naming the claim (?started_at=), and released.
  claimCamera: op('POST', '/v1/channels/{channel}/voice/camera', 'bearer', { response: check(voiceCameraV1) }),
  stopCamera: op('DELETE', '/v1/channels/{channel}/voice/camera', 'bearer'),

  // Report a problem.
  sendReport: op('POST', '/v1/reports', 'bearer', { request: check(createProblemReportRequestV1), response: check(createProblemReportResponseV1) }),
  // Problem reports, for operators (issue #1029): the list, one report, and
  // marking one filed.
  listReports: op('GET', '/v1/reports', 'bearer', { response: check(listProblemReportsResponseV1) }),
  getReport: op('GET', '/v1/reports/{id}', 'bearer', { response: check(problemReportV1) }),
  fileReport: op('PUT', '/v1/reports/{id}', 'bearer', { request: check(updateProblemReportRequestV1), response: check(problemReportV1) }),
} as const satisfies Record<string, Operation>;

export type OperationId = keyof typeof operations;

// The values a path template takes. Numbers are written as decimal.
export type Params = Readonly<Record<string, string | number>>;

// A call: an operation, the values of its path, and its query. query is
// already encoded and has no leading "?"; build it with queryOf, or for a
// file name with fileQuery.
export interface Call {
  readonly op: OperationId;
  readonly params?: Params;
  readonly query?: string;
  // The principal whose view this read asks for (Conch-Preview-As). Set only
  // by previewCall, on the five previewable reads; a transport sends it as the
  // header on a GET and refuses it on anything else. There is no ambient
  // setting: a call carries it or it does not.
  readonly previewAs?: number;
}

// The header that asks a previewable read to answer as another member.
export const previewHeader = 'Conch-Preview-As';

export function call(id: OperationId, params?: Params, query?: string): Call {
  return { op: id, ...(params === undefined ? {} : { params }), ...(query === undefined ? {} : { query }) };
}

// previewCall is a call that asks a previewable read to answer as target. An
// operation not marked previewable is a mistake in the caller, never sent.
export function previewCall(id: OperationId, target: number, params?: Params, query?: string): Call {
  if (operations[id].previewable !== true) throw new Error(`operation ${id} cannot be previewed`);
  return { ...call(id, params, query), previewAs: target };
}

// queryOf encodes a query as the platform's URLSearchParams does.
export function queryOf(values: Readonly<Record<string, string>>): string {
  return new URLSearchParams(values).toString();
}

// fileQuery is the query of an upload: the file's name, escaped as a URI
// component (a space is %20).
export function fileQuery(name: string): string {
  return `filename=${encodeURIComponent(name)}`;
}

// pathOf is the request target of a call: the path template with each {name}
// replaced by the encoded value, then the query. A name the call does not
// give is an error in the caller, never sent as it stands.
export function pathOf(c: Call): string {
  const template = operations[c.op].path;
  const path = template.replace(/\{(\w+)\}/g, (_, name: string) => {
    const value = c.params?.[name];
    if (value === undefined) throw new Error(`operation ${c.op} needs ${name}`);
    return encodeURIComponent(String(value));
  });
  return c.query === undefined || c.query === '' ? path : `${path}?${c.query}`;
}
