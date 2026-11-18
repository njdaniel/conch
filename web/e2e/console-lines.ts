// What Chromium itself writes to a page's console when LiveKit refuses a
// connection (issue #1760) or rejects its handshake (issue #1915), and the one
// allowance expectClean makes for it.
// Input: console lines and LiveKit's origin. Output: the lines with the join
// token taken out of those line shapes. It owns the shapes and nothing else.

// A WebSocket that fails to open is logged by Chromium, not by the app, with
// the whole address, and livekit-client puts the join token in that address.
// While LiveKit is stopped the SDK retries, and each try is refused:
//   error: WebSocket connection to 'ws://127.0.0.1:1234/rtc/v1?access_token=...&join_request=...' failed: Error in connection establishment: net::ERR_CONNECTION_REFUSED
// Once LiveKit is back, it no longer has the room the page was in, and it
// answers the SDK's next try with a status instead of the upgrade (404 in the
// restart test, issue #1915; 400 for a join request it cannot read):
//   error: WebSocket connection to 'ws://127.0.0.1:1234/rtc/v1?access_token=...&join_request=...' failed: Error during WebSocket handshake: Unexpected response code: 404
// Whether either line is written, and whether it carries a join token conchd
// issued or one LiveKit refreshed after the join, is a matter of timing. The
// token is the person's own, short-lived, and only in their own devtools
// console; it is not an app log (the page's own logging is checked everywhere
// else).
//
// The page's watcher records each console entry as "<type>: <text>" (voice-world.ts
// watchPage), and Chromium writes these as type error, so the line is
// "error: WebSocket connection to ...".
//
// withoutJoinTokenInSocketRefusals takes the token out of lines of exactly those
// shapes: the given LiveKit origin, the /rtc or /rtc/v1 path, access_token
// first, and one of Chromium's two endings, "Error in connection
// establishment: net::ERR_..." or "Error during WebSocket handshake:
// Unexpected response code: <three digits>". A line of any other shape (another
// of Chromium's endings among them) keeps its token, so expectClean still
// rejects it; and what is left of an allowed line (the join_request, for one)
// is still checked for every other secret.
export function withoutJoinTokenInSocketRefusals(lines: readonly string[], livekitOrigin: string): string[] {
  const origin = livekitOrigin.replace(/^http/, 'ws').replace(/\/+$/, '');
  const escaped = origin.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const ending = '(?:Error in connection establishment: net::ERR_[A-Z_]+|Error during WebSocket handshake: Unexpected response code: [0-9]{3})';
  const shape = new RegExp(`^(error: WebSocket connection to '${escaped}/rtc(?:/v1)?\\?access_token=)[^&'\\s]*(&[^'\\s]*)?(' failed: ${ending})$`);
  return lines.map((line) => line.replace(shape, '$1$2$3'));
}
