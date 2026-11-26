// What Chromium itself writes to a page's console when LiveKit refuses a
// connection (issue #1760) or rejects its handshake (issues #1915 and #2150),
// and how expectClean can check those lines.
// Input: console lines, the entries the browser logged itself, and LiveKit's
// origin. Output: the lines with the join token taken out of those line
// shapes, where the browser's network source wrote them. It owns the shapes
// and the rule of where a line must come from, and nothing else.

// A WebSocket that fails to open is logged by Chromium, not by the app, with
// the whole address, and livekit-client puts the join token in that address.
// While LiveKit is stopped the SDK retries, and each try is refused:
//   error: WebSocket connection to 'ws://127.0.0.1:1234/rtc/v1?access_token=...&join_request=...' failed: Error in connection establishment: net::ERR_CONNECTION_REFUSED
// Once LiveKit is back, it no longer has the room the page was in, and it
// answers the SDK's next try with a status instead of the upgrade (404 in the
// restart test, issue #1915; 400 for a join request it cannot read):
//   error: WebSocket connection to 'ws://127.0.0.1:1234/rtc/v1?access_token=...&join_request=...' failed: Error during WebSocket handshake: Unexpected response code: 404
// Whether either line is written, and whether it carries a join token conchd
// issued or one LiveKit refreshed after the join, is a matter of timing. Both
// are in the world's secrets (watchPage in voice-world.ts reads the refreshed
// one from the signalling frames, issue #1948), so either is taken out of an
// allowed line here and caught on any other. The token is the person's own,
// short-lived, and only in their own devtools console; it is not an app log
// (the page's own logging is checked everywhere else).
//
// The page's watcher records each console entry as "<type>: <text>" (voice-world.ts
// watchPage), and Chromium writes these as type error, so the line is
// "error: WebSocket connection to ...".
//
// What a line says is not enough: the page can console.error the very same
// text, token and all, and Playwright's console event does not tell the two
// apart. Where it came from does. Chromium writes these lines itself, through
// the DevTools Log domain (Log.entryAdded, source "network"), while every
// console.* call of the page arrives through Runtime.consoleAPICalled and is
// never a Log entry. watchPage records the Log entries beside the console
// (Watch.browserLog, Chromium only), and a line is allowed only when the
// browser's network source logged that exact line; each such entry allows one
// line, so a copy the page logs of a real one is still caught.
//
// withoutJoinTokenInSocketRefusals takes the token out of lines of exactly those
// shapes: the given LiveKit origin, the /rtc or /rtc/v1 path, access_token
// first, and one of Chromium's two endings, "Error in connection
// establishment: net::ERR_..." or "Error during WebSocket handshake:
// Unexpected response code: <three digits>", and only as many of them as the
// browser's network source logged. A line of any other shape (another of
// Chromium's endings among them), and a line of that shape the page logged
// itself, keeps its token, so expectClean still rejects it; and what is left
// of an allowed line (the join_request, for one) is still checked for every
// other secret.
// The token-refresh test uses v1FallbackOnly during the initial join (issue
// #2150), where only a 404 handshake on /rtc/v1 is expected.

// One entry the browser logged itself (DevTools Log.entryAdded): its source
// ("network", "javascript", "violation", ...), its level and its text.
export interface BrowserLogEntry {
  readonly source: string;
  readonly level: string;
  readonly text: string;
}

export function withoutJoinTokenInSocketRefusals(
  lines: readonly string[],
  browserLog: readonly BrowserLogEntry[],
  livekitOrigin: string,
  { v1FallbackOnly = false }: { readonly v1FallbackOnly?: boolean } = {},
): string[] {
  const origin = livekitOrigin.replace(/^http/, 'ws').replace(/\/+$/, '');
  const escaped = origin.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const path = v1FallbackOnly ? '/rtc/v1' : '/rtc(?:/v1)?';
  const handshake = 'Error during WebSocket handshake: Unexpected response code: ';
  const ending = v1FallbackOnly ? `${handshake}404` : `(?:Error in connection establishment: net::ERR_[A-Z_]+|${handshake}[0-9]{3})`;
  const shape = new RegExp(`^(error: WebSocket connection to '${escaped}${path}\\?access_token=)[^&'\\s]*(&[^'\\s]*)?(' failed: ${ending})$`);
  // How many times the browser's network source logged each line, written as
  // watchPage writes a console line.
  const fromNetwork = new Map<string, number>();
  for (const e of browserLog) {
    if (e.source !== 'network') continue;
    const line = `${e.level}: ${e.text}`;
    fromNetwork.set(line, (fromNetwork.get(line) ?? 0) + 1);
  }
  return lines.map((line) => {
    if (!shape.test(line)) return line;
    const left = fromNetwork.get(line) ?? 0;
    if (left === 0) return line;
    fromNetwork.set(line, left - 1);
    return line.replace(shape, '$1$2$3');
  });
}

// What expectClean says when a secret is in the console (issue #1955): each
// line that holds a secret, with every secret in it written as "[secret #i]"
// so that no secret is printed, and where the line came from: "browser log
// (<source>)" when the browser logged that exact line itself, "page console"
// otherwise. It reads the lines as given to the check (after the allowance
// above), so it shows what the check refused.
export function secretLines(lines: readonly string[], browserLog: readonly BrowserLogEntry[], secrets: readonly string[]): string[] {
  const out: string[] = [];
  for (const line of lines) {
    const found = secrets.map((s, i) => ({ s, i })).filter(({ s }) => s !== '' && line.includes(s));
    if (found.length === 0) continue;
    const from = browserLog.find((e) => `${e.level}: ${e.text}` === line);
    let shown = line;
    // The longest first, so that a secret inside another is not left half written.
    for (const { s, i } of [...found].sort((x, y) => y.s.length - x.s.length)) shown = shown.replaceAll(s, `[secret #${String(i)}]`);
    out.push(`${from === undefined ? 'page console' : `browser log (${from.source})`}: ${shown}`);
  }
  return out;
}
