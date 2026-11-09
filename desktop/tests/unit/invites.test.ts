import { describe, expect, it } from 'vitest';

import { Invites, MAX_INVITE_LINK_LENGTH, parseInviteLink, PENDING_MS, STAGED_MS, type InvitesDeps } from '../../main/invites.ts';
import { TeardownHooks } from '../../main/profiles/hooks.ts';
import { ProfilesService } from '../../main/profiles/service.ts';
import { ProfileStore } from '../../main/profiles/store.ts';
import { fakeFactory, fakeVault, memoryFiles, ORIGIN_A, ORIGIN_B } from './profiles-support.ts';

// A code that is easy to find in a log, a file or an answer.
const CODE = 'CANARY-invite_code.9x~';
const link = (origin: string, code = CODE): string => `${origin}/app/#/invite/${code}`;

describe('parseInviteLink', () => {
  it('accepts exactly <origin>/app/#/invite/<code>, canonicalising the origin', () => {
    expect(parseInviteLink(link(ORIGIN_A))).toEqual({ ok: true, origin: ORIGIN_A, code: CODE });
    expect(parseInviteLink(`  ${link(ORIGIN_A)}\n`)).toEqual({ ok: true, origin: ORIGIN_A, code: CODE });
    expect(parseInviteLink('HTTPS://Chat.Example.ORG:443/app/#/invite/abc')).toEqual({ ok: true, origin: 'https://chat.example.org', code: 'abc' });
    expect(parseInviteLink('https://chat.example.org:8443/app/#/invite/abc')).toMatchObject({ ok: true, origin: 'https://chat.example.org:8443' });
    expect(parseInviteLink('http://localhost:8080/app/#/invite/abc')).toEqual({ ok: true, origin: 'http://localhost:8080', code: 'abc' });
    expect(parseInviteLink('http://127.0.0.1/app/#/invite/abc')).toMatchObject({ ok: true, origin: 'http://127.0.0.1' });
    expect(parseInviteLink('http://[::1]:3000/app/#/invite/abc')).toMatchObject({ ok: true, origin: 'http://[::1]:3000' });
    // The escapes inviteLink() makes are undone.
    expect(parseInviteLink(link(ORIGIN_A, 'a%2Db'))).toMatchObject({ ok: true, code: 'a-b' });
    expect(parseInviteLink(link(ORIGIN_A, 'x'.repeat(256)))).toMatchObject({ ok: true });
  });

  const refused: [string, unknown, string][] = [
    ['http off loopback', `http://chat.example.org/app/#/invite/${CODE}`, 'scheme'],
    ['javascript:', `javascript:alert('${CODE}')`, 'scheme'],
    ['file:', `file:///etc/passwd#/invite/${CODE}`, 'scheme'],
    ['the conch: scheme (#1592 owns it)', `conch://invite?server=${ORIGIN_A}&code=${CODE}`, 'scheme'],
    ['data:', `data:text/html,${CODE}`, 'scheme'],
    ['no scheme', `chat.example.org/app/#/invite/${CODE}`, 'malformed'],
    ['https: with no slashes', `https:chat.example.org/app/#/invite/${CODE}`, 'malformed'],
    ['userinfo', `https://nick:pw@chat.example.org/app/#/invite/${CODE}`, 'userinfo'],
    ['userinfo that looks like a host', `https://chat.example.org@evil.example/app/#/invite/${CODE}`, 'userinfo'],
    ['a backslash', `https://chat.example.org\\@evil.example/app/#/invite/${CODE}`, 'malformed'],
    ['a punycode host', `https://xn--80ak6aa92e.example/app/#/invite/${CODE}`, 'host'],
    ['a punycode label deeper in', `https://chat.xn--e1afmkfd.example/app/#/invite/${CODE}`, 'host'],
    ['a percent-encoded host', `https://chat%2eexample.org/app/#/invite/${CODE}`, 'host'],
    ['a non-ASCII host', `https://chät.example.org/app/#/invite/${CODE}`, 'host'],
    ['an empty host', `https:///app/#/invite/${CODE}`, 'host'],
    ['another path', `https://chat.example.org/other/#/invite/${CODE}`, 'path'],
    ['no path', `https://chat.example.org#/invite/${CODE}`, 'path'],
    ['a path under /app/', `https://chat.example.org/app/x/#/invite/${CODE}`, 'path'],
    ['/app without the slash', `https://chat.example.org/app#/invite/${CODE}`, 'path'],
    ['a query', `https://chat.example.org/app/?a=b#/invite/${CODE}`, 'query'],
    ['an empty query', `https://chat.example.org/app/?#/invite/${CODE}`, 'query'],
    ['a query before the path', `https://chat.example.org?a=b/app/#/invite/${CODE}`, 'path'],
    ['no fragment', 'https://chat.example.org/app/', 'fragment'],
    ['a sign-in link', `https://chat.example.org/app/#/signin/${CODE}`, 'fragment'],
    ['a setup link', `https://chat.example.org/app/#/setup/${CODE}`, 'fragment'],
    ['a channel link', 'https://chat.example.org/app/#/c/general', 'fragment'],
    ['an empty code', 'https://chat.example.org/app/#/invite/', 'code'],
    ['a code too long', link(ORIGIN_A, 'x'.repeat(257)), 'code'],
    ['a code with a slash', link(ORIGIN_A, 'a/b'), 'code'],
    ['a code with an encoded slash', link(ORIGIN_A, 'a%2Fb'), 'code'],
    ['a code with an encoded control character', link(ORIGIN_A, 'a%00b'), 'code'],
    ['a code with an encoded newline', link(ORIGIN_A, 'a%0Ab'), 'code'],
    ['a code with a bad escape', link(ORIGIN_A, 'a%zz'), 'code'],
    ['a code with a second fragment', link(ORIGIN_A, 'a#b'), 'code'],
    ['a code with a query', link(ORIGIN_A, 'a?b=c'), 'code'],
    ['a control character in the link', `${ORIGIN_A}/app/#/invite/a\u0007b`, 'characters'],
    ['a space inside the link', `${ORIGIN_A}/app/#/invite/a b`, 'characters'],
    ['a zero-width space in the code', `${ORIGIN_A}/app/#/invite/a​b`, 'characters'],
    ['a right-to-left override', `${ORIGIN_A}/app/#/invite/‮abc`, 'characters'],
    ['a zero-width joiner in the host', `https://chat‍.example.org/app/#/invite/${CODE}`, 'characters'],
    ['an over-long input', `${ORIGIN_A}/app/#/invite/${'x'.repeat(MAX_INVITE_LINK_LENGTH)}`, 'length'],
    ['an empty input', '', 'length'],
    ['only whitespace', '   ', 'length'],
    ['not a string', { link: 1 }, 'type'],
  ];

  it.each(refused)('refuses %s', (_name, input, refusal) => {
    expect(parseInviteLink(input)).toEqual({ ok: false, refusal });
  });

  it('never repeats the link or its code in a refusal', () => {
    for (const [, input] of refused) {
      if (typeof input !== 'string') continue;
      const result = parseInviteLink(input);
      expect(JSON.stringify(result)).not.toContain('CANARY');
      expect(JSON.stringify(result)).not.toContain('example');
    }
  });
});

// A real profiles service over in-memory parts: what the router sees in use.
async function setup(options: { voice?: boolean; probe?: 'ok' | 'unreachable' } = {}) {
  const files = memoryFiles();
  const store = new ProfileStore(files);
  const factory = fakeFactory();
  const log: string[] = [];
  const profiles = new ProfilesService({
    store,
    vault: fakeVault(),
    hooks: new TeardownHooks(),
    createActivation: factory.create,
    probe: () => Promise.resolve(options.probe ?? 'ok'),
    platform: 'linux',
    now: () => 0,
    log: (line) => log.push(line),
    compat: { servers: ['v1.0.0'], override: false },
    desktopVersion: '0.0.0',
    readHealth: () => Promise.resolve({ kind: 'answered', version: 'v1.0.0' }),
    verifyStored: () => Promise.resolve({ kind: 'rejected' }),
    revokeStored: () => Promise.resolve('ended'),
  });
  await profiles.start();
  const clock = { now: 1_000 };
  let n = 0;
  const invites = new Invites({
    profiles,
    voiceRunning: () => options.voice === true,
    now: () => clock.now,
    log: (line) => log.push(line),
    newHandle: () => `invite-${String(++n).padStart(32, '0')}`,
  });
  const add = async (origin: string): Promise<string> => {
    const added = await profiles.add(origin);
    if (!added.ok) throw new Error('add failed');
    return added.id;
  };
  return { profiles, store, files, factory, log, invites, clock, add };
}

type Ready = Awaited<ReturnType<typeof setup>>;

function checked(s: Ready, text: string) {
  const answer = s.invites.check(text);
  if (!answer.ok) throw new Error('check refused');
  return answer;
}

describe('checking a link', () => {
  it('answers the server it names and the profiles saved for it, and contacts and changes nothing', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    await s.add(ORIGIN_B);
    const opened = s.factory.made.length;
    const answer = checked(s, link(ORIGIN_A));
    expect(answer).toMatchObject({ ok: true, origin: ORIGIN_A, host: 'a.example', voice: false });
    expect(answer.profiles).toEqual([{ id: work, label: 'a.example', account: null, active: false }]);
    expect(s.factory.made).toHaveLength(opened);
    expect(s.profiles.view().active).toBeNull();
    // The answer holds a handle and no code.
    expect(JSON.stringify(answer)).not.toContain('CANARY');
  });

  it('offers every profile for the origin by name and account, and none for a server not saved', async () => {
    const s = await setup();
    const one = await s.add(ORIGIN_A);
    const two = await s.add(ORIGIN_A);
    expect(checked(s, link(ORIGIN_A)).profiles.map((p) => p.id)).toEqual([one, two]);
    expect(checked(s, link('https://c.example')).profiles).toEqual([]);
  });

  it('says when voice is running', async () => {
    const s = await setup({ voice: true });
    expect(checked(s, link(ORIGIN_A)).voice).toBe(true);
  });

  it('refuses a bad link with a reason, and logs the reason alone', async () => {
    const s = await setup();
    expect(s.invites.check(`http://chat.example.org/app/#/invite/${CODE}`)).toEqual({ ok: false, refusal: 'scheme' });
    expect(s.log).toContain('conch-desktop: refused an invite link (scheme)');
    expect(s.log.join('\n')).not.toContain('CANARY');
  });
});

describe('accepting', () => {
  it('switches to the chosen profile only on accept, and hands the code over once, to that profile', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const answer = checked(s, link(ORIGIN_A));
    expect(s.profiles.view().active).toBeNull();
    expect(s.invites.take()).toBeNull();
    expect(await s.invites.accept(answer.handle, { profile: work })).toEqual({ ok: true, switched: true });
    expect(s.profiles.view().active).toBe(work);
    expect(s.invites.take()).toBe(CODE);
    expect(s.invites.take()).toBeNull();
  });

  it('opens a link for the profile already in force without a switch', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    await s.profiles.switchTo(work);
    const opened = s.factory.made.length;
    const answer = checked(s, link(ORIGIN_A));
    expect(answer.profiles[0]?.active).toBe(true);
    expect(await s.invites.accept(answer.handle, { profile: work })).toEqual({ ok: true, switched: false });
    expect(s.factory.made).toHaveLength(opened);
    expect(s.invites.take()).toBe(CODE);
    expect(s.invites.take()).toBeNull();
  });

  it('adds the server only on accept, then switches to it', async () => {
    const s = await setup();
    const answer = checked(s, link(ORIGIN_A));
    expect(s.store.list()).toHaveLength(0);
    expect(await s.invites.accept(answer.handle, { add: true })).toEqual({ ok: true, switched: true });
    expect(s.store.list()).toHaveLength(1);
    expect(s.profiles.view().active).toBe(s.store.list()[0]?.id);
    expect(s.invites.take()).toBe(CODE);
  });

  it('can open a saved server’s link as a new entry beside it', async () => {
    const s = await setup();
    const old = await s.add(ORIGIN_A);
    await s.profiles.switchTo(old);
    const answer = checked(s, link(ORIGIN_A));
    expect(await s.invites.accept(answer.handle, { add: true })).toEqual({ ok: true, switched: true });
    expect(s.store.list()).toHaveLength(2);
    expect(s.profiles.view().active).not.toBe(old);
    expect(s.invites.take()).toBe(CODE);
  });

  it('adds nothing and stages nothing when the server cannot be reached', async () => {
    const s = await setup({ probe: 'unreachable' });
    const answer = checked(s, link(ORIGIN_A));
    expect(await s.invites.accept(answer.handle, { add: true })).toEqual({ ok: false, refusal: 'unreachable' });
    expect(s.store.list()).toHaveLength(0);
    expect(s.invites.take()).toBeNull();
  });

  it('refuses a profile that is not for the link’s server, and spends the handle', async () => {
    const s = await setup();
    const other = await s.add(ORIGIN_B);
    const answer = checked(s, link(ORIGIN_A));
    expect(await s.invites.accept(answer.handle, { profile: other })).toEqual({ ok: false, refusal: 'unknown-profile' });
    expect(s.profiles.view().active).toBeNull();
    expect(await s.invites.accept(answer.handle, { add: true })).toEqual({ ok: false, refusal: 'expired' });
    expect(s.store.list()).toHaveLength(1);
  });

  it('uses a handle once, and not one main did not issue', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const answer = checked(s, link(ORIGIN_A));
    expect(await s.invites.accept('invite-' + 'f'.repeat(32), { profile: work })).toEqual({ ok: false, refusal: 'expired' });
    expect(await s.invites.accept(answer.handle, { profile: work })).toMatchObject({ ok: true });
    expect(await s.invites.accept(answer.handle, { profile: work })).toEqual({ ok: false, refusal: 'expired' });
  });

  it('cancelling spends the handle and changes nothing', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const answer = checked(s, link(ORIGIN_A));
    expect(s.invites.cancel(answer.handle)).toBe(true);
    expect(await s.invites.accept(answer.handle, { profile: work })).toEqual({ ok: false, refusal: 'expired' });
    expect(s.profiles.view().active).toBeNull();
  });

  it('lets a checked link lapse, and a staged code lapse', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const stale = checked(s, link(ORIGIN_A));
    s.clock.now += PENDING_MS;
    expect(await s.invites.accept(stale.handle, { profile: work })).toEqual({ ok: false, refusal: 'expired' });
    const fresh = checked(s, link(ORIGIN_A));
    await s.invites.accept(fresh.handle, { profile: work });
    s.clock.now += STAGED_MS;
    expect(s.invites.take()).toBeNull();
  });

  it('gives the code to no profile but the one it was staged for', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const other = await s.add(ORIGIN_B);
    await s.profiles.switchTo(other);
    // Staged for work, but the one in force is other.
    const deps: InvitesDeps = {
      profiles: { findByOrigin: (o) => s.profiles.findByOrigin(o), view: () => s.profiles.view(), add: (a) => s.profiles.add(a), switchTo: () => Promise.resolve(true) },
      voiceRunning: () => false,
      now: () => s.clock.now,
      log: () => undefined,
    };
    const invites = new Invites(deps);
    const answer = invites.check(link(ORIGIN_A));
    if (!answer.ok) throw new Error('refused');
    expect(await invites.accept(answer.handle, { profile: work })).toMatchObject({ ok: true });
    expect(invites.take()).toBeNull();
    expect(invites.take()).toBeNull();
  });

  it('never writes the code to profiles.json or to a log', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const a = checked(s, link(ORIGIN_A));
    await s.invites.accept(a.handle, { profile: work });
    const b = checked(s, link(ORIGIN_B));
    await s.invites.accept(b.handle, { add: true });
    s.invites.check(`http://chat.example.org/app/#/invite/${CODE}`);
    s.invites.take();
    expect(s.files.text ?? '').not.toContain('CANARY');
    expect(s.files.text ?? '').not.toContain(CODE);
    expect(s.log.join('\n')).not.toContain('CANARY');
    expect(s.factory.log.join('\n')).not.toContain('CANARY');
  });
});
