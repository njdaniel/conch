// What Chromium itself writes to a page's console when LiveKit refuses a
// connection (issue #1760), and the one allowance expectClean makes for it.
// Input: console lines and LiveKit's origin. Output: the lines with the join
// token taken out of that one line shape. It owns the shape and nothing else.

// A WebSocket that fails to open is logged by Chromium, not by the app, with
// the whole address, and livekit-client puts the join token in that address:
//   error: WebSocket connection to 'ws://127.0.0.1:1234/rtc/v1?access_token=...&join_request=...' failed: Error in connection establishment: net::ERR_CONNECTION_REFUSED
// While LiveKit is stopped the SDK retries, so whether such a line is written is
// a matter of timing. The token is the person's own, short-lived, and only in
// their own devtools console; it is not an app log (the page's own logging is
// checked everywhere else).
//
// The page's watcher records each console entry as "<type>: <text>" (voice-world.ts
// watchPage), and Chromium writes this one as type error, so the line is
// "error: WebSocket connection to ...".
//
// withoutJoinTokenInSocketRefusals takes the token out of lines of exactly that
// shape: the given LiveKit origin, the /rtc or /rtc/v1 path, access_token first,
// and Chromium's "Error in connection establishment: net::ERR_..." ending. A
// line of any other shape keeps its token, so expectClean still rejects it; and
// what is left of an allowed line (the join_request, for one) is still checked
// for every other secret.
export function withoutJoinTokenInSocketRefusals(lines: readonly string[], livekitOrigin: string): string[] {
  const origin = livekitOrigin.replace(/^http/, 'ws').replace(/\/+$/, '');
  const escaped = origin.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const shape = new RegExp(`^(error: WebSocket connection to '${escaped}/rtc(?:/v1)?\\?access_token=)[^&'\\s]*(&[^'\\s]*)?(' failed: Error in connection establishment: net::ERR_[A-Z_]+)$`);
  return lines.map((line) => line.replace(shape, '$1$2$3'));
}
