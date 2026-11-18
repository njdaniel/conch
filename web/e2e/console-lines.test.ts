// The one console allowance of expectClean (issues #1760 and #1915). A unit
// test: no browser, no network.
import { describe, expect, it } from 'vitest';
import { withoutJoinTokenInSocketRefusals } from './console-lines';

const origin = 'ws://127.0.0.1:22197';
const tail = "' failed: Error in connection establishment: net::ERR_CONNECTION_REFUSED";
// LiveKit back up, answering with a status instead of the upgrade (issue #1915).
const rejected = (code: string) => `' failed: Error during WebSocket handshake: Unexpected response code: ${code}`;

describe('withoutJoinTokenInSocketRefusals', () => {
  it.each([
    ['the SDK retry', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin&join_request=CAES%3D${tail}`],
    ['the first connect', `error: WebSocket connection to '${origin}/rtc?access_token=eyJjoin&reconnect=1${tail}`],
    ['no other parameter', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail}`],
    ['the room gone after a restart', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin&join_request=CAES%3D${rejected('404')}`],
    ['an unreadable join request', `error: WebSocket connection to '${origin}/rtc?access_token=eyJjoin&join_request=x${rejected('400')}`],
  ])('takes the token out of %s', (_what, line) => {
    const [out] = withoutJoinTokenInSocketRefusals([line], origin);
    expect(out).not.toContain('eyJjoin');
    expect(out).toContain('access_token=');
  });

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
    expect(withoutJoinTokenInSocketRefusals([line], origin)).toEqual([line]);
  });

  it('keeps another secret in an allowed line', () => {
    const [out] = withoutJoinTokenInSocketRefusals([`error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin&join_request=conch_pat_x${tail}`], origin);
    expect(out).toContain('conch_pat_x');
  });

  it('accepts an http origin', () => {
    const [out] = withoutJoinTokenInSocketRefusals([`error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail}`], 'http://127.0.0.1:22197/');
    expect(out).not.toContain('eyJjoin');
  });
});
