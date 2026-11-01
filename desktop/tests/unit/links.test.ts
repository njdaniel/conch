import { EventEmitter } from 'node:events';

import { describe, expect, it, vi } from 'vitest';

import { Gestures } from '../../main/gestures.ts';
import { checkLink, Links, LINKS_PER_MINUTE, MAX_LINK_LENGTH, refusalMessage, type LinkRefusal } from '../../main/links.ts';

const ch = (code: number): string => String.fromCodePoint(code);

describe('checkLink', () => {
  const good: [string, string][] = [
    ['https://example.com/', 'https://example.com/'],
    ['http://example.com/a?b=c#d', 'http://example.com/a?b=c#d'],
    ['HTTPS://Example.COM', 'https://example.com/'],
    ['https://example.com:8443/x', 'https://example.com:8443/x'],
    ['https://[::1]/x', 'https://[::1]/x'],
    ['https://example.com/a%20b', 'https://example.com/a%20b'],
    ['https://example.com/' + 'a'.repeat(100), 'https://example.com/' + 'a'.repeat(100)],
  ];
  it.each(good)('accepts %s, as its canonical form', (input, canonical) => {
    expect(checkLink(input)).toEqual({ ok: true, url: canonical });
  });

  const hostile: [string, string, LinkRefusal][] = [
    ['file', 'file:///etc/passwd', 'scheme'],
    ['javascript', 'javascript:alert(1)', 'scheme'],
    ['data', 'data:text/html,<script>1</script>', 'scheme'],
    ['blob', 'blob:https://example.com/x', 'scheme'],
    ['the app scheme', 'conch-app://ui/', 'scheme'],
    ['a custom scheme', 'steam://run/1', 'scheme'],
    ['an executable scheme', 'ms-msdt:/id', 'scheme'],
    ['ssh', 'ssh://host/', 'scheme'],
    ['ftp', 'ftp://host/x', 'scheme'],
    ['mailto', 'mailto:a@b.c', 'scheme'],
    ['userinfo', 'https://user@example.com/', 'userinfo'],
    ['userinfo with password', 'https://user:pw@example.com/', 'userinfo'],
    ['an empty userinfo', 'https://:@example.com/', 'userinfo'],
    ['userinfo disguised as a host', 'https://example.com@evil.example/', 'userinfo'],
    ['backslash userinfo', 'https:\\\\example.com@evil.example/', 'userinfo'],
    ['a percent-encoded host', 'https://exa%6dple.com/', 'host'],
    ['a punycode host', 'https://xn--e1afmkfd.example/', 'host'],
    ['a punycode label', 'https://www.xn--e1afmkfd.example/', 'host'],
    ['a non-ASCII host', `https://${ch(0xe9)}xample.com/`, 'host'],
    ['a fullwidth host', `https://${ch(0xff45)}xample.com/`, 'host'],
    ['a zero-width space', `https://exam${ch(0x200b)}ple.com/`, 'characters'],
    ['a right-to-left override', `https://example.com/${ch(0x202e)}gpj.exe`, 'characters'],
    ['a soft hyphen', `https://exam${ch(0xad)}ple.com/`, 'characters'],
    ['a newline', 'https://example.com/\nx', 'characters'],
    ['a tab', 'https://exam\tple.com/', 'characters'],
    ['a NUL', 'https://example.com/\u0000', 'characters'],
    ['a space', 'https://example.com/a b', 'characters'],
    ['a tag character', 'https://example.com/\u{e0041}', 'characters'],
    ['no host', 'https://', 'malformed'],
    ['relative', '/x', 'malformed'],
    ['nothing', '', 'length'],
    ['too long', 'https://example.com/' + 'a'.repeat(MAX_LINK_LENGTH), 'length'],
  ];
  it.each(hostile)('refuses %s', (_name, input, refusal) => {
    expect(checkLink(input)).toEqual({ ok: false, refusal });
  });

  it.each([null, undefined, 1, {}, ['https://example.com/'], true])('refuses a non-string (%j)', (input) => {
    expect(checkLink(input)).toEqual({ ok: false, refusal: 'type' });
  });

  it('says why in words that do not repeat the link', () => {
    for (const refusal of ['type', 'length', 'characters', 'malformed', 'scheme', 'userinfo', 'host'] as const) {
      expect(refusalMessage(refusal)).not.toMatch(/https?:\/\/[a-z]/u);
    }
  });
});

describe('Links.fromWindow', () => {
  function setup(input: boolean) {
    const win = {};
    const other = {};
    const opened: string[] = [];
    const told: string[] = [];
    const logs: string[] = [];
    const links = new Links({
      gestures: { take: (c: object | null) => (input && c === win ? 'pointer' : null) } as unknown as Gestures,
      confirm: () => Promise.resolve(true),
      owned: () => win,
      openExternal: (url) => {
        opened.push(url);
        return Promise.resolve();
      },
      tell: (m) => told.push(m),
      log: (l) => logs.push(l),
    });
    return { links, win, other, opened, told, logs };
  }

  it('opens a valid link the person just clicked, in the OS browser, canonicalised', () => {
    const { links, win, opened, told } = setup(true);
    expect(links.fromWindow(win, 'HTTPS://Example.com', false)).toBe('opened');
    expect(opened).toEqual(['https://example.com/']);
    expect(told).toEqual([]);
  });

  it('refuses a hostile link the person clicked, with a visible message, and opens nothing', () => {
    const { links, win, opened, told } = setup(true);
    for (const url of ['file:///etc/passwd', 'javascript:alert(1)', 'conch-app://ui/', 'https://u:p@example.com/']) {
      expect(links.fromWindow(win, url, false)).toBe('refused');
    }
    expect(opened).toEqual([]);
    expect(told).toHaveLength(4);
  });

  it('ignores a window.open with no user gesture, and says nothing', () => {
    const { links, win, opened, told, logs } = setup(false);
    expect(links.fromWindow(win, 'https://example.com/', false)).toBe('ignored');
    expect(opened).toEqual([]);
    expect(told).toEqual([]);
    expect(logs).toEqual(['conch-desktop: refused a new window (no user gesture)']);
  });

  it('ignores a form submission to a new window, even after a click', () => {
    const { links, win, opened } = setup(true);
    expect(links.fromWindow(win, 'https://example.com/', true)).toBe('ignored');
    expect(opened).toEqual([]);
  });

  it('ignores a request from any contents but the app window', () => {
    const { links, other, opened } = setup(true);
    expect(links.fromWindow(other, 'https://example.com/', false)).toBe('ignored');
    expect(opened).toEqual([]);
  });

  it('logs a failure to open without the link', async () => {
    const logs: string[] = [];
    const win = {};
    const links = new Links({
      gestures: { take: () => 'pointer' } as unknown as Gestures,
      confirm: () => Promise.resolve(true),
      owned: () => win,
      openExternal: () => Promise.reject(new Error('xdg-open https://example.com/secret failed')),
      tell: vi.fn(),
      log: (l) => logs.push(l),
    });
    links.fromWindow(win, 'https://example.com/secret', false);
    await new Promise((r) => setTimeout(r, 0));
    expect(logs).toEqual(['conch-desktop: the system could not open a link']);
  });
});

describe('Links.fromWindow with the real gestures', () => {
  function real(replies: boolean[] = []) {
    const clock = { now: 1_000 };
    const gestures = new Gestures(() => clock.now);
    const win = new EventEmitter();
    gestures.attach(win);
    const opened: string[] = [];
    const told: string[] = [];
    const asked: [string, string][] = [];
    const answer = (n: number): Promise<boolean> => (replies.length > 0 ? Promise.resolve(replies[Math.min(n, replies.length) - 1] ?? false) : Promise.resolve(true));
    const links = new Links({
      gestures,
      confirm: (host, url) => {
        asked.push([host, url]);
        return answer(asked.length);
      },
      owned: () => win,
      openExternal: (url) => {
        opened.push(url);
        return Promise.resolve();
      },
      tell: (m) => told.push(m),
      log: () => undefined,
      now: () => clock.now,
    });
    const click = () => win.emit('input-event', {}, { type: 'mouseUp' });
    const enter = () => win.emit('input-event', {}, { type: 'rawKeyDown', key: 'Enter', code: 'Enter', isAutoRepeat: false, modifiers: [] });
    return { links, win, opened, told, asked, click, enter, clock };
  }

  it('one click opens one link, not two: a replay of window.open is ignored', () => {
    const t = real();
    t.click();
    expect(t.links.fromWindow(t.win, 'https://example.com/a', false)).toBe('opened');
    expect(t.links.fromWindow(t.win, 'https://example.com/b', false)).toBe('ignored');
    expect(t.links.fromWindow(t.win, 'https://example.com/c', false)).toBe('ignored');
    expect(t.opened).toEqual(['https://example.com/a']);
    t.click();
    expect(t.links.fromWindow(t.win, 'https://example.com/d', false)).toBe('opened');
    expect(t.opened).toEqual(['https://example.com/a', 'https://example.com/d']);
  });

  it('typing a letter does not arm a link, whatever the window.open that follows', () => {
    const t = real();
    for (const [key, code] of [['h', 'KeyH'], ['i', 'KeyI'], [' ', 'Space']] as const) {
      t.win.emit('input-event', {}, { type: 'char', key, code, isAutoRepeat: false, modifiers: [] });
      t.win.emit('input-event', {}, { type: 'keyUp', key, code, isAutoRepeat: false, modifiers: [] });
    }
    t.win.emit('input-event', {}, { type: 'rawKeyDown', key: 'h', code: 'KeyH', isAutoRepeat: false, modifiers: [] });
    expect(t.links.fromWindow(t.win, 'https://example.com/', false)).toBe('ignored');
    expect(t.opened).toEqual([]);
  });

  const flush = () => new Promise((r) => setTimeout(r, 0));

  it('a pointer-armed link opens without asking', () => {
    const t = real();
    t.click();
    expect(t.links.fromWindow(t.win, 'https://example.com/', false)).toBe('opened');
    expect(t.asked).toEqual([]);
    expect(t.opened).toEqual(['https://example.com/']);
  });

  it('a key-armed link shows the host and the address first, and opens only on Open', async () => {
    const t = real([true]);
    t.enter();
    expect(t.links.fromWindow(t.win, 'https://Example.com:8443/a?b=c', false)).toBe('confirming');
    expect(t.opened).toEqual([]);
    await flush();
    expect(t.asked).toEqual([['example.com:8443', 'https://example.com:8443/a?b=c']]);
    expect(t.opened).toEqual(['https://example.com:8443/a?b=c']);
  });

  it('Cancel opens nothing, and the gesture is spent', async () => {
    const t = real([false]);
    t.enter();
    expect(t.links.fromWindow(t.win, 'https://example.com/', false)).toBe('confirming');
    await flush();
    expect(t.opened).toEqual([]);
    expect(t.links.fromWindow(t.win, 'https://example.com/', false)).toBe('ignored');
    await flush();
    expect(t.asked).toHaveLength(1);
    expect(t.opened).toEqual([]);
  });

  it('asks nothing about a link that fails the checks, and tells of it as before', () => {
    const t = real();
    t.enter();
    expect(t.links.fromWindow(t.win, 'https://u:p@example.com/', false)).toBe('refused');
    expect(t.asked).toEqual([]);
    expect(t.told).toHaveLength(1);
  });

  it('a dialog that cannot be shown opens nothing', async () => {
    const win = new EventEmitter();
    const gestures = new Gestures(() => 1);
    gestures.attach(win);
    const opened: string[] = [];
    const links = new Links({
      gestures,
      owned: () => win,
      openExternal: (u) => (opened.push(u), Promise.resolve()),
      confirm: () => Promise.reject(new Error('no display')),
      tell: vi.fn(),
      log: () => undefined,
    });
    win.emit('input-event', {}, { type: 'rawKeyDown', key: 'Enter', code: 'Enter', isAutoRepeat: false, modifiers: [] });
    expect(links.fromWindow(win, 'https://example.com/', false)).toBe('confirming');
    await flush();
    expect(opened).toEqual([]);
  });

  it('Enter then a script replay: one confirmation, not two', async () => {
    const t = real([true]);
    t.enter();
    expect(t.links.fromWindow(t.win, 'https://example.com/a', false)).toBe('confirming');
    expect(t.links.fromWindow(t.win, 'https://example.com/b', false)).toBe('ignored');
    await flush();
    expect(t.asked).toHaveLength(1);
  });

  it('a request that fails a check still uses the act up', () => {
    const t = real();
    t.click();
    expect(t.links.fromWindow(t.win, 'https://example.com/', true)).toBe('ignored');
    expect(t.links.fromWindow(t.win, 'https://example.com/', false)).toBe('ignored');
  });

  it('is limited to ten a minute, so refused links cannot make a storm of message boxes', () => {
    const t = real();
    for (let i = 0; i < LINKS_PER_MINUTE + 5; i++) {
      t.click();
      t.links.fromWindow(t.win, 'file:///etc/passwd', false);
    }
    // The ten refusals each told; the first one over the cap told once more.
    expect(t.told).toHaveLength(LINKS_PER_MINUTE + 1);
    expect(t.told.at(-1)).toBe('Too many links were opened at once. Try again in a minute.');
    t.clock.now += 60_000;
    t.click();
    expect(t.links.fromWindow(t.win, 'https://example.com/', false)).toBe('opened');
  });
});
