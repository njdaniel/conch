// The desktop's HttpTransport: every method is a call of window.conchDesktop
// (./bridge), answered by main, which makes the request, holds the bearer and
// validates the answer. This module turns main's answers back into the
// Response the client reads, so the client's own code runs unchanged.
//
// It never sees a credential. The `token` the client passes is the stand-in of
// ./credentials (or a pasted credential on its way to main); the transport
// ignores it, and main sends the real bearer. A response that carries a token
// is rebuilt with the stand-in in the token's place.
import type { LoginRequestV1, RedeemSetupTokenRequestV1, ResetPasswordRequestV1, TOTPLoginRequestV1 } from '../../schema/auth';
import type { JoinAccountRequestV1, JoinGuestRequestV1 } from '../../schema/guest';
import type { RedeemInviteGuestRequestV1, RedeemInviteRequestV2 } from '../../schema/invite';
import type { RedeemSignInLinkRequestV1 } from '../../schema/signin';
import { operations, type Call, type OperationId } from '../operations';
import type { HttpTransport } from '../types';
import type { BridgeReply, BridgeRequest, ConchDesktopBridge, NoAnswerReason, SignInOutcome } from './bridge';
import { bridge } from './access';
import { isSessionToken, STAND_IN } from './credentials';

const json = 'application/json';

const reasons: Readonly<Record<NoAnswerReason, string>> = {
  certificate: 'the server’s certificate is not trusted by this computer',
  redirect: 'the server tried to redirect the request',
  oversize: 'the server’s answer was too large',
  invalid: 'the server’s answer was not what was promised',
  stale: 'the answer was for a sign-in that has ended',
  unsupported: 'the desktop app cannot do this yet',
  'no-server': 'no server address has been set',
};

// noAnswer is "no answer at all", as fetch rejects: the client reports it as
// a network failure.
function noAnswer(reason?: NoAnswerReason): Promise<never> {
  return Promise.reject(new TypeError(reason === undefined ? 'the server did not answer' : reasons[reason]));
}

function jsonResponse(status: number, body: unknown, extra?: Readonly<Record<string, string>>): Response {
  const headers = new Headers(extra);
  const empty = body === undefined || status === 204 || status === 205 || status === 304;
  if (!empty) headers.set('Content-Type', json);
  return new Response(empty ? null : JSON.stringify(body), { status, headers });
}

function fromReply(reply: BridgeReply): Promise<Response> {
  switch (reply.kind) {
    case 'none':
      return noAnswer(reply.reason);
    case 'unauthorized':
      return Promise.resolve(jsonResponse(401, { code: 'unauthorized', message: 'The sign-in has ended.' }));
    case 'reply':
      return Promise.resolve(
        jsonResponse(reply.status, reply.body, {
          ...(reply.retryAfter === undefined ? {} : { 'Retry-After': String(reply.retryAfter) }),
          ...(reply.requestId === undefined ? {} : { 'X-Request-Id': reply.requestId }),
        }),
      );
  }
}

function requestOf(call: Call, body?: unknown, keepalive?: boolean): BridgeRequest {
  return {
    operation: call.op,
    ...(call.params === undefined ? {} : { params: call.params }),
    ...(call.query === undefined ? {} : { query: call.query }),
    ...(body === undefined ? {} : { body }),
    ...(keepalive === undefined ? {} : { keepalive }),
  };
}

// refusal rebuilds a server's refusal of a sign-in step: its status, its error
// document and its Retry-After.
function refusal(outcome: Extract<SignInOutcome, { kind: 'unauthorized' | 'invalid' }>): Response {
  const doc = {
    code: outcome.code ?? 'refused',
    message: outcome.message ?? '',
    ...(outcome.retryAfter === undefined ? {} : { retry_after_seconds: outcome.retryAfter }),
  };
  return jsonResponse(outcome.status >= 200 && outcome.status < 600 ? outcome.status : 502, doc, {
    ...(outcome.retryAfter === undefined ? {} : { 'Retry-After': String(outcome.retryAfter) }),
    ...(outcome.requestId === undefined ? {} : { 'X-Request-Id': outcome.requestId }),
  });
}

// fromOutcome is the answer of a sign-in step as the client reads it: the
// server's own document with the stand-in where the token was.
function fromOutcome(id: OperationId, outcome: SignInOutcome): Promise<Response> {
  if (outcome.ok) {
    const holdsToken = operations[id].credentials?.includes('token') === true && outcome.who !== null;
    return Promise.resolve(jsonResponse(200, holdsToken ? { ...outcome.answer, token: STAND_IN } : outcome.answer));
  }
  switch (outcome.kind) {
    case 'challenge':
      return Promise.resolve(jsonResponse(200, { next: 'totp', challenge: outcome.challenge, challenge_expires_at: outcome.expiresAt }));
    case 'unauthorized':
    case 'invalid':
      return Promise.resolve(refusal(outcome));
    case 'network':
      return noAnswer(outcome.reason);
    case 'unkept':
      return noAnswer();
  }
}

// The steps that give a credential, by operation. Each hands its body to the
// one bridge function that is allowed to carry it.
type Step = (b: ConchDesktopBridge, body: never) => Promise<SignInOutcome>;
const steps: Partial<Record<OperationId, Step>> = {
  login: (b, body: LoginRequestV1) => b.signInWithPassword(body),
  loginTotp: (b, body: TOTPLoginRequestV1) => b.signInWithCode(body),
  redeemInvite: (b, body: RedeemInviteRequestV2) => b.redeemInvite(body),
  redeemInviteGuest: (b, body: RedeemInviteGuestRequestV1) => b.redeemInviteAsGuest(body),
  redeemSignInLink: (b, body: RedeemSignInLinkRequestV1) => b.redeemSignInLink(body),
  setupRedeem: (b, body: RedeemSetupTokenRequestV1) => b.redeemSetupToken(body),
  resetPassword: (b, body: ResetPasswordRequestV1) => b.resetPassword(body),
  joinGuest: (b, body: JoinGuestRequestV1) => b.joinAsGuest(body),
  joinAccount: (b, body: JoinAccountRequestV1) => b.createAccount(body),
};

// withSignal ends a wait when the caller's signal does, as fetch's rejection.
function withSignal<T>(wait: Promise<T>, signal: AbortSignal | undefined): Promise<T> {
  if (signal === undefined) return wait;
  if (signal.aborted) return Promise.reject(new DOMException('aborted', 'AbortError'));
  return new Promise<T>((resolve, reject) => {
    const abort = () => {
      reject(new DOMException('aborted', 'AbortError'));
    };
    signal.addEventListener('abort', abort, { once: true });
    wait.then(resolve, reject).finally(() => {
      signal.removeEventListener('abort', abort);
    });
  });
}

function need(): Promise<ConchDesktopBridge> {
  const b = bridge();
  return b === undefined ? Promise.reject(new TypeError('the desktop bridge is missing')) : Promise.resolve(b);
}

const later = (what: string) => (): Promise<never> => Promise.reject(new TypeError(`${what} is not available in the desktop app yet`));

export const http: HttpTransport = {
  async get(call, token) {
    const b = await need();
    // A credential the person pasted is a candidate main has not seen: it
    // goes to main to be checked and kept there, and what comes back is the
    // account. Every other call is main's session.
    if (call.op === 'whoami' && !isSessionToken(token)) {
      const outcome = await b.signInWithCredential(token);
      return outcome.ok && outcome.who !== null ? jsonResponse(200, outcome.who) : fromOutcome(call.op, outcome);
    }
    return fromReply(await b.request(requestOf(call)));
  },

  async send(call, _token, { body, keepalive, signal }) {
    const b = await need();
    if (call.op === 'logout') {
      const ended = await withSignal(b.signOut(), signal);
      if (ended === 'unsent') return noAnswer();
      return ended === 'ended' ? jsonResponse(204, undefined) : jsonResponse(502, { code: 'unconfirmed', message: 'The server did not confirm.' });
    }
    // An operation whose answer holds a credential, a ticket or the person's
    // voice grant is main's alone: the client does not get to ask.
    if (!operations[call.op].renderer) return noAnswer('unsupported');
    return fromReply(await withSignal(b.request(requestOf(call, body, keepalive)), signal));
  },

  async open(call, body) {
    const b = await need();
    const step = steps[call.op];
    if (step === undefined) return fromReply(await b.request(requestOf(call, body)));
    const check = operations[call.op].request;
    if (check === undefined || check(body).length > 0) return noAnswer();
    return fromOutcome(call.op, await step(b, body as never));
  },

  async health(call) {
    return fromReply(await (await need()).request(requestOf(call)));
  },

  upload: later('Attachments'),
  download: later('Attachments'),
};
