// Chromium's failed-socket allowances and what expectClean says of a line it
// refuses (issue #1955). These unit tests need no browser or network.
import { describe, expect, it } from 'vitest';
import { secretLines, withoutJoinTokenInSocketRefusals, type BrowserLogEntry } from './console-lines';

const origin = 'ws://127.0.0.1:22197';
const tail = "' failed: Error in connection establishment: net::ERR_CONNECTION_REFUSED";
// LiveKit back up, answering with a status instead of the upgrade (issue #1915).
const rejected = (code: string) => `' failed: Error during WebSocket handshake: Unexpected response code: ${code}`;

// The browser's own Log entry for a line as watchPage records it ("error: ..."):
// what Chromium logs for a failed socket, from its network source.
const logged = (line: string, source = 'network'): BrowserLogEntry => {
  const at = line.indexOf(': ');
  return { source, level: line.slice(0, at), text: line.slice(at + 2) };
};
// The lines as allowed when the browser itself logged each of them.
const fromBrowser = (lines: string[]) => withoutJoinTokenInSocketRefusals(lines, lines.map((l) => logged(l)), origin);

const refused = `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin&join_request=CAES%3D${tail}`;
const gone = `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin&join_request=CAES%3D${rejected('404')}`;

describe('withoutJoinTokenInSocketRefusals', () => {
  it.each([
    ['the SDK retry', refused],
    ['the first connect', `error: WebSocket connection to '${origin}/rtc?access_token=eyJjoin&reconnect=1${tail}`],
    ['no other parameter', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail}`],
    ['the room gone after a restart', gone],
    ['an unreadable join request', `error: WebSocket connection to '${origin}/rtc?access_token=eyJjoin&join_request=x${rejected('400')}`],
  ])('takes the token out of %s, logged by the browser', (_what, line) => {
    const [out] = fromBrowser([line]);
    expect(out).not.toContain('eyJjoin');
    expect(out).toContain('access_token=');
  });

  // Each logged by the browser's network source too: the shape alone refuses them.
  it.each([
    ['another origin', `error: WebSocket connection to 'ws://127.0.0.1:1/rtc/v1?access_token=eyJjoin${tail}`],
    ['another path', `error: WebSocket connection to '${origin}/other?access_token=eyJjoin${tail}`],
    ['the token not first', `error: WebSocket connection to '${origin}/rtc/v1?x=1&access_token=eyJjoin${tail}`],
    ['another ending', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin' failed: eyJjoin`],
    ['no type prefix', `WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail}`],
    ['another type', `log: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail}`],
    ['a second line', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail}\neyJjoin`],
    ['an app log', `connecting to ${origin}/rtc/v1?access_token=eyJjoin`],
    ['text after the line', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail} and eyJjoin`],
    ['text after a status', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${rejected('404')} eyJjoin`],
    ['a status of other than three digits', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${rejected('4040')}`],
    ['a status that is not a number', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${rejected('eyJjoin')}`],
    ['another handshake error', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin' failed: Error during WebSocket handshake: Invalid status line`],
    ['a socket closed while connecting', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin' failed: WebSocket is closed before the connection is established.`],
    ['a status on another path', `error: WebSocket connection to '${origin}/other?access_token=eyJjoin${rejected('404')}`],
  ])('leaves %s alone', (_what, line) => {
    expect(fromBrowser([line])).toEqual([line]);
  });

  // Where the line came from (the security review of #1915): the page's own
  // console.error of the exact shape, a join token or a member's credential in
  // it, is never a Log entry of the browser's network source.
  it.each([
    ['the page logged a refusal itself', refused, [] as BrowserLogEntry[]],
    ['the page logged a rejection itself', gone, []],
    ['a credential, logged by the page in the shape', refused.replace('eyJjoin', 'conch_pat_x'), []],
    ['the browser logged it from another source', refused, [logged(refused, 'javascript')]],
    ['the browser logged it at another level', refused, [logged(refused.replace(/^error/, 'warning'))]],
    ['the browser logged another token', refused, [logged(refused.replace('eyJjoin', 'eyJother'))]],
    ['the browser logged another ending', refused, [logged(gone)]],
  ])('leaves a line alone when %s', (_what, line, browserLog) => {
    expect(withoutJoinTokenInSocketRefusals([line], browserLog, origin)).toEqual([line]);
  });

  it('allows one line for each line the browser logged, so a copy the page logs is still caught', () => {
    const out = withoutJoinTokenInSocketRefusals([refused, refused, gone], [logged(refused), logged(gone)], origin);
    expect(out.filter((l) => l.includes('eyJjoin'))).toEqual([refused]);
  });

  it('allows the room renewal 404 and still flags the token in other console lines', () => {
    const line = `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${rejected('404')}`;
    const leak = 'log: eyJjoin';
    const browserLog = [logged(line)];
    const check = (lines: string[]) => secretLines(withoutJoinTokenInSocketRefusals(lines, browserLog, origin), browserLog, ['eyJjoin']);
    expect(check([line])).toEqual([]);
    expect(check([line, leak, line])).toEqual([
      'page console: log: [secret #0]',
      `browser log (network): ${line.replace('eyJjoin', '[secret #0]')}`,
    ]);
  });

  it('allows only the browser’s /rtc/v1 fallback during the join', () => {
    const legacy404 = gone.replace('/rtc/v1?', '/rtc?');
    const v1Error = gone.replace(rejected('404'), rejected('500'));
    const otherOrigin404 = gone.replace(origin, 'ws://127.0.0.1:1');
    const http404 = 'error: Failed to load resource: the server responded with a status of 404 (Not Found)';
    const forged = gone.replace('eyJjoin', 'eyJrefreshed');
    const lines = [gone, gone, refused, legacy404, v1Error, otherOrigin404, http404, forged, 'log: eyJjoin', 'log: eyJrefreshed'];
    const browserLog = [gone, refused, legacy404, v1Error, otherOrigin404, http404].map((line) => logged(line));
    browserLog.push(logged(forged, 'javascript'));
    const out = withoutJoinTokenInSocketRefusals(lines, browserLog, origin, { v1FallbackOnly: true });
    expect(out).toEqual([gone.replace('eyJjoin', ''), ...lines.slice(1)]);
    const leaks = secretLines(out, browserLog, ['eyJjoin', 'eyJrefreshed']);
    expect(leaks).toContain('page console: log: [secret #0]');
    expect(leaks).toContain('page console: log: [secret #1]');
    expect(leaks.some((line) => /eyJjoin|eyJrefreshed/.test(line))).toBe(false);
  });

  it('keeps another secret in an allowed line', () => {
    const [out] = fromBrowser([`error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin&join_request=conch_pat_x${tail}`]);
    expect(out).toContain('conch_pat_x');
    expect(out).not.toContain('eyJjoin');
  });

  it('accepts an http origin', () => {
    const line = `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail}`;
    const [out] = withoutJoinTokenInSocketRefusals([line], [logged(line)], 'http://127.0.0.1:22197/');
    expect(out).not.toContain('eyJjoin');
  });
});

// What expectClean prints of a line holding a secret (issue #1955): never the
// secret, and where the line came from.
describe('secretLines', () => {
  it('writes each secret as its number and says the browser logged the line', () => {
    expect(secretLines([refused], [logged(refused)], ['conch_pat_x', 'eyJjoin'])).toEqual([
      `browser log (network): error: WebSocket connection to '${origin}/rtc/v1?access_token=[secret #1]&join_request=CAES%3D${tail}`,
    ]);
  });

  it('says the page logged a line the browser did not, and skips lines without a secret', () => {
    expect(secretLines(['log: hello', 'error: token eyJjoin'], [logged(refused)], ['eyJjoin'])).toEqual(['page console: error: token [secret #0]']);
  });

  it('writes a secret inside another as the longer one', () => {
    const out = secretLines(['log: eyJjoin.sig'], [], ['eyJjoin', 'eyJjoin.sig']);
    expect(out).toEqual(['page console: log: [secret #1]']);
  });
});
