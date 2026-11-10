import { afterEach, describe, expect, it, vi } from 'vitest';

import { deliverInvite, Invites, MAX_INVITE_LINK_LENGTH, parseInviteLink, parseSchemeLink, PENDING_MS, schemeLinkFrom, STAGED_MS, SWAP_GUARD_MS, type InvitesDeps } from '../../main/invites.ts';
import { INVITE_ARRIVED, INVITE_EVENTS } from '../../shared/bridge-names.ts';
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
    // A default port written out is dropped by the canonical form; that is fine.
    expect(parseInviteLink('http://localhost:80/app/#/invite/abc')).toMatchObject({ ok: true, origin: 'http://localhost' });
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
    ['a right-to-left override', `${ORIGIN_A}/app/#/invite/\u202eabc`, 'characters'],
    ['a zero-width joiner in the host', `https://chat‍.example.org/app/#/invite/${CODE}`, 'characters'],
    ['a decimal IPv4 spelling', `http://2130706433/app/#/invite/${CODE}`, 'host'],
    ['a hex dotted IPv4 spelling', `http://0x7f.0.0.1/app/#/invite/${CODE}`, 'host'],
    ['an octal IPv4 spelling', `http://017700000001/app/#/invite/${CODE}`, 'host'],
    ['a short IPv4 spelling', `http://127.1/app/#/invite/${CODE}`, 'host'],
    ['a hex IPv4 spelling over https', `https://0x7f000001/app/#/invite/${CODE}`, 'host'],
    ['a trailing dot', `https://chat.example.org./app/#/invite/${CODE}`, 'host'],
    ['a host of dots', `https://../app/#/invite/${CODE}`, 'host'],
    ['a lone hyphen host', `https://-/app/#/invite/${CODE}`, 'host'],
    ['an empty label', `https://a..b/app/#/invite/${CODE}`, 'host'],
    ['port 0', `https://chat.example.org:0/app/#/invite/${CODE}`, 'host'],
    ['a host over 253 characters', `https://${Array.from({ length: 5 }, () => 'a'.repeat(60)).join('.')}/app/#/invite/${CODE}`, 'host'],
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

describe('voice that started after the check', () => {
  it('refuses a switch the check did not warn about, and spends the handle and stages nothing', async () => {
    let voice = false;
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const invites = new Invites({
      profiles: { findByOrigin: (o) => s.profiles.findByOrigin(o), view: () => s.profiles.view(), add: (a) => s.profiles.add(a), switchTo: (id) => s.profiles.switchTo(id) },
      voiceRunning: () => voice,
      now: () => s.clock.now,
      log: () => undefined,
    });
    const answer = invites.check(link(ORIGIN_A));
    if (!answer.ok) throw new Error('refused');
    expect(answer.voice).toBe(false);
    voice = true;
    expect(await invites.accept(answer.handle, { profile: work })).toEqual({ ok: false, refusal: 'voice-started' });
    expect(s.profiles.view().active).toBeNull();
    expect(invites.take()).toBeNull();
    expect(await invites.accept(answer.handle, { profile: work })).toEqual({ ok: false, refusal: 'expired' });
    // Checked again with voice running, the same click goes through.
    const again = invites.check(link(ORIGIN_A));
    if (!again.ok) throw new Error('refused');
    expect(again.voice).toBe(true);
    expect(await invites.accept(again.handle, { profile: work })).toEqual({ ok: true, switched: true });
  });

  it('refuses adding a server the same way, before anything is added', async () => {
    let voice = false;
    const s = await setup();
    const invites = new Invites({
      profiles: { findByOrigin: (o) => s.profiles.findByOrigin(o), view: () => s.profiles.view(), add: (a) => s.profiles.add(a), switchTo: (id) => s.profiles.switchTo(id) },
      voiceRunning: () => voice,
      now: () => s.clock.now,
      log: () => undefined,
    });
    const answer = invites.check(link(ORIGIN_A));
    if (!answer.ok) throw new Error('refused');
    voice = true;
    expect(await invites.accept(answer.handle, { add: true })).toEqual({ ok: false, refusal: 'voice-started' });
    expect(s.store.list()).toHaveLength(0);
  });
});

describe('expiry by timer', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('drops a checked link at its time though nothing else happens', async () => {
    vi.useFakeTimers();
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const answer = checked(s, link(ORIGIN_A));
    // The injected clock does not move: only the timer can have dropped it.
    vi.advanceTimersByTime(PENDING_MS);
    expect(await s.invites.accept(answer.handle, { profile: work })).toEqual({ ok: false, refusal: 'expired' });
  });

  it('drops a staged code at its time though nothing else happens', async () => {
    vi.useFakeTimers();
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const answer = checked(s, link(ORIGIN_A));
    await s.invites.accept(answer.handle, { profile: work });
    vi.advanceTimersByTime(STAGED_MS);
    expect(s.invites.take()).toBeNull();
  });

  it('leaves no timer behind for a spent handle or a taken code', async () => {
    vi.useFakeTimers();
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    checked(s, link(ORIGIN_A));
    const answer = checked(s, link(ORIGIN_A));
    await s.invites.accept(answer.handle, { profile: work });
    // Only the first, unspent link's timer is left.
    s.invites.take();
    expect(vi.getTimerCount()).toBe(1);
  });

  it('clears the staged code when the switch throws', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const invites = new Invites({
      profiles: { findByOrigin: (o) => s.profiles.findByOrigin(o), view: () => s.profiles.view(), add: (a) => s.profiles.add(a), switchTo: () => Promise.reject(new Error('boom')) },
      voiceRunning: () => false,
      now: () => s.clock.now,
      log: () => undefined,
    });
    const answer = invites.check(link(ORIGIN_A));
    if (!answer.ok) throw new Error('refused');
    await expect(invites.accept(answer.handle, { profile: work })).rejects.toThrow('boom');
    await s.profiles.switchTo(work);
    expect(invites.take()).toBeNull();
  });
});

// The conch:// scheme (issue #1592, D5-06): conch://invite?server=<percent-
// encoded origin>&code=<code>.
const scheme = (origin: string, code = CODE): string => `conch://invite?server=${encodeURIComponent(origin)}&code=${encodeURIComponent(code)}`;

describe('parseSchemeLink', () => {
  it('accepts the documented shape and gives what parseInviteLink gives for the https link it names', () => {
    expect(parseSchemeLink(scheme(ORIGIN_A))).toEqual({ ok: true, origin: ORIGIN_A, code: CODE });
    expect(parseSchemeLink(scheme(ORIGIN_A))).toEqual(parseInviteLink(link(ORIGIN_A)));
    // Either order; the scheme in any case; a port and loopback over http.
    expect(parseSchemeLink(`conch://invite?code=abc&server=${encodeURIComponent(ORIGIN_A)}`)).toEqual({ ok: true, origin: ORIGIN_A, code: 'abc' });
    expect(parseSchemeLink(`CONCH://invite?server=${encodeURIComponent(ORIGIN_A)}&code=abc`)).toMatchObject({ ok: true });
    expect(parseSchemeLink(scheme('https://chat.example.org:8443', 'abc'))).toMatchObject({ ok: true, origin: 'https://chat.example.org:8443' });
    expect(parseSchemeLink(scheme('http://localhost:8080', 'abc'))).toMatchObject({ ok: true, origin: 'http://localhost:8080' });
    expect(parseSchemeLink(scheme('http://[::1]:3000', 'abc'))).toMatchObject({ ok: true, origin: 'http://[::1]:3000' });
    expect(parseSchemeLink(scheme(ORIGIN_A, 'x'.repeat(256)))).toMatchObject({ ok: true });
    // A code that reads like a switch is only a code: it is a value inside one
    // argument that starts with "conch:", and nothing here runs it.
    expect(parseSchemeLink(scheme(ORIGIN_A, '--inspect'))).toEqual({ ok: true, origin: ORIGIN_A, code: '--inspect' });
  });

  const refused: [string, unknown, string][] = [
    ['a number', 42, 'type'],
    ['undefined', undefined, 'type'],
    ['an empty string', '', 'length'],
    ['too long', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=${'x'.repeat(MAX_INVITE_LINK_LENGTH)}`, 'length'],
    ['an https link', link(ORIGIN_A), 'scheme'],
    ['another scheme', `javascript:alert('${CODE}')`, 'scheme'],
    ['a scheme that only starts with conch', `conchx://invite?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}`, 'scheme'],
    ['a switch that is not a URL', '--inspect', 'scheme'],
    ['a switch that carries one', `--url=${scheme(ORIGIN_A)}`, 'scheme'],
    ['a host that reads as a switch', 'conch://--inspect', 'malformed'],
    ['a host that reads as a switch, with a query', `conch://--inspect=9229?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}`, 'malformed'],
    ['no slashes', `conch:invite?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}`, 'malformed'],
    ['another host', `conch://join?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}`, 'malformed'],
    ['the host in capitals', `conch://INVITE?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}`, 'malformed'],
    ['userinfo before the host', `conch://u:p@invite?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}`, 'malformed'],
    ['a path after the host', `conch://invite/x?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}`, 'malformed'],
    ['a slash before the query', `conch://invite/?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}`, 'malformed'],
    ['no query', 'conch://invite', 'malformed'],
    ['an empty query', 'conch://invite?', 'malformed'],
    ['a fragment', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}#x`, 'fragment'],
    ['only the server', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}`, 'malformed'],
    ['only the code', `conch://invite?code=${CODE}`, 'malformed'],
    ['a third parameter', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=${CODE}&x=1`, 'malformed'],
    ['a repeated server', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&server=${encodeURIComponent(ORIGIN_B)}`, 'malformed'],
    ['a repeated code', `conch://invite?code=${CODE}&code=other`, 'malformed'],
    ['an unknown parameter', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&token=${CODE}`, 'malformed'],
    ['parameter names in capitals', `conch://invite?SERVER=${encodeURIComponent(ORIGIN_A)}&CODE=${CODE}`, 'malformed'],
    ['a semicolon separator', `conch://invite?server=${encodeURIComponent(ORIGIN_A)};code=${CODE}`, 'malformed'],
    ['a parameter with no value sign', `conch://invite?server&code=${CODE}`, 'malformed'],
    ['a value with a second equals sign', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a=b`, 'malformed'],
    ['an empty server', `conch://invite?server=&code=${CODE}`, 'host'],
    ['an empty code', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=`, 'code'],
    ['a server that is not percent-encoded', `conch://invite?server=${ORIGIN_A}&code=${CODE}`, 'host'],
    ['a plus for a space', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a+b`, 'code'],
    ['a server with a path', `conch://invite?server=${encodeURIComponent(`${ORIGIN_A}/app/`)}&code=${CODE}`, 'host'],
    ['a server with a query', `conch://invite?server=${encodeURIComponent(`${ORIGIN_A}?x=1`)}&code=${CODE}`, 'host'],
    ['a server with a fragment', `conch://invite?server=${encodeURIComponent(`${ORIGIN_A}#x`)}&code=${CODE}`, 'host'],
    ['a server with userinfo', `conch://invite?server=${encodeURIComponent('https://nick:pw@chat.example.org')}&code=${CODE}`, 'host'],
    ['a server that hides a host behind userinfo', `conch://invite?server=${encodeURIComponent('https://chat.example.org@evil.example')}&code=${CODE}`, 'host'],
    ['a server with a backslash', `conch://invite?server=${encodeURIComponent('https://chat.example.org\\@evil.example')}&code=${CODE}`, 'host'],
    ['a server over http beyond loopback', `conch://invite?server=${encodeURIComponent('http://chat.example.org')}&code=${CODE}`, 'scheme'],
    ['a server on another scheme', `conch://invite?server=${encodeURIComponent('ftp://chat.example.org')}&code=${CODE}`, 'host'],
    ['a server that is a switch', `conch://invite?server=${encodeURIComponent('--inspect')}&code=${CODE}`, 'host'],
    ['a punycode host', `conch://invite?server=${encodeURIComponent('https://xn--80ak6aa92e.example')}&code=${CODE}`, 'host'],
    ['a double-encoded host', `conch://invite?server=${encodeURIComponent(encodeURIComponent(ORIGIN_A))}&code=${CODE}`, 'host'],
    ['an escaped dot in the host', `conch://invite?server=${encodeURIComponent('https://chat%2eexample.org')}&code=${CODE}`, 'host'],
    ['a non-ASCII host, as written', `conch://invite?server=${encodeURIComponent('https://chät.example.org')}&code=${CODE}`, 'host'],
    ['a decimal-integer loopback', `conch://invite?server=${encodeURIComponent('http://2130706433')}&code=${CODE}`, 'host'],
    ['a short-form loopback', `conch://invite?server=${encodeURIComponent('http://127.1')}&code=${CODE}`, 'host'],
    ['a trailing dot', `conch://invite?server=${encodeURIComponent('https://chat.example.org.')}&code=${CODE}`, 'host'],
    ['port zero', `conch://invite?server=${encodeURIComponent('https://chat.example.org:0')}&code=${CODE}`, 'host'],
    ['a code with an encoded slash', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a%2Fb`, 'code'],
    ['a code with an encoded control character', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a%00b`, 'code'],
    ['a code with an encoded newline', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a%0Ab`, 'code'],
    ['a code with an encoded invisible character', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a%E2%80%8Bb`, 'code'],
    ['a double-encoded code', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a%2541`, 'code'],
    ['a broken escape', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a%E0%A4%A`, 'malformed'],
    ['a code too long', scheme(ORIGIN_A, 'x'.repeat(257)), 'code'],
    ['a raw space', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a b`, 'characters'],
    ['a trailing newline', `${scheme(ORIGIN_A)}\n`, 'characters'],
    ['a leading space', ` ${scheme(ORIGIN_A)}`, 'characters'],
    ['a raw control character', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a\u0000b`, 'characters'],
    ['a raw invisible character', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a\u200Bb`, 'characters'],
    ['a raw bidi override', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a\u202Eb`, 'characters'],
    ['a backslash', `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a\\b`, 'characters'],
  ];

  it.each(refused)('refuses %s', (_name, input, refusal) => {
    expect(parseSchemeLink(input)).toEqual({ ok: false, refusal });
  });

  it('never repeats the link, the code or the server in a refusal', () => {
    for (const [, input] of refused) {
      const result = JSON.stringify(parseSchemeLink(input));
      expect(result).not.toContain('CANARY');
      expect(result).not.toContain('example');
    }
  });
});

describe('schemeLinkFrom', () => {
  const good = scheme(ORIGIN_A);
  const exe = '/opt/conch-desktop/conch-desktop';

  it('picks the first argument of the scheme, after the executable', () => {
    expect(schemeLinkFrom([exe, good])).toBe(good);
    expect(schemeLinkFrom([exe, '--some-flag', good])).toBe(good);
    expect(schemeLinkFrom([exe, 'CONCH://anything'])).toBe('CONCH://anything');
    // Index 0 is the executable, whatever it looks like.
    expect(schemeLinkFrom([good])).toBeNull();
  });

  it('considers only the first, whether or not it parses', () => {
    const second = scheme(ORIGIN_B);
    expect(schemeLinkFrom([exe, good, second])).toBe(good);
    expect(schemeLinkFrom([exe, 'conch://not-an-invite', good])).toBe('conch://not-an-invite');
  });

  it('never reads a switch as a link, however it continues', () => {
    for (const arg of ['--inspect', '--inspect=9229', '--remote-debugging-port=1', `--url=${good}`, `-${good}`, '-conch://invite', '--', `--${good}`]) {
      expect(schemeLinkFrom([exe, arg]), arg).toBeNull();
      expect(schemeLinkFrom([exe, arg, good]), arg).toBe(good);
    }
    // A host that reads as a switch is a link to be refused, not a switch to be run.
    expect(schemeLinkFrom([exe, 'conch://--inspect'])).toBe('conch://--inspect');
    expect(parseSchemeLink('conch://--inspect')).toMatchObject({ ok: false });
  });

  it('ignores everything that is not a string argument of the scheme', () => {
    for (const argv of [undefined, null, 'conch://invite', 42, {}, [], [exe], [exe, 7, null, undefined, {}], [exe, '', ' conch://invite', 'https://x/app/#/invite/a', 'xconch://invite', 'conch', 'conch:']]) {
      const picked = schemeLinkFrom(argv);
      expect(picked === null || picked === 'conch:', JSON.stringify(argv)).toBe(true);
    }
    expect(schemeLinkFrom([exe, ' conch://invite'])).toBeNull();
  });

  it('looks at a bounded number of arguments', () => {
    const argv = [exe, ...Array.from({ length: 200 }, () => '--x'), good];
    expect(schemeLinkFrom(argv)).toBeNull();
  });
});

describe('a scheme link reaching the router', () => {
  const exe = '/opt/conch-desktop/conch-desktop';
  afterEach(() => {
    vi.useRealTimers();
  });

  it('is kept without a handle, a profile change or any contact until the page asks, then answers as a pasted link does', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const switched = vi.spyOn(s.profiles, 'switchTo');
    const added = vi.spyOn(s.profiles, 'add');
    expect(s.invites.arrive([exe, scheme(ORIGIN_A)])).toBe(true);
    expect(switched).not.toHaveBeenCalled();
    expect(added).not.toHaveBeenCalled();
    const answer = s.invites.arrived();
    if (answer === null || !answer.ok) throw new Error('nothing arrived');
    expect(answer).toMatchObject({ origin: ORIGIN_A, host: new URL(ORIGIN_A).host, voice: false });
    expect(answer.profiles.map((p) => p.id)).toEqual([work]);
    expect(JSON.stringify(answer)).not.toContain('CANARY');
    // Asking again gets nothing: it was taken.
    expect(s.invites.arrived()).toBeNull();
    // Nothing joined or switched yet; the click is accept, with its checks.
    expect(switched).not.toHaveBeenCalled();
    // The click comes after the swap guard (SWAP_GUARD_MS).
    s.clock.now += SWAP_GUARD_MS;
    expect(await s.invites.accept(answer.handle, { profile: work })).toMatchObject({ ok: true });
    expect(s.invites.take()).toBe(CODE);
  });

  // Security review S1: a newer link replaces the choice in place; a click
  // aimed at the old one must not act on the new one.
  it('refuses a click on a handle shown less than SWAP_GUARD_MS ago, keeps it, and accepts it after', async () => {
    const s = await setup();
    await s.add(ORIGIN_A);
    const other = await s.add(ORIGIN_B);
    const switched = vi.spyOn(s.profiles, 'switchTo');
    s.invites.arrive([exe, scheme(ORIGIN_A, 'first')]);
    const first = s.invites.arrived();
    if (first === null || !first.ok) throw new Error('nothing arrived');
    s.clock.now += 5_000;
    // A second link swaps the choice under the pointer.
    s.invites.arrive([exe, scheme(ORIGIN_B, 'second')]);
    const second = s.invites.arrived();
    if (second === null || !second.ok) throw new Error('nothing arrived');
    s.clock.now += SWAP_GUARD_MS - 1;
    expect(await s.invites.accept(second.handle, { add: true })).toEqual({ ok: false, refusal: 'just-arrived' });
    expect(switched).not.toHaveBeenCalled();
    expect(s.invites.take()).toBeNull();
    // Refused, not spent: the same handle works once the interval has passed.
    s.clock.now += 1;
    expect(await s.invites.accept(second.handle, { profile: other })).toMatchObject({ ok: true });
  });

  it('applies the guard to a first arrival too, and not to a pasted link', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    s.invites.arrive([exe, scheme(ORIGIN_A)]);
    const arrived = s.invites.arrived();
    if (arrived === null || !arrived.ok) throw new Error('nothing arrived');
    expect(await s.invites.accept(arrived.handle, { profile: work })).toEqual({ ok: false, refusal: 'just-arrived' });
    const pasted = s.invites.check(link(ORIGIN_A));
    if (!pasted.ok) throw new Error('refused');
    expect(await s.invites.accept(pasted.handle, { profile: work })).toMatchObject({ ok: true });
  });

  it('is held to the voice check at the click, as a pasted link is', async () => {
    const s = await setup();
    const work = await s.add(ORIGIN_A);
    const other = await s.add(ORIGIN_B);
    await s.profiles.switchTo(other);
    let voice = false;
    const invites = new Invites({
      profiles: { findByOrigin: (o) => s.profiles.findByOrigin(o), view: () => s.profiles.view(), add: (a) => s.profiles.add(a), switchTo: (id) => s.profiles.switchTo(id) },
      voiceRunning: () => voice,
      now: () => s.clock.now,
      log: () => undefined,
      newHandle: () => `invite-${'1'.repeat(32)}`,
    });
    expect(invites.arrive([exe, scheme(ORIGIN_A)])).toBe(true);
    const answer = invites.arrived();
    if (answer === null || !answer.ok) throw new Error('nothing arrived');
    voice = true;
    s.clock.now += SWAP_GUARD_MS;
    expect(await invites.accept(answer.handle, { profile: work })).toEqual({ ok: false, refusal: 'voice-started' });
    expect(invites.take()).toBeNull();
  });

  it('is refused by the same host rules as a pasted link, and logs the reason alone', async () => {
    const s = await setup();
    const hostile = `conch://invite?server=${encodeURIComponent('http://2130706433')}&code=${CODE}`;
    expect(s.invites.arrive([exe, hostile])).toBe(false);
    expect(s.invites.arrived()).toBeNull();
    expect(s.log.at(-1)).toBe('conch-desktop: refused a conch: link (host)');
  });

  it('ignores arguments that are not a link, silently, and reads no switch as one', async () => {
    const s = await setup();
    const before = s.log.length;
    for (const argv of [[exe], [exe, '--inspect', '--user-data-dir=/tmp/x'], [exe, `--url=${scheme(ORIGIN_A)}`], [exe, 'https://chat.example.org/app/#/invite/abc'], 'conch://invite', undefined]) {
      expect(s.invites.arrive(argv)).toBe(false);
    }
    expect(s.invites.arrived()).toBeNull();
    expect(s.log).toHaveLength(before);
  });

  it('considers only the first link; a malformed first is not made up for by a good second', async () => {
    const s = await setup();
    expect(s.invites.arrive([exe, 'conch://invite?server=x', scheme(ORIGIN_A)])).toBe(false);
    expect(s.invites.arrived()).toBeNull();
    expect(s.invites.arrive([exe, scheme(ORIGIN_A, 'first'), scheme(ORIGIN_B, 'second')])).toBe(true);
    expect(s.invites.arrived()).toMatchObject({ ok: true, origin: ORIGIN_A });
  });

  it('keeps one link: a newer one replaces an older', async () => {
    const s = await setup();
    s.invites.arrive([exe, scheme(ORIGIN_A, 'first')]);
    s.invites.arrive([exe, scheme(ORIGIN_B, 'second')]);
    expect(s.invites.arrived()).toMatchObject({ ok: true, origin: ORIGIN_B });
    expect(s.invites.arrived()).toBeNull();
  });

  it('lets a kept link lapse at its time though nothing else happens, and leaves no timer when taken', async () => {
    vi.useFakeTimers();
    const s = await setup();
    s.invites.arrive([exe, scheme(ORIGIN_A)]);
    vi.advanceTimersByTime(PENDING_MS);
    expect(s.invites.arrived()).toBeNull();
    s.invites.arrive([exe, scheme(ORIGIN_A)]);
    s.invites.arrived();
    // The checked link's own timer remains; the arrival's is gone.
    expect(vi.getTimerCount()).toBe(1);
  });

  it('never logs the argument, the code or the origin', async () => {
    const s = await setup();
    s.invites.arrive([exe, scheme(ORIGIN_A)]);
    s.invites.arrive([exe, `conch://invite?server=${encodeURIComponent(ORIGIN_A)}&code=a%2Fb${CODE}`]);
    s.invites.arrive([exe, `conch://invite?${CODE}`]);
    const all = s.log.join('\n');
    expect(all).not.toContain('CANARY');
    expect(all).not.toContain('example');
    expect(all).not.toContain('conch://');
  });
});

describe('deliverInvite', () => {
  const gate = (shows: boolean) => ({ generation: 1, shows: () => shows }) as never;

  it('tells the owned page that a link is waiting, with nothing of the link, while its document is committed', () => {
    const send = vi.fn();
    deliverInvite({ contents: () => ({ send }), gate: () => gate(true) });
    expect(send).toHaveBeenCalledExactlyOnceWith(INVITE_EVENTS, INVITE_ARRIVED);
    expect(JSON.stringify(send.mock.calls)).toBe(JSON.stringify([[INVITE_EVENTS, { type: 'arrived' }]]));
  });

  it('tells nobody when there is no window or the document is not committed', () => {
    const send = vi.fn();
    deliverInvite({ contents: () => null, gate: () => gate(true) });
    deliverInvite({ contents: () => ({ send }), gate: () => gate(false) });
    expect(send).not.toHaveBeenCalled();
  });
});
