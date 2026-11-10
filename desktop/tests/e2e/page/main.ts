// The page of the live companion test (issue #1583, D4-06 of #772): the web
// client's own voice client (web/src/voice/connection.ts), in companion mode,
// in a plain Chromium, with the desktop bridge stood in for by functions the
// test process exposes. It is built by companion-live.test.ts (Vite) and is
// never part of the packaged app, the web build or any release: nothing under
// web/ or desktop/main refers to it.
//
// What is real here: the VoiceClient with its companion code (startCompanion
// Share, watchCompanionStream), the LiveKit SDK, the room, the capture
// options, the publish. What the test stands in for, and where:
//  - the bridge (window.__bridge, from Playwright's exposeFunction): the grant
//    is main's Companion service in the test process (companion.ts), asked of
//    the real conchd; arm and the capture check are main's ShareAllowance. A
//    getDisplayMedia call asks the bridge's capture() first, as main's
//    display-capture handler does, and is refused with NotAllowedError when
//    the allowance does not give it: no click, no capture;
//  - the screen itself: Chromium's fake screen, from the launch flags the test
//    gives the Chromium it starts. There is no portal in CI.
// This file only wires those together and reports what the room holds.
import { Room as LiveKitRoom } from 'livekit-client';
import type { ShareClaimResult } from '../../../../web/src/api';
import type { CompanionEndReason, CompanionGrantResult } from '../../../../web/src/platform/desktop/bridge';
import type { ShareQuality } from '../../../../web/src/voice/share';
import { VoiceClient, type CompanionEnv } from '../../../../web/src/voice/connection';
import { newLiveKitRoom } from '../../../../web/src/voice/sdk';

// What the test process exposes (Playwright exposeFunction).
interface Bridge {
  grant(channel: string): Promise<CompanionGrantResult>;
  arm(): Promise<boolean>;
  end(): Promise<boolean>;
  capture(): Promise<boolean>;
  claim(channel: string, quality: ShareQuality | null): Promise<ShareClaimResult>;
  release(channel: string, startedAt: string): Promise<void>;
}

// One publication a room holds, as the SDK reports it.
export interface PublicationFact {
  readonly who: string;
  readonly local: boolean;
  readonly source: string;
  readonly kind: string;
  readonly subscribed: boolean;
}

declare global {
  interface Window {
    __bridge: Bridge;
    companionPage: CompanionPage;
  }
}

const rooms: LiveKitRoom[] = [];

// Every room the SDK makes is kept, so the test can read its publications.
// The adapter (web/src/voice/sdk.ts) holds its room privately, and the SDK's
// connect is an instance property, not a method of the prototype; the adapter
// does register for the room's events, which is a method of it, so the first
// registration on a room records it.
type Register = (...args: never[]) => unknown;
const register = Reflect.get(LiveKitRoom.prototype, 'on') as Register;
Object.defineProperty(LiveKitRoom.prototype, 'on', {
  configurable: true,
  writable: true,
  value: function (this: LiveKitRoom, ...args: never[]) {
    if (!rooms.includes(this)) rooms.push(this);
    return register.apply(this, args);
  },
});

const displayMedia = navigator.mediaDevices.getDisplayMedia.bind(navigator.mediaDevices);
let captures = 0;
navigator.mediaDevices.getDisplayMedia = async (options?: DisplayMediaStreamOptions) => {
  if (!(await window.__bridge.capture())) throw new DOMException('the share allowance gives no capture', 'NotAllowedError');
  captures++;
  return displayMedia(options);
};

const endListeners = new Set<(reason: CompanionEndReason) => void>();

const companion: CompanionEnv = {
  grant: (channel) => window.__bridge.grant(channel),
  arm: () => window.__bridge.arm(),
  end: () => window.__bridge.end(),
  onEnd: (listener) => {
    endListeners.add(listener);
    return () => {
      endListeners.delete(listener);
    };
  },
  deafened: () => false,
  onDeafened: () => () => undefined,
};

const client = new VoiceClient({
  clock: {
    setTimeout: (run, ms) => window.setTimeout(run, ms),
    clearTimeout: (handle) => {
      window.clearTimeout(handle as number);
    },
  },
  random: Math.random,
  // A companion is never joined as the person: no session of the person's own.
  createSession: () => Promise.resolve({ ok: false, failure: 'network' }),
  companion,
  report: () => Promise.resolve('failed'),
  newRoom: newLiveKitRoom,
  claimShare: (channel, quality) => window.__bridge.claim(channel, quality ?? null),
  stopShare: (channel, startedAt) => window.__bridge.release(channel, startedAt),
});

// Each change of the client's phase and share, for the test's failure message.
let last = '';
client.subscribe(() => {
  const s = client.snapshot();
  const line = `${s.phase} companion=${String(s.companion)} share=${s.share.kind} why=${s.why ?? ''} refusal=${s.companionRefusal ?? ''}`;
  if (line !== last) console.log(line);
  last = line;
});

function publications(): PublicationFact[] {
  const facts: PublicationFact[] = [];
  const room = rooms.at(-1);
  if (room === undefined) return facts;
  for (const pub of room.localParticipant.trackPublications.values()) {
    facts.push({ who: room.localParticipant.identity, local: true, source: pub.source, kind: pub.kind, subscribed: true });
  }
  for (const p of room.remoteParticipants.values()) {
    for (const pub of p.trackPublications.values()) {
      facts.push({ who: p.identity, local: false, source: pub.source, kind: pub.kind, subscribed: pub.isSubscribed });
    }
  }
  return facts;
}

// What the test drives and reads.
class CompanionPage {
  share(channel: string): void {
    client.startCompanionShare(channel);
  }

  watch(channel: string, principal: number): void {
    client.watchCompanionStream(channel, principal);
  }

  stopShare(): void {
    client.stopShare();
  }

  stopWatching(principal: number): void {
    client.watchStream(principal, false);
  }

  // main ended the companion (Companion.end told the page).
  mainEnded(reason: CompanionEndReason): void {
    for (const listener of [...endListeners]) listener(reason);
  }

  facts(): {
    readonly phase: string;
    readonly companion: boolean;
    readonly share: string;
    readonly watched: number[];
    readonly why: string | null;
    readonly captures: number;
    readonly rooms: number;
    readonly roomState: string | null;
    readonly publications: PublicationFact[];
  } {
    const s = client.snapshot();
    return {
      phase: s.phase,
      companion: s.companion,
      share: s.share.kind,
      watched: [...s.watched],
      why: s.why,
      captures,
      rooms: rooms.length,
      roomState: rooms.at(-1)?.state ?? null,
      publications: publications(),
    };
  }
}

window.companionPage = new CompanionPage();
