import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { call, fileQuery, operations, pathOf, queryOf, type OperationId } from './operations';

// internal/server/routes.go is the server's route table. Every entry of the
// operation table must be a route it serves, so the table cannot name a
// request conchd would answer 404.
// Read through Node's fs (vitest runs in Node, from web/), so the dev server
// needs no widened file access.
const routesSource = readFileSync('../internal/server/routes.go', 'utf8');

// A pattern is "METHOD /path" in a Go string; {name} wildcards are compared
// without their names (the server calls a principal {principal_id}, the table
// {principal}).
const pattern = (method: string, path: string) => `${method} ${path.replace(/\{\w+\}/g, '{}')}`;
const served = new Set([...routesSource.matchAll(/"((?:GET|POST|PUT|DELETE) \/[^"\s]*)"/g)].map((match) => {
  const [method = '', path = ''] = (match[1] ?? '').split(' ');
  return pattern(method, path);
}));

const entries = Object.entries(operations) as [OperationId, (typeof operations)[OperationId]][];

describe('the operation table', () => {
  it('reads the server route table', () => {
    expect(served.size).toBeGreaterThan(50);
    expect(served.has('GET /v1/whoami')).toBe(true);
  });

  it.each(entries)('%s is a route conchd serves', (id, entry) => {
    expect(served.has(pattern(entry.method, entry.path)), `${id}: ${entry.method} ${entry.path}`).toBe(true);
  });

  it('has one entry per request: no method and path twice', () => {
    const seen = entries.map(([, entry]) => pattern(entry.method, entry.path));
    expect(new Set(seen).size).toBe(seen.length);
  });

  it('names only absolute paths, and a credential field only where the answer holds one', () => {
    for (const [id, entry] of entries) {
      expect(entry.path.startsWith('/'), id).toBe(true);
      expect(entry.path.includes('?'), id).toBe(false);
      if (entry.credentials !== undefined) expect(entry.renderer, `${id} holds a credential`).toBe(false);
    }
  });

  // The desktop renderer may not invoke these (an answer that holds a
  // credential, a socket ticket, or the person's own voice grant). The browser
  // holds them itself and is unaffected. A change to this list is a decision.
  it('pins what the desktop renderer cannot invoke', () => {
    const blocked = entries.filter(([, entry]) => !entry.renderer).map(([id]) => id).sort();
    expect(blocked).toEqual(
      [
        'changePassword',
        'convertGuest',
        'createInvite',
        'createSignInLink',
        'joinAccount',
        'joinGuest',
        'login',
        'loginTotp',
        'reauth',
        'redeemInvite',
        'redeemInviteGuest',
        'redeemSignInLink',
        'resetPassword',
        'setPassword',
        'setupRedeem',
        'totpEnroll',
        'voiceSession',
        'wsTicket',
      ].sort(),
    );
  });

  it('marks the channel voice session grant, and every operation whose answer holds a token, as not the renderer’s', () => {
    expect(operations.voiceSession.renderer).toBe(false);
    for (const [id, entry] of entries) {
      if (entry.credentials?.includes('token') === true) expect(entry.renderer, id).toBe(false);
    }
  });

  // Every response field that is a token, a code, a ticket or a secret: the
  // operation that returns it is the main process's, with the field named.
  it('blocks, and names the field of, every operation whose answer carries a secret', () => {
    const carrying: Partial<Record<OperationId, string>> = {
      login: 'token',
      loginTotp: 'token',
      changePassword: 'token',
      setPassword: 'token',
      resetPassword: 'token',
      setupRedeem: 'token',
      redeemInvite: 'token',
      redeemInviteGuest: 'token',
      redeemSignInLink: 'token',
      joinGuest: 'token',
      joinAccount: 'token',
      convertGuest: 'token',
      wsTicket: 'ticket',
      voiceSession: 'rooms',
      createSignInLink: 'code',
      createInvite: 'code',
      totpEnroll: 'secret',
    };
    for (const [id, field] of Object.entries(carrying) as [OperationId, string][]) {
      expect(operations[id].renderer, id).toBe(false);
      expect(operations[id].credentials, id).toContain(field);
    }
  });

  it('marks the routes that take a secret with a working credential', () => {
    const secret = entries.filter(([, entry]) => entry.secret === true).map(([id]) => id).sort();
    expect(secret).toEqual(['changePassword', 'reauth', 'setPassword', 'totpConfirm']);
  });

  it('opens its sockets by ticket on GET routes', () => {
    const sockets = entries.filter(([, entry]) => entry.socket === true);
    expect(sockets.map(([id]) => id).sort()).toEqual(['eventsSocket', 'messagesSocket', 'occupancySocket', 'voiceSocket']);
    for (const [, entry] of sockets) expect(entry.method).toBe('GET');
  });
});

describe('pathOf', () => {
  it('fills the template with encoded values and leaves a plain path alone', () => {
    expect(pathOf(call('whoami'))).toBe('/v1/whoami');
    expect(pathOf(call('listMessages', { channel: 'red team' }))).toBe('/v2/channels/red%20team/messages');
    expect(pathOf(call('putMember', { channel: 'a/b', principal: 7 }))).toBe('/v1/channels/a%2Fb/members/7');
    expect(pathOf(call('revokeAll', { principal: 3 }))).toBe('/v1/principals/3/credentials/revoke-all');
  });

  it('appends the query as given', () => {
    expect(pathOf(call('listMessages', { channel: 'x' }, queryOf({ order: 'asc', limit: '50', after: '9' })))).toBe('/v2/channels/x/messages?order=asc&limit=50&after=9');
    expect(pathOf(call('uploadFile', { channel: 'red team' }, fileQuery('screen shot.png')))).toBe('/v2/channels/red%20team/files?filename=screen%20shot.png');
    expect(pathOf(call('stopShare', { channel: 'x' }, ''))).toBe('/v1/channels/x/voice/share');
  });

  it('refuses a call that leaves a value out, rather than send the braces', () => {
    expect(() => pathOf(call('listMessages'))).toThrow(/needs channel/);
  });
});
