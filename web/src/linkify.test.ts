import { describe, expect, it } from 'vitest';
import { linkify, maxLink, type Segment } from './linkify';

const t = (text: string): Segment => ({ kind: 'text', text });
const l = (text: string, href = text): Segment => ({ kind: 'link', text, href });

describe('linkify', () => {
  const cases: readonly { name: string; body: string; want: Segment[] }[] = [
    { name: 'empty', body: '', want: [] },
    { name: 'plain text', body: 'nothing here', want: [t('nothing here')] },
    { name: 'full stop outside', body: 'see https://example.com/a?b=1.', want: [t('see '), l('https://example.com/a?b=1'), t('.')] },
    { name: 'comma, bang and question', body: 'http://example.com/x, https://example.com/y! https://example.com/z?', want: [l('http://example.com/x'), t(', '), l('https://example.com/y'), t('! '), l('https://example.com/z'), t('?')] },
    { name: 'unbalanced paren outside', body: '(see https://example.com/a)', want: [t('(see '), l('https://example.com/a'), t(')')] },
    { name: 'balanced paren inside', body: 'https://example.com/wiki/A_(b)', want: [l('https://example.com/wiki/A_(b)')] },
    { name: 'balanced paren then full stop', body: 'https://example.com/wiki/A_(b).', want: [l('https://example.com/wiki/A_(b)'), t('.')] },
    { name: 'two URLs', body: 'https://a.example and http://b.example', want: [l('https://a.example', 'https://a.example/'), t(' and '), l('http://b.example', 'http://b.example/')] },
    { name: 'uppercase scheme', body: 'HTTPS://EXAMPLE.COM/a', want: [l('HTTPS://EXAMPLE.COM/a', 'https://example.com/a')] },
    { name: 'javascript stays text', body: 'javascript:alert(1)', want: [t('javascript:alert(1)')] },
    { name: 'data stays text', body: 'data:text/html,<script>alert(1)</script>', want: [t('data:text/html,<script>alert(1)</script>')] },
    { name: 'ftp stays text', body: 'ftp://x', want: [t('ftp://x')] },
    { name: 'www stays text', body: 'www.example.com', want: [t('www.example.com')] },
    { name: 'scheme alone stays text', body: 'http:// and https://', want: [t('http:// and https://')] },
    { name: 'scheme glued to a word stays text', body: 'xhttps://example.com', want: [t('xhttps://example.com')] },
    { name: 'tags stay characters', body: '<b>https://example.com/a</b>', want: [t('<b>'), l('https://example.com/a'), t('</b>')] },
    { name: 'unicode host stays text', body: 'https://例え.jp/パス.', want: [t('https://例え.jp/パス.')] },
    { name: 'lookalike host stays text', body: 'https://аррӏе.com/', want: [t('https://аррӏе.com/')] },
    { name: 'accented host stays text', body: 'https://münchen.de/', want: [t('https://münchen.de/')] },
    // The URL parser skips extra slashes and backslashes after the scheme;
    // the host behind them is checked all the same (codex round 2 on #1092).
    { name: 'extra slash before a lookalike host', body: 'https:///аррӏе.com/login', want: [t('https:///аррӏе.com/login')] },
    { name: 'backslash before a lookalike host', body: 'https://\\аррӏе.com/login', want: [t('https://\\аррӏе.com/login')] },
    { name: 'uppercase scheme, extra slashes, lookalike host', body: 'HTTPS:////аррӏе.com/', want: [t('HTTPS:////аррӏе.com/')] },
    { name: 'percent-encoded lookalike host after an extra slash', body: 'https:///%D0%B0pple.com/', want: [t('https:///%D0%B0pple.com/')] },
    { name: 'a punycode host written out stays text', body: 'https://xn--80ak6aa92e.com/', want: [t('https://xn--80ak6aa92e.com/')] },
    { name: 'combining grapheme joiner in the path stays text', body: 'https://example.com/log\u034Fin', want: [t('https://example.com/log\u034Fin')] },
    { name: 'variation selector in the path stays text', body: 'https://example.com/log\uFE0Fin', want: [t('https://example.com/log\uFE0Fin')] },
    { name: 'credentials stay text', body: 'https://bank.example@evil.example/login', want: [t('https://bank.example@evil.example/login')] },
    { name: 'user and password stay text', body: 'see https://user:pw@example.com/', want: [t('see https://user:pw@example.com/')] },
    { name: 'bidi override stays text', body: 'https://trusted.example\u202e@evil.example/login', want: [t('https://trusted.example\u202e@evil.example/login')] },
    { name: 'isolate stays text', body: 'https://example.com/a\u2066b', want: [t('https://example.com/a\u2066b')] },
    { name: 'zero width stays text', body: 'https://exa\u200bmple.com/', want: [t('https://exa\u200bmple.com/')] },
    { name: 'soft hyphen stays text', body: 'https://example.com/a\u00adb', want: [t('https://example.com/a\u00adb')] },
    { name: 'a byte order mark ends the run, as space does', body: 'https://example.com/a\ufeffb', want: [l('https://example.com/a'), t('\ufeffb')] },
    { name: 'ascii host with a non-ascii path still links', body: 'https://example.com/パス', want: [l('https://example.com/パス', 'https://example.com/%E3%83%91%E3%82%B9')] },
    { name: 'newline ends a link', body: 'https://example.com/a\nnext', want: [l('https://example.com/a'), t('\nnext')] },
  ];
  it.each(cases)('$name', ({ body, want }) => {
    expect(linkify(body)).toEqual(want);
  });

  it('leaves a link longer than the cap as text', () => {
    const body = `https://example.com/${'a'.repeat(maxLink)}`;
    expect(linkify(body)).toEqual([t(body)]);
  });

  it('keeps every character of the body, in order', () => {
    const body = 'a (https://example.com/x). b https://y.example/z, javascript:1 c';
    expect(linkify(body).map((s) => s.text).join('')).toBe(body);
  });

  it.each([
    ['repeated scheme', 'http://'.repeat(200_000)],
    ['closing brackets', `https://example.com/${')'.repeat(500_000)}`],
    ['punctuation tail', `https://example.com/${'.'.repeat(500_000)}`],
    ['many short links', 'https://a.example '.repeat(50_000)],
    ['no-host schemes', 'https:// '.repeat(100_000)],
    ['nested brackets', `https://example.com/${'(['.repeat(200_000)}`],
  ])('reads a long hostile body (%s) in bounded time', (_name, body) => {
    const started = performance.now();
    const out = linkify(body);
    expect(performance.now() - started).toBeLessThan(2000);
    expect(out.map((s) => s.text).join('').length).toBeLessThanOrEqual(body.length);
  });
});
