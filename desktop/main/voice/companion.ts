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
//  - the DELETE follows a rename of the channel (issue #1881, as the share's
//    sound does, #1880): conchd resolves the channel by its current name, so
//    after a rename the held name answers 404 channel_not_found. When main
//    holds the channel's id (channelId, learned by the share's sound from a
//    presence answer that showed this person's share, never from the page),
//    that 404 alone makes main read conchd's channel list, find the id and
//    send the DELETE once more by the name it has now. With no id, or a name
//    main's client would refuse, the one DELETE by the held name stands, as it
//    did, and its failure is logged. One bound covers the DELETE, the list
//    read and the retry (COMPANION_REMOVE_TIMEOUT_MS);
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
import { currentName, isNotFound } from './channel-follow.ts';
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
  // Whether the screen is locked (Lifecycle.locked, issue #1588): no grant
  // is given then. Required: a caller that leaves it out would fail open
  // (tests pass `() => false`).
  readonly locked: () => boolean;
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
  // The owned window's web contents now, or null. Taken with the grant, so a
  // reload can be bound to the window that held it. Absent: null.
  readonly contents?: () => object | null;
  // Reloads the owned window's document: the end of anything the page still
  // holds when the companion's removal at conchd was not confirmed. Given the
  // contents and the document generation that held the grant, as tell is
  // given the document: the implementation reloads only if the owned contents
  // are still that object and still show that document (S2 of #1621), so a
  // profile switch or a new document inside the removal's wait is not
  // reloaded. Not asked for the ends 'reload' and 'crash': the document is
  // already gone. Absent: nothing.
  readonly reload?: (contents: object | null, document: number) => void;
  // Called when a grant was issued and the record is made, with the contents
  // and document that hold it (app.ts: starts the share allowance's capture
  // window, share-allowance.ts).
  readonly granted?: (contents: object | null, document: number) => void;
  // Called at the start of every end() and stop(), whether or not a companion
  // is held (app.ts: clears the share allowance, share-allowance.ts).
  readonly ended?: () => void;
  // The id of the live share's channel, as main learned it from conchd's own
  // answers (ShareSound.channelId), or null when none was learned. Read at the
  // start of every end() and stop(), synchronously and before any await: the
  // share's sound (soundEnded) forgets the id once its own withdrawals are
  // done, and the companion's DELETE runs alongside it and may need the id
  // after its first answer. Absent: no id, and the DELETE is by the held name
  // alone.
  readonly channelId?: (share: ShareRef) => number | null;
  // The share's sound (issue #1854): told of every end and stop of a held
  // companion, once, with the share's ref and after the record is forgotten.
  // It sends the helper's stop and then the consent's DELETE, and resolves
  // when both are done (bounded); the companion's own removal does not wait
  // for it and it does not wait for the removal. Absent (the switch off):
  // nothing.
  readonly soundEnded?: (share: ShareRef, reason: RemoveReason) => Promise<void>;
}

// The live share as the share's sound (issue #1854, voice/share-audio.ts) holds
// it: a number main made for this companion alone (never the page's, never
// reused), the destination, the helper generation it was asked under, the
// document that was given the grant and the client of the activation it was
// asked with. A stop of the sound names the share by `id`, so a late stop of
// an older share can never stop a newer one's sound.
export interface ShareRef {
  readonly id: number;
  readonly channel: string;
  readonly generation: number;
  readonly document: number;
  readonly client: CompanionClient;
  // The principal the grant's identity (p<id>.screen) names: this person's own
  // participant in presence. From conchd's answer, never the page's.
  readonly principal: number;
}

// The live companion: what it was asked for, and with what.
interface Held {
  readonly id: number;
  readonly channel: string;
  // The voice helper's generation it was asked under: a new helper is not
  // the parent this companion was folded into.
  readonly generation: number;
  readonly client: CompanionClient;
  // The document that was given the grant.
  readonly document: number;
  // The owned contents when it was given: null when none was known.
  readonly contents: object | null;
  readonly origin: string;
  readonly principal: number;
}

type Asking = Pick<Held, 'channel' | 'generation'>;

// Why a removal was sent, for its log line.
export type RemoveReason = CompanionEndReason | 'stopped' | 'unused';

// stillHeldBy says whether the owned window is still the one that held a
// grant: the same web contents object, and the same document generation (S2
// of #1621). Only then is a reload for an unconfirmed removal the end of what
// that page holds; any other window or document is not its to end.
export function stillHeldBy(now: { readonly contents: object | null; readonly generation: number }, held: { readonly contents: object | null; readonly document: number }): boolean {
  return now.contents !== null && now.contents === held.contents && now.generation === held.document;
}

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
function grantOf(body: unknown): { readonly grant: CompanionGrant; readonly origin: string; readonly principal: number } | null {
  if (fromServer(voiceCompanionResponseV1)(body).length > 0) return null;
  const answer = body as VoiceCompanionResponseV1;
  if (answer.publish !== REQUEST.publish || answer.subscribe !== REQUEST.subscribe) return null;
  if (!COMPANION_IDENTITY.test(answer.identity) || answer.token === '' || answer.token.length > MAX_TOKEN_LENGTH) return null;
  const origin = livekitOrigin(answer.livekit_url);
  if (origin === null) return null;
  const principal = Number(/^p([1-9][0-9]{0,18})\./u.exec(answer.identity)?.[1]);
  if (!Number.isSafeInteger(principal)) return null;
  // Exactly these four: never the room's name, the bearer or the session.
  return { origin, principal, grant: { livekitUrl: answer.livekit_url, token: answer.token, identity: answer.identity, expiresAt: answer.expires_at } };
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
  readonly #changed = new Set<() => void>();
  // Counts ends and stops: a request begun before one is overtaken by it.
  #epoch = 0;
  // Numbers the companions held, from 1: a share's identity for its sound.
  #serial = 0;

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

  // onChange calls the listener whenever a companion becomes live or ends, so
  // the tray can follow "Sharing your screen". It returns the remover.
  onChange(listener: () => void): () => void {
    this.#changed.add(listener);
    return () => {
      this.#changed.delete(listener);
    };
  }

  #announce(): void {
    for (const listener of [...this.#changed]) {
      try {
        listener();
      } catch {
        // A listener's failure is not the companion's.
      }
    }
  }

  // liveOrigin is the LiveKit origin of the live grant, or null when no
  // companion is live (#1580, D4-03: the request filter allows it alone).
  liveOrigin(): string | null {
    return this.#held?.origin ?? null;
  }

  // current is the live share, as its sound holds it (ShareRef), or null when
  // no companion is live. It is main's own record: nothing the page says.
  current(): ShareRef | null {
    const held = this.#held;
    return held === null ? null : { id: held.id, channel: held.channel, generation: held.generation, document: held.document, client: held.client, principal: held.principal };
  }

  #refOf(held: Held): ShareRef {
    return { id: held.id, channel: held.channel, generation: held.generation, document: held.document, client: held.client, principal: held.principal };
  }

  // #idOf is the id of the held share's channel, if main learned one; never
  // throws. Taken before #endSound (see CompanionDeps.channelId).
  #idOf(held: Held): number | null {
    try {
      return this.#deps.channelId?.(this.#refOf(held)) ?? null;
    } catch {
      return null;
    }
  }

  // #endSound tells the share's sound that this share ended, and never throws.
  #endSound(held: Held, reason: RemoveReason): Promise<void> {
    const sound = this.#deps.soundEnded;
    if (sound === undefined) return Promise.resolve();
    try {
      return sound(this.#refOf(held), reason).catch(() => {
        this.#deps.log(`companion: the share's sound could not be ended after ${reason}`);
      });
    } catch {
      this.#deps.log(`companion: the share's sound could not be ended after ${reason}`);
      return Promise.resolve();
    }
  }

  // grant asks conchd for the companion's grant for the voice destination
  // and hands back the grant alone.
  async grant(request: { readonly channel: string }): Promise<CompanionGrantResult> {
    const { channel } = request;
    if (this.#deps.locked()) {
      this.#deps.log('companion: refused: the screen is locked');
      return refuse('ended');
    }
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
    const contents = this.#deps.contents?.() ?? null;
    this.#held = { ...asking, id: ++this.#serial, client, document, contents, origin: given.origin, principal: given.principal };
    try {
      this.#deps.granted?.(contents, document);
    } catch {
      // A window that cannot be told is not a reason to drop the grant.
    }
    this.#deps.log('companion: issued');
    // After the window was told and the grant is held: a listener (the tray)
    // sees the live companion, never one that has already ended.
    this.#announce();
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
    // Ended is called above, before the record goes: no listener sees a held
    // grant after it.
    this.#announce();
    // The id is read now, before the first await: the sound's end forgets it
    // when its withdrawals are done (a microtask later when it has none), while
    // the DELETE below may need it after its first answer.
    const id = this.#idOf(held);
    // The share's sound ends first, at once: its stop is written now, ahead
    // of anything the removal does.
    const sound = this.#endSound(held, 'stopped');
    const closing = this.#close();
    const removed = await this.#remove(held.client, held.channel, 'stopped', id);
    await closing;
    await sound;
    this.#unconfirmed(removed, held, 'stopped');
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
    this.#announce();
    try {
      this.#deps.tell(held.document, { type: 'end', reason });
    } catch {
      // A window that cannot be told is not a reason to keep the companion.
    }
    // The id is read now, before the first await: the sound's end forgets it
    // when its withdrawals are done (a microtask later when it has none), while
    // the DELETE below may need it after its first answer.
    const id = this.#idOf(held);
    // The share's sound ends at once, with the end: its stop is written before
    // anything else here, and its withdrawal follows the stop.
    const sound = this.#endSound(held, reason);
    // Begun at once, alongside the DELETE: neither waits for the other.
    const closing = this.#close();
    const removed = await this.#remove(held.client, held.channel, reason, id);
    await closing;
    await sound;
    this.#unconfirmed(removed, held, reason);
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

  // #unconfirmed reloads the window that held the grant when the removal was
  // not confirmed, unless that document is already gone: after a reload or a
  // crash there is nothing left to reload, and the next document is not the
  // one that held the grant.
  #unconfirmed(removed: boolean, held: Held, reason: RemoveReason): void {
    if (removed) return;
    if (reason === 'reload' || reason === 'crash') return;
    this.#deps.log('companion: the removal was not confirmed; reloading the window');
    try {
      this.#deps.reload?.(held.contents, held.document);
    } catch {
      // A window that cannot be reloaded is already gone.
    }
  }

  // The one DELETE of an end. True when conchd confirmed it. By the held
  // name; when it answers 404 channel_not_found and `id` is known (it is not
  // for an unused grant, which no share's sound ever saw, nor for a watch
  // with no share), the channel's name now is found by that id and the DELETE
  // is sent once more with it. Nothing else is retried, and a name main's
  // client refuses (paramOk, in currentName) is not sent. One deadline bounds
  // the DELETE, the list read and the retry.
  async #remove(client: CompanionClient, channel: string, reason: RemoveReason, id: number | null = null): Promise<boolean> {
    const deadline = Date.now() + COMPANION_REMOVE_TIMEOUT_MS;
    const sendBy = async (name: string, first = false): Promise<Exchange> => {
      const ms = first ? COMPANION_REMOVE_TIMEOUT_MS : deadline - Date.now();
      if (ms <= 0) return { kind: 'none' };
      try {
        return await client.send({ op: 'voiceViewerEnd', params: { channel: name }, query: 'publish=screen', timeoutMs: ms });
      } catch {
        return { kind: 'none' };
      }
    };
    let exchange = await sendBy(channel, true);
    if (id !== null && isNotFound(exchange)) {
      const name = await currentName(client, id, deadline - Date.now());
      if (name !== 'gone' && name !== 'unknown' && name.name !== channel) exchange = await sendBy(name.name);
    }
    if (exchange.kind === 'answer' && exchange.ok) return true;
    this.#deps.log(`companion: the removal after ${reason} was not confirmed`);
    return false;
  }
}
