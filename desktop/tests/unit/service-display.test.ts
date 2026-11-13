// The six operations whose answer holds a secret made for someone else (issue
// #1768): an invite's link, a sign-in link for another device, the
// authenticator's seed, a sign-in link for a person, a script's credential, a
// hook's secret. Each has its own function in main, which makes the request
// with main's bearer and answers once with the server's reply; the generic
// request still refuses all six.
import { afterEach, describe, expect, it } from 'vitest';

import { invocable, type DisplayStep } from '../../main/api/service.ts';
import { credential, sendJson, startHttp, stack, STAMP, type Fake, type Handler } from './support.ts';

const BEARER = 'conch_bearer_canary_5521';
const SECRET = 'display-secret-canary-8842';

const servers: Fake[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});

const link = { id: 5, principal_id: 7, created_by: 7, created_at: STAMP, expires_at: STAMP };
const invite = { id: 9, kind: 'member', created_by: 7, created_by_name: 'ada', created_at: STAMP, uses: 0, joiners: 0, credential_lifetime_seconds: 0 };

interface Row {
  readonly step: DisplayStep;
  readonly route: string;
  readonly params?: Readonly<Record<string, number>>;
  readonly body?: unknown;
  // The server's answer, with SECRET in it.
  readonly answer: unknown;
}

const rows: readonly Row[] = [
  { step: 'createMemberLink', route: 'POST /v2/invites', body: { kind: 'member' }, answer: { invite, code: SECRET } },
  { step: 'createSignInLink', route: 'POST /v1/sign-in-links/mine', body: { label: 'phone' }, answer: { sign_in_link: link, code: SECRET } },
  {
    step: 'totpEnroll',
    route: 'POST /v1/totp/enroll',
    answer: { secret: SECRET, issuer: 'Conch', account: 'ada', algorithm: 'SHA1', digits: 6, period_seconds: 30, uri: `otpauth://totp/Conch:ada?secret=${SECRET}` },
  },
  {
    step: 'createPrincipalSignInLink',
    route: 'POST /v1/principals/12/sign-in-links',
    params: { id: 12 },
    body: { purpose: 'sign_in' },
    answer: { sign_in_link: link, code: SECRET },
  },
  { step: 'createPrincipalCredential', route: 'POST /v1/principals/12/credentials', params: { id: 12 }, body: { label: 'ci' }, answer: { credential, token: SECRET } },
  { step: 'createHook', route: 'POST /v1/hooks', body: { channel: 'general', principal: 12 }, answer: { id: 4, token: SECRET } },
];

// A conchd that answers one route as given, to a request with BEARER.
function conchd(route: string, status: number, answer: unknown, extra: Record<string, string> = {}): Handler {
  return (request, res) => {
    if (`${request.method} ${request.url}` !== route) sendJson(res, 404, { code: 'not_found', message: 'no such route' });
    else if (request.headers['authorization'] !== `Bearer ${BEARER}`) sendJson(res, 401, { code: 'unauthorized', message: 'no' });
    else sendJson(res, status, answer, extra);
  };
}

async function ready(handler: Handler, signedIn = true) {
  const fake = await startHttp(handler);
  servers.push(fake);
  const logs: string[] = [];
  const s = stack((line) => logs.push(line));
  s.session.setOrigin(fake.origin);
  if (signedIn) s.session.adopt(BEARER);
  return { fake, logs, ...s };
}

describe.each(rows)('$step', (row) => {
  it('makes the one request with main’s bearer and answers once with the server’s reply, secret and all', async () => {
    const { service, fake } = await ready(conchd(row.route, 200, row.answer));
    const reply = await service.display(row.step, row.params, row.body);
    expect(reply).toEqual({ kind: 'reply', status: 200, body: row.answer });
    expect(fake.seen).toHaveLength(1);
    expect(`${fake.seen[0]?.method ?? ''} ${fake.seen[0]?.url ?? ''}`).toBe(row.route);
    expect(fake.seen[0]?.headers['authorization']).toBe(`Bearer ${BEARER}`);
    expect(fake.seen[0]?.body === '' ? undefined : JSON.parse(fake.seen[0]?.body ?? '')).toEqual(row.body);
  });

  it.each([
    [403, 'permission_denied', undefined],
    [409, 'conflict', undefined],
    [429, 'rate_limited', 7],
  ])('answers a server refusal %i as that refusal, with its code and Retry-After', async (status, code, retryAfter) => {
    const headers: Record<string, string> = retryAfter === undefined ? {} : { 'Retry-After': String(retryAfter) };
    const { service } = await ready(conchd(row.route, status, { code, message: 'no' }, headers));
    expect(await service.display(row.step, row.params, row.body)).toEqual({
      kind: 'reply',
      status,
      body: { code, message: 'no' },
      ...(retryAfter === undefined ? {} : { retryAfter }),
    });
  });

  it('sends nothing and says the sign-in has ended when main holds no bearer', async () => {
    const { service, fake } = await ready(conchd(row.route, 200, row.answer), false);
    expect(await service.display(row.step, row.params, row.body)).toEqual({ kind: 'unauthorized' });
    expect(fake.seen).toEqual([]);
  });

  it('never logs the secret, and neither the reply nor the log holds main’s bearer, on an answer, a refusal or a document that is not the one promised', async () => {
    for (const [status, answer] of [
      [200, row.answer],
      [403, { code: 'permission_denied', message: 'no' }],
      [200, { not: 'the document', leaked: SECRET }],
    ] as const) {
      const { service, logs } = await ready(conchd(row.route, status, answer));
      const reply = await service.display(row.step, row.params, row.body);
      expect(logs.join('\n')).not.toContain(SECRET);
      expect(logs.join('\n')).not.toContain(BEARER);
      expect(JSON.stringify(reply)).not.toContain(BEARER);
    }
  });

  it('answers the declared document only: a valid one with a token of main’s and an extra key comes back without them', async () => {
    const { service } = await ready(conchd(row.route, 200, { token: BEARER, extra: 1, ...(row.answer as Record<string, unknown>) }));
    const reply = await service.display(row.step, row.params, row.body);
    // A credential's or a hook's answer declares a field called token, which
    // is the server's own and wins; where none is declared the injected one,
    // like the extra key, is dropped by the response validator.
    const declared = row.answer as Record<string, unknown>;
    expect(reply).toEqual({ kind: 'reply', status: 200, body: declared });
    expect(JSON.stringify(reply)).not.toContain(BEARER);
    expect(JSON.stringify(reply)).not.toContain('extra');
  });

  it('answers a document that is not the one promised as no answer, without echoing it', async () => {
    const { service } = await ready(conchd(row.route, 200, { not: 'the document', leaked: SECRET }));
    const reply = await service.display(row.step, row.params, row.body);
    expect(reply).toEqual({ kind: 'none', reason: 'invalid' });
    expect(JSON.stringify(reply)).not.toContain(SECRET);
  });

  it('is still refused by the generic request, which is unchanged', async () => {
    expect(invocable(row.step)).toBe(false);
    const { service, fake } = await ready(conchd(row.route, 200, row.answer));
    expect(await service.request({ operation: row.step, ...(row.params === undefined ? {} : { params: row.params }), ...(row.body === undefined ? {} : { body: row.body }) })).toEqual({
      kind: 'none',
      reason: 'unsupported',
    });
    expect(fake.seen).toEqual([]);
  });
});
