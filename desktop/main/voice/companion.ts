// Main's screen companion (issue #1579, D4-02 of #772; linux-desktop.md §4,
// §6, §6.4, §9, §10). Input: the person's request for a grant (the IPC layer
// has checked the caller, the shape and that it follows a click or key), the
// voice service's confirmed view, and the ends main decides on: the person's
// Leave, the helper's loss, a room renewal, sign-out, a switch, quit, the
// page's crash or reload, and (#1588, D5-02) lock and suspend. Output: the
// companion grant (CompanionGrant) and nothing else, handed to the renderer;
// on every end, a CompanionEvent to the page and then a DELETE to conchd.
// Owns: the one live companion, p<id>.screen, of the activation in force, the
// one request for it in flight, and the LiveKit origin of the live grant
// (liveOrigin, which #1580's request filter and display-capture rules read).
// Beside a grant it says when this desktop cannot share a screen (#1580:
// shareUnavailable, from the portal probe), so the page can say so; the
// grant itself is still given, for watching.
//
// What it keeps:
//  - a grant is asked for only for the voice destination, while the view is
//    running and the helper is connected there: browsing another channel
//    never gets one (UX-02), and a refusal here calls nothing;
//  - it is asked through the operation table (voiceViewer, renderer: false)
//    with the client of the activation in force, which alone holds the
//    bearer; the room's name and the rest of the answer are dropped here;
//  - voice_no_session is asked once more after one poller pass, and nothing
//    else is retried: there is no loop;
//  - at most one companion, live or being asked for: a second request is
//    refused as conchd would refuse it (elsewhere), and calls nothing;
//  - every end forgets the record first, tells the page (if the document that
//    was given the grant still shows) and then sends the DELETE once; a DELETE
//    that fails is logged by the end's reason alone;
//  - every end, the page's own included, also asks the owned window's session
//    to close its connections (closeConnections), after the record is
//    forgotten: the filter only judges a request when it begins. That call
//    reaches idle and pending sockets only: a live WebSocket stays open and
//    keeps sending (probed on #1621, S2). What ends the companion's LiveKit
//    socket is conchd's DELETE removing the participant. So when the DELETE
//    is not confirmed (it failed or timed out), main reloads the owned
//    window's document (reload), and no page-held socket survives an
//    unconfirmed end. Main has no signal for whether the page's
//    display-capture track still runs, so it cannot tell a page that ignored
//    the end event;
//  - every grant needs a new act: none is re-issued after a room renewal,
//    which ends the share (Q-D4-2 on #1578).
//
// No log line carries a token, a channel, a room or a URL.
import type {
  CompanionEndReason,
  CompanionEvent,
  CompanionGrant,
  CompanionGrantError,
  CompanionGrantResult,
  CompanionShareUnavailable,
  NativeVoiceView,
} from '../../../web/src/platform/desktop/bridge.ts';
import { voiceCompanionResponseV1, type VoiceCompanionRequestV1, type VoiceCompanionResponseV1 } from '../../../web/src/schema/generated.ts';
import { fromServer } from '../../../web/src/schema/validate.ts';
import type { Exchange, Send } from '../api/client.ts';
import type { VoiceService } from './service.ts';

// One pass of conchd's voice poller: how long after the helper connects
// conchd may still not have seen p<id> in the room (voice_no_session).
export const COMPANION_PASS_MS = 1_500;

// How long the DELETE may take: inside the teardown sequence's bound for one
// step (profiles/switch.ts STEP_BOUND_MS), so a switch is not held up by it.
export const COMPANION_REMOVE_TIMEOUT_MS = 2_500;

// B2 (§6): the companion publishes the screen and may watch others' shares.
const REQUEST: VoiceCompanionRequestV1 = { publish: 'screen', subscribe: true };

// Bounds on what is handed on, past the schema's own checks.
const MAX_TOKEN_LENGTH = 8 * 1024;
const MAX_URL_LENGTH = 2 * 1024;
const COMPANION_IDENTITY = /^p[1-9][0-9]{0,18}\.screen$/u;
const LOOPBACK = new Set(['localhost', '127.0.0.1', '[::1]']);

// The request executor of an activation (api/client.ts); a test stands in.
export interface CompanionClient {
  send(request: Send): Promise<Exchange>;
}

export interface CompanionDeps {
  // The voice owner: its view now, and every change of it.
  readonly voice: Pick<VoiceService, 'state' | 'subscribe'>;
  // The client of the activation in force, or null when there is none.
  readonly client: () => CompanionClient | null;
  // The owned window's document generation now (DocumentGate.generation).
  readonly document: () => number;
  // Sends the event to the owned web contents, if the given document still
  // shows there; else nothing.
  readonly tell: (document: number, event: CompanionEvent) => void;
  readonly log: (line: string) => void;
  // Test seam for the one wait before the second ask.
  readonly sleep?: (ms: number) => Promise<void>;
  // Asks whether this desktop can share a screen (portal.ts), alongside the
  // request to conchd; null when it can. Absent: nothing is said.
  readonly share?: () => Promise<CompanionShareUnavailable | null>;
  // Asks the owned window's session to close its connections (the
  // activation's session.closeAllConnections). Called on every end, after the
  // record is forgotten. It closes idle and pending sockets only; a live
  // WebSocket survives it. Absent: nothing.
  readonly closeConnections?: () => Promise<void>;
  // Reloads the owned window's document: the end of anything the page still
  // holds when the companion's removal at conchd was not confirmed. Absent:
  // nothing.
  readonly reload?: () => void;
  // Called at the start of every end() and stop(), whether or not a companion
  // is held (app.ts: clears the share allowance, share-allowance.ts).
  readonly ended?: () => void;
}

// The live companion: what it was asked for, and with what.
interface Held {
  readonly channel: string;
  // The voice helper's generation it was asked under: a new helper is not
  // the parent this companion was folded into.
  readonly generation: number;
  readonly client: CompanionClient;
  // The document that was given the grant.
  readonly document: number;
  readonly origin: string;
}

type Asking = Pick<Held, 'channel' | 'generation'>;

// Why a removal was sent, for its log line.
type RemoveReason = CompanionEndReason | 'stopped' | 'unused';

const refuse = (error: CompanionGrantError): CompanionGrantResult => ({ ok: false, error });

// connectedIn says whether a grant may be asked for: voice is running in
// that channel and the helper is connected there.
function connectedIn(view: NativeVoiceView, channel: string): boolean {
  return view.status === 'running' && view.channel === channel && view.helper?.phase === 'connected';
}

// lossOf says why a companion asked for under `held` must end on this view,
// or null when it may stay. The LiveKit SDK's own resume of the helper's
// connection (reconnecting, with no reason) keeps it; a room conchd deleted
// is a renewal; any other phase, a new helper, another channel or a view
// that is not running is the helper's loss.
function lossOf(view: NativeVoiceView, held: Asking): CompanionEndReason | null {
  if (view.status !== 'running' || view.channel !== held.channel || view.generation !== held.generation || view.helper === undefined) return 'helper';
  const { phase, reason } = view.helper;
  if (phase === 'connected') return null;
  if (phase === 'reconnecting' && reason === undefined) return null;
  return reason === 'room_deleted' ? 'renewal' : 'helper';
}

// livekitOrigin is the origin of the LiveKit URL conchd named: wss or https,
// or ws or http to loopback (development), with no user information.
function livekitOrigin(text: string): string | null {
  if (text.length > MAX_URL_LENGTH) return null;
  let url: URL;
  try {
    url = new URL(text);
  } catch {
    return null;
  }
  if (url.username !== '' || url.password !== '') return null;
  const secure = url.protocol === 'wss:' || url.protocol === 'https:';
  const local = (url.protocol === 'ws:' || url.protocol === 'http:') && LOOPBACK.has(url.hostname);
  return secure || local ? url.origin : null;
}

// grantOf is the part of conchd's answer the renderer is given, and the
// origin main remembers, or null for an answer that is not a screen
// companion's grant as asked for.
function grantOf(body: unknown): { readonly grant: CompanionGrant; readonly origin: string } | null {
  if (fromServer(voiceCompanionResponseV1)(body).length > 0) return null;
  const answer = body as VoiceCompanionResponseV1;
  if (answer.publish !== REQUEST.publish || answer.subscribe !== REQUEST.subscribe) return null;
  if (!COMPANION_IDENTITY.test(answer.identity) || answer.token === '' || answer.token.length > MAX_TOKEN_LENGTH) return null;
  const origin = livekitOrigin(answer.livekit_url);
  if (origin === null) return null;
  // Exactly these four: never the room's name, the bearer or the session.
  return { origin, grant: { livekitUrl: answer.livekit_url, token: answer.token, identity: answer.identity, expiresAt: answer.expires_at } };
}

function codeOf(exchange: Exchange): string | null {
  if (exchange.kind !== 'answer') return null;
  const body: unknown = exchange.body;
  if (typeof body !== 'object' || body === null) return null;
  const code: unknown = (body as Record<string, unknown>)['code'];
  return typeof code === 'string' ? code : null;
}

const refusedWith = (exchange: Exchange, code: string): boolean => exchange.kind === 'answer' && exchange.status === 409 && codeOf(exchange) === code;

export class Companion {
  readonly #deps: CompanionDeps;
  readonly #sleep: (ms: number) => Promise<void>;
  #held: Held | null = null;
  #asking: Asking | null = null;
  // Counts ends and stops: a request begun before one is overtaken by it.
  #epoch = 0;

  constructor(deps: CompanionDeps) {
    this.#deps = deps;
    this.#sleep =
      deps.sleep ??
      ((ms) =>
        new Promise((resolve) => {
          setTimeout(resolve, ms);
        }));
    deps.voice.subscribe((view) => {
      this.#onVoice(view);
    });
  }

  // liveOrigin is the LiveKit origin of the live grant, or null when no
  // companion is live (#1580, D4-03: the request filter allows it alone).
  liveOrigin(): string | null {
    return this.#held?.origin ?? null;
  }

  // grant asks conchd for the companion's grant for the voice destination
  // and hands back the grant alone.
  async grant(request: { readonly channel: string }): Promise<CompanionGrantResult> {
    const { channel } = request;
    const view = this.#deps.voice.state();
    if (!connectedIn(view, channel)) {
      this.#deps.log('companion: refused: voice is not connected there');
      return refuse('not-in-voice');
    }
    const client = this.#deps.client();
    if (client === null) return refuse('failed');
    // A record of an activation that is no longer in force: the media hook
    // ends it first, so this is only a safety net.
    if (this.#held !== null && this.#held.client !== client) await this.end('switch');
    if (this.#held !== null || this.#asking !== null) {
      this.#deps.log('companion: refused: one is already live');
      return refuse('elsewhere');
    }
    const asking: Asking = { channel, generation: view.generation };
    this.#asking = asking;
    try {
      const share = (this.#deps.share?.() ?? Promise.resolve(null)).catch((): CompanionShareUnavailable => 'no-portal');
      return await this.#ask(client, asking, this.#epoch, this.#deps.document(), share);
    } finally {
      this.#asking = null;
    }
  }

  async #ask(client: CompanionClient, asking: Asking, epoch: number, document: number, share: Promise<CompanionShareUnavailable | null>): Promise<CompanionGrantResult> {
    let exchange = await this.#post(client, asking.channel);
    if (refusedWith(exchange, 'voice_no_session')) {
      // conchd has not yet seen p<id> in the room: one pass, then once more.
      await this.#sleep(COMPANION_PASS_MS);
      if (epoch !== this.#epoch) return refuse('ended');
      exchange = await this.#post(client, asking.channel);
      if (refusedWith(exchange, 'voice_no_session')) {
        this.#deps.log('companion: refused by conchd: no voice session yet');
        return refuse('not-yet');
      }
    }
    if (refusedWith(exchange, 'voice_companion_busy')) {
      this.#deps.log('companion: refused by conchd: one is live elsewhere');
      return refuse('elsewhere');
    }
    if (exchange.kind !== 'answer' || !exchange.ok) {
      this.#deps.log(`companion: refused by conchd (${exchange.kind === 'answer' ? String(exchange.status) : exchange.kind})`);
      return refuse('failed');
    }
    const given = grantOf(exchange.body);
    // Asked alongside conchd; read before the overtaken check, so that
    // nothing is awaited between that check and the record.
    const shareUnavailable = await share;
    const overtaken = epoch !== this.#epoch || this.#deps.client() !== client || lossOf(this.#deps.voice.state(), asking) !== null;
    if (given === null || overtaken) {
      // conchd has issued a companion nobody will use: remove it.
      if (given === null) this.#deps.log('companion: the answer was not the grant asked for');
      await this.#remove(client, asking.channel, 'unused');
      return refuse(given === null ? 'failed' : 'ended');
    }
    this.#held = { ...asking, client, document, origin: given.origin };
    this.#deps.log('companion: issued');
    return shareUnavailable === null ? { ok: true, grant: given.grant } : { ok: true, grant: given.grant, shareUnavailable };
  }

  // stop is the page's own companionEnd: the record goes and conchd is asked
  // to remove the companion. The page asked, so it is not told. A request
  // still in flight is overtaken.
  async stop(): Promise<boolean> {
    this.#epoch++;
    this.#deps.ended?.();
    const held = this.#held;
    if (held === null) return false;
    this.#held = null;
    const closing = this.#close();
    const removed = await this.#remove(held.client, held.channel, 'stopped');
    await closing;
    this.#unconfirmed(removed);
    return true;
  }

  // end is main's own end of the live companion, for any reason it has: the
  // record goes, the page that was given the grant is told, and conchd is
  // asked to remove the companion, in that order. A request still in flight
  // is overtaken. Public for #1588 (D5-02: lock and suspend).
  async end(reason: CompanionEndReason): Promise<boolean> {
    this.#epoch++;
    this.#deps.ended?.();
    const held = this.#held;
    if (held === null) return false;
    this.#held = null;
    try {
      this.#deps.tell(held.document, { type: 'end', reason });
    } catch {
      // A window that cannot be told is not a reason to keep the companion.
    }
    // Begun at once, alongside the DELETE: neither waits for the other.
    const closing = this.#close();
    const removed = await this.#remove(held.client, held.channel, reason);
    await closing;
    this.#unconfirmed(removed);
    return true;
  }

  #onVoice(view: NativeVoiceView): void {
    const held = this.#held;
    if (held !== null) {
      const loss = lossOf(view, held);
      if (loss !== null) void this.end(loss);
      return;
    }
    // A request in flight for a voice that has gone is overtaken.
    if (this.#asking !== null && lossOf(view, this.#asking) !== null) this.#epoch++;
  }

  async #post(client: CompanionClient, channel: string): Promise<Exchange> {
    try {
      return await client.send({ op: 'voiceViewer', params: { channel }, body: REQUEST });
    } catch {
      return { kind: 'none' };
    }
  }

  // #close drops the open connections of the window's session, once per end.
  // It never throws and is not retried: the record is already gone, so no new
  // request to LiveKit can begin.
  async #close(): Promise<void> {
    try {
      await this.#deps.closeConnections?.();
    } catch {
      this.#deps.log('companion: open connections could not be closed');
    }
  }

  // #unconfirmed reloads the window when the removal was not confirmed.
  #unconfirmed(removed: boolean): void {
    if (removed) return;
    this.#deps.log('companion: the removal was not confirmed; reloading the window');
    try {
      this.#deps.reload?.();
    } catch {
      // A window that cannot be reloaded is already gone.
    }
  }

  // The one DELETE of an end: never retried. True when conchd confirmed it.
  async #remove(client: CompanionClient, channel: string, reason: RemoveReason): Promise<boolean> {
    let exchange: Exchange;
    try {
      exchange = await client.send({ op: 'voiceViewerEnd', params: { channel }, query: 'publish=screen', timeoutMs: COMPANION_REMOVE_TIMEOUT_MS });
    } catch {
      exchange = { kind: 'none' };
    }
    if (exchange.kind === 'answer' && exchange.ok) return true;
    this.#deps.log(`companion: the removal after ${reason} was not confirmed`);
    return false;
  }
}
