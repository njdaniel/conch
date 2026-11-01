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
  createChannelRequest,
  createChannelResponse,
  createInterventionRequestV1,
  createInviteRequestV1,
  createInviteResponseV1,
  createProblemReportRequestV1,
  createProblemReportResponseV1,
  listProblemReportsResponseV1,
  problemReportV1,
  updateProblemReportRequestV1,
  createSessionRequestV1,
  createSignInLinkResponseV1,
  createDeviceLinkRequestV1,
  editMessageRequestV1,
  editMessageResponseV1,
  endGuestsRequestV1,
  endGuestsResponseV1,
  fileV1,
  health,
  interventionV1,
  inviteSettingsV1,
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

  // Sign-in and the credentials it gives. Every answer that holds a token is
  // kept out of the desktop renderer's hands.
  login: op('POST', '/v1/login', 'none', { request: check(loginRequestV1), response: check(loginResponseV1), renderer: false, credentials: token }),
  loginTotp: op('POST', '/v1/login/totp', 'none', { request: check(totpLoginRequestV1), response: check(loginResponseV1), renderer: false, credentials: token }),
  logout: op('DELETE', '/v1/credentials/current', 'bearer'),
  revokeAll: op('POST', '/v1/principals/{principal}/credentials/revoke-all', 'bearer', { response: check(revokeAllCredentialsResponseV1) }),
  listOwnCredentials: op('GET', '/v1/credentials/mine', 'bearer', { response: check(listOwnCredentialsResponseV1) }),
  endOwnCredential: op('DELETE', '/v1/credentials/mine/{id}', 'bearer'),
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
  createInvite: op('POST', '/v1/invites', 'bearer', { request: check(createInviteRequestV1), response: check(createInviteResponseV1), renderer: false, credentials: ['code'] }),
  listInvites: op('GET', '/v1/invites', 'bearer', { response: check(listInvitesResponseV1) }),
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
  renamePrincipal: op('PUT', '/v1/principals/{id}/name', 'bearer', { request: check(renamePrincipalRequestV1), response: check(renamePrincipalResponseV1) }),
  listChannels: op('GET', '/v1/channels', 'bearer', { response: check(listChannelsResponse) }),
  createChannel: op('POST', '/v0/channels', 'bearer', { request: check(createChannelRequest), response: check(createChannelResponse) }),
  renameChannel: op('PUT', '/v1/channels/{id}/name', 'bearer', { request: check(renameChannelRequestV1), response: check(renameChannelResponseV1) }),
  listMembers: op('GET', '/v1/channels/{channel}/members', 'bearer', { response: check(listChannelMembersResponseV1) }),
  addMember: op('POST', '/v1/channels/{channel}/members', 'bearer', { request: check(addChannelMemberRequestV1), response: check(channelMemberV1) }),
  putMember: op('PUT', '/v1/channels/{channel}/members/{principal}', 'bearer', { request: check(putChannelMemberRequestV1) }),
  removeMember: op('DELETE', '/v1/channels/{channel}/members/{principal}', 'bearer'),
  listNets: op('GET', '/v1/channels/{channel}/nets', 'bearer', { response: check(listNetsResponseV1) }),

  // Messages and files.
  listMessages: op('GET', '/v2/channels/{channel}/messages', 'bearer', { response: check(listMessagesResponseV2) }),
  postMessage: op('POST', '/v2/channels/{channel}/messages', 'bearer', { request: check(postMessageRequestV2), response: check(postMessageResponseV2) }),
  searchMessages: op('GET', '/v2/channels/{channel}/messages/search', 'bearer', { response: check(searchMessagesResponseV2) }),
  // The author's own recent text (issue #845): the edit, and the earlier
  // versions only the author may read.
  editMessage: op('PUT', '/v2/channels/{channel}/messages/{id}/body', 'bearer', { request: check(editMessageRequestV1), response: check(editMessageResponseV1) }),
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
}

export function call(id: OperationId, params?: Params, query?: string): Call {
  return { op: id, ...(params === undefined ? {} : { params }), ...(query === undefined ? {} : { query }) };
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
