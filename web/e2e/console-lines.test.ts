// The one console allowance of expectClean (issue #1760). A unit test: no
// browser, no network.
import { describe, expect, it } from 'vitest';
import { withoutJoinTokenInSocketRefusals } from './console-lines';

const origin = 'ws://127.0.0.1:22197';
const tail = "' failed: Error in connection establishment: net::ERR_CONNECTION_REFUSED";

describe('withoutJoinTokenInSocketRefusals', () => {
  it.each([
    ['the SDK retry', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin&join_request=CAES%3D${tail}`],
    ['the first connect', `error: WebSocket connection to '${origin}/rtc?access_token=eyJjoin&reconnect=1${tail}`],
    ['no other parameter', `error: WebSocket connection to '${origin}/rtc/v1?access_token=eyJjoin${tail}`],
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
