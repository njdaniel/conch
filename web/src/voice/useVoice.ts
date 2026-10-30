// Everything the voice section runs on: the client, the talk key, the
// presence of the channel on screen (its socket, joined or not, which the
// client also reads for the screen share while joined), and the page-level
// releases (blur, hidden, pagehide).
import { useCallback, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react';
import * as api from '../api';
import type { ChannelV0 } from '../schema/channel';
import type { VoicePresenceV1 } from '../schema/voice';
import { Feed, type FeedStatus } from '../state/feed';
import { VoiceClient, type Microphone, type Output, type ProcessingStore, type VoiceState } from './connection';
import { clampTail, defaultTail } from './gate';
import { defaultTalkKey, isTyping } from './keys';
import { parsePresence } from './presence';
import { canChooseOutput, newLiveKitRoom } from './sdk';
import { browserFeatures, type ShareFeatures } from './share';

const realClock = {
  setTimeout: (run: () => void, ms: number) => window.setTimeout(run, ms),
  clearTimeout: (handle: unknown) => {
    window.clearTimeout(handle as number);
  },
};

const keyStore = 'conch.web.voice.key';

function readKey(): string {
  try {
    return window.sessionStorage.getItem(keyStore) ?? defaultTalkKey;
  } catch {
    return defaultTalkKey;
  }
}

function writeKey(code: string): void {
  try {
    window.sessionStorage.setItem(keyStore, code);
  } catch {
    // The choice lasts until the page is reloaded.
  }
}

// The release delay (issue #965), for as long as the tab lasts, as the talk
// key is: none stored, or one that is not a number of ms, is the default.
const releaseStore = 'conch.web.voice.release';

function readRelease(): number {
  try {
    const kept = window.sessionStorage.getItem(releaseStore);
    return kept === null || !/^\d{1,4}$/.test(kept) ? defaultTail : clampTail(Number(kept));
  } catch {
    return defaultTail;
  }
}

function writeRelease(ms: number): void {
  try {
    window.sessionStorage.setItem(releaseStore, String(ms));
  } catch {
    // The choice lasts until the page is reloaded.
  }
}

// The microphone chosen (issue #658), by device id and by name, for as long
// as the tab lasts: none stored is the system default.
const microphoneStore = 'conch.web.voice.microphone';
const microphoneNameStore = 'conch.web.voice.microphone.name';

function readMicrophone(): Microphone | null {
  try {
    const id = window.sessionStorage.getItem(microphoneStore);
    if (id === null) return null;
    return { id, label: window.sessionStorage.getItem(microphoneNameStore) ?? '' };
  } catch {
    return null;
  }
}

function writeMicrophone(choice: Microphone | null): void {
  try {
    if (choice === null) {
      window.sessionStorage.removeItem(microphoneStore);
      window.sessionStorage.removeItem(microphoneNameStore);
    } else {
      window.sessionStorage.setItem(microphoneStore, choice.id);
      window.sessionStorage.setItem(microphoneNameStore, choice.label);
    }
  } catch {
    // The choice lasts until the page is reloaded.
  }
}

// The speakers chosen (issue #660), by device id and by name, for as long as
// the tab lasts: none stored is the system default.
const outputStore = 'conch.web.voice.speakers';
const outputNameStore = 'conch.web.voice.speakers.name';

function readOutput(): Output | null {
  try {
    const id = window.sessionStorage.getItem(outputStore);
    if (id === null) return null;
    return { id, label: window.sessionStorage.getItem(outputNameStore) ?? '' };
  } catch {
    return null;
  }
}

function writeOutput(choice: Output | null): void {
  try {
    if (choice === null) {
      window.sessionStorage.removeItem(outputStore);
      window.sessionStorage.removeItem(outputNameStore);
    } else {
      window.sessionStorage.setItem(outputStore, choice.id);
      window.sessionStorage.setItem(outputNameStore, choice.label);
    }
  } catch {
    // The choice lasts until the page is reloaded.
  }
}

// The voice processing chosen for each microphone (issue #659), for as long
// as the tab lasts: a device under its id, and System default under a key of
// its own. A device never set has it on.
const processingStore = 'conch.web.voice.processing';

function processingKey(device: string | null): string {
  return device === null ? `${processingStore}.system` : `${processingStore}.device:${device}`;
}

function readProcessing(device: string | null): boolean {
  try {
    return window.sessionStorage.getItem(processingKey(device)) !== 'off';
  } catch {
    return true;
  }
}

function writeProcessing(device: string | null, on: boolean): void {
  try {
    window.sessionStorage.setItem(processingKey(device), on ? 'on' : 'off');
  } catch {
    // The choice lasts until the page is reloaded.
  }
}

// processingChoices reads and keeps the choices in the tab, and holds them
// in memory too, so that they last until a reload where storage refuses.
function processingChoices(): ProcessingStore {
  const known = new Map<string | null, boolean>();
  return {
    get: (device) => known.get(device) ?? readProcessing(device),
    set: (device, on) => {
      known.set(device, on);
      writeProcessing(device, on);
    },
  };
}

// What is known about the channel's voice, as the strip says it. loading is
// before the first frame; presence is a frame on an open socket; unreachable
// is the socket lost and not back, so the server cannot be reached;
// not_allowed is a person whose access has no voice; gone is any other close
// the server meant (the channel lost, which the page says elsewhere).
export type Snapshot =
  | { readonly kind: 'loading' }
  | { readonly kind: 'unreachable' }
  | { readonly kind: 'not_allowed' }
  | { readonly kind: 'gone' }
  | { readonly kind: 'presence'; readonly presence: VoicePresenceV1 };

// How every voice route refuses a person whose invite did not grant voice
// (schema.ErrorCodeVoiceNotAllowed).
const voiceNotAllowed = 'voice_not_allowed';

export interface VoiceControl {
  readonly state: VoiceState;
  readonly snapshot: Snapshot;
  // The channel whose presence is shown: the one joined, else the one on screen.
  readonly shown: string | null;
  readonly talkKey: string;
  readonly capturing: boolean;
  readonly secure: boolean;
  // What this browser can share, and with what sound.
  readonly features: ShareFeatures;
  readonly client: VoiceClient;
  readonly setTalkKey: (code: string) => void;
  // Sets the release delay, and keeps it for the tab.
  readonly setReleaseDelay: (ms: number) => void;
  readonly capture: (on: boolean) => void;
  readonly join: () => void;
  // Joins to watch, without the microphone.
  readonly watchOnly: () => void;
  readonly leave: () => void;
}

const noChannels: readonly ChannelV0[] = [];

export function useVoice({
  signedIn,
  channel,
  channels = noChannels,
  voiceAllowed,
  reachable,
  onUnauthorized,
}: {
  signedIn: boolean;
  channel: string | null;
  // The person's channels, by id and name as the page holds them now: how a
  // channel renamed while shown or joined (issue #728) is known to be the same
  // channel, so its presence socket stays open and voice stays joined.
  channels?: readonly ChannelV0[];
  // Whether this person may use voice at all: a human whose access includes
  // it. Without it no presence is asked for.
  voiceAllowed: boolean;
  // Whether conchd is reached: the status line's Connected.
  reachable: boolean;
  onUnauthorized: () => void;
}): VoiceControl {
  const clientRef = useRef<VoiceClient | null>(null);
  if (clientRef.current === null) {
    clientRef.current = new VoiceClient({
      clock: realClock,
      random: Math.random,
      createSession: api.createVoiceSession,
      report: api.reportVoiceTransmit,
      newRoom: newLiveKitRoom,
      claimShare: api.claimVoiceShare,
      stopShare: api.stopVoiceShare,
      microphone: readMicrophone(),
      processing: processingChoices(),
      outputSelection: canChooseOutput(),
      output: readOutput(),
      gate: { tail: readRelease() },
    });
  }
  const client = clientRef.current;
  const state = useSyncExternalStore(client.subscribe, client.snapshot);
  const [talkKey, setKey] = useState(readKey);
  const [capturing, setCapturing] = useState(false);
  const [snapshot, setSnapshot] = useState<Snapshot>({ kind: 'loading' });
  const [features] = useState(browserFeatures);
  // The presence feed of the shown channel, what it last reported and the
  // presence it last gave, for the effects beside the one that owns it.
  const feedRef = useRef<Feed | null>(null);
  const feedStatus = useRef<FeedStatus | null>(null);
  const latest = useRef<VoicePresenceV1 | null>(null);
  const joinedRef = useRef<string | null>(null);
  // Whether the feed is lost and not back: the server out of reach.
  const lost = useRef(false);

  const joined = state.phase !== 'idle' && state.phase !== 'stopped' ? state.channel : null;
  const shown = joined ?? (signedIn ? channel : null);
  // The id of the channel joined, taken at the join, and of the one shown.
  // The presence socket follows the shown channel by id where the list names
  // it, so a rename does not reopen it; by name where it does not.
  const joinedAt = useRef<number | null>(null);
  const idOf = (name: string | null) => (name === null ? null : (channels.find((c) => c.name === name)?.id ?? null));
  const shownId = joined !== null ? joinedAt.current : idOf(shown);
  const shownKey = shownId === null ? shown : `#${String(shownId)}`;
  // The shown channel's name as of this render, for the socket's next attempt.
  const shownName = useRef(shown);
  shownName.current = shown;

  // The channel joined renamed: the client is told, before the page is
  // painted with the old name, and asks conchd by the new one from then on.
  useLayoutEffect(() => {
    if (joined === null) {
      joinedAt.current = null;
      return;
    }
    const id = joinedAt.current;
    const now = id === null ? undefined : channels.find((c) => c.id === id)?.name;
    if (now !== undefined && now !== joined) client.renamed(joined, now);
  }, [joined, channels, client]);

  // The microphone chosen is kept for the tab: the person's choice, the same
  // device found again under a new id, and the client letting go of one that
  // was not there at a join.
  useEffect(() => {
    writeMicrophone(state.microphone);
  }, [state.microphone]);

  // So are the speakers: the person's choice, the same output found again
  // under a new id, the one the SDK moved sound to, and the system default
  // after one the browser refused.
  useEffect(() => {
    writeOutput(state.output);
  }, [state.output]);

  // Signing out ends voice.
  useEffect(() => {
    if (!signedIn) void client.leave();
  }, [signedIn, client]);
  useEffect(
    () => () => {
      client.dispose();
    },
    [client],
  );

  // Presence of the shown channel, from its socket, whether or not this page
  // is in voice: the roster of a channel not joined is as live as the one
  // joined. The effect follows the shown channel, not the joining, so joining
  // or leaving in the channel on screen keeps the socket it has.
  useEffect(() => {
    if (shownKey === null) return;
    const named = () => shownName.current ?? shownKey;
    latest.current = null;
    if (!voiceAllowed) {
      // conchd refuses this person's socket before the upgrade, which reaches
      // the page as a close 1006 that the feed would retry for ever: none is
      // opened, and nothing is read.
      setSnapshot({ kind: 'not_allowed' });
      return;
    }
    let live = true;
    lost.current = false;
    setSnapshot({ kind: 'loading' });
    const show = (presence: VoicePresenceV1) => {
      latest.current = presence;
      lost.current = false;
      setSnapshot({ kind: 'presence', presence });
      if (joinedRef.current !== null) client.presence(presence);
    };
    const set = (kind: 'not_allowed' | 'gone') => {
      latest.current = null;
      lost.current = false;
      setSnapshot({ kind });
    };
    // Out of reach, a roster from before may name people who have left
    // since, so the strip not in voice shows none. In voice it keeps the last
    // one, as before: the room itself may still be connected, and a share
    // watched is not taken away for a blip of conchd's. Leaving while lost
    // shows the strip as lost (the effect on `joined` below).
    const unreachable = () => {
      lost.current = true;
      if (joinedRef.current === null) {
        latest.current = null;
        setSnapshot({ kind: 'unreachable' });
      }
    };
    const feed = new Feed({
      channel: named(),
      clock: realClock,
      after: () => 0,
      // The voice socket sends a frame only when presence changes, so the
      // silence watchdog is long: a reconnect is a fresh snapshot.
      silence: 300_000,
      // Each attempt names the channel as it is called then.
      open: (_channel, _after, events) => api.openVoiceSocket(named(), events),
      onFrame: (data) => {
        const presence = parsePresence(data);
        if (presence === null || !live) return;
        show(presence);
      },
      onStatus: (status: FeedStatus) => {
        if (!live) return;
        // The first attempt is still loading; a wait, or an attempt after
        // one, is the server out of reach.
        const first = feedStatus.current === null;
        feedStatus.current = status;
        if (status === 'unauthorized') onUnauthorized();
        else if (status === 'waiting' || (status === 'connecting' && !first)) unreachable();
        else if (status === 'denied') settle();
      },
    });
    // A 1008 close says only that the server meant it. One read says why:
    // voice withdrawn, the channel lost, or neither (it answers), when the
    // feed goes back through its backoff rather than retrying at once.
    const settle = () => {
      void api.getVoicePresence(named()).then((result) => {
        if (!live) return;
        if (result.ok) {
          show(result.value);
          feed.retryLater();
        } else if (result.failure === 'unauthorized') onUnauthorized();
        else set(result.code === voiceNotAllowed ? 'not_allowed' : 'gone');
      });
    };
    feedStatus.current = null;
    feedRef.current = feed;
    feed.start();
    return () => {
      live = false;
      feed.stop();
      feedRef.current = null;
      feedStatus.current = null;
    };
  }, [shownKey, voiceAllowed, onUnauthorized, client]);

  // While in voice the client reads presence too, for the screen share. On
  // joining it is given what the socket last said: the socket is not opened
  // again, and says nothing more until something changes. Leaving while the
  // socket is lost leaves a strip that says so, not the roster kept in voice.
  useEffect(() => {
    joinedRef.current = joined;
    if (joined === null && lost.current) {
      latest.current = null;
      setSnapshot({ kind: 'unreachable' });
    } else if (joined !== null && latest.current !== null) client.presence(latest.current);
  }, [joined, client]);

  // Back in reach (the status line says Connected): a feed waiting out its
  // backoff, up to 30 s, tries again at once.
  useEffect(() => {
    if (reachable && feedStatus.current === 'waiting') feedRef.current?.start();
  }, [reachable]);

  // The talk key, only while the tab has focus and focus is not in a field.
  useEffect(() => {
    if (!signedIn || capturing) return;
    const down = (e: KeyboardEvent) => {
      if (e.code !== talkKey || e.repeat || e.ctrlKey || e.altKey || e.metaKey || isTyping(e.target)) return;
      e.preventDefault();
      client.press();
    };
    const up = (e: KeyboardEvent) => {
      if (e.code === talkKey) client.release();
    };
    const away = () => {
      client.lost();
    };
    const hidden = () => {
      if (document.visibilityState === 'hidden') client.lost();
    };
    window.addEventListener('keydown', down);
    window.addEventListener('keyup', up);
    window.addEventListener('blur', away);
    window.addEventListener('pagehide', away);
    document.addEventListener('visibilitychange', hidden);
    return () => {
      // A key held while the key, capture or sign-in changes would never see
      // its keyup here: let go of it.
      client.release();
      window.removeEventListener('keydown', down);
      window.removeEventListener('keyup', up);
      window.removeEventListener('blur', away);
      window.removeEventListener('pagehide', away);
      document.removeEventListener('visibilitychange', hidden);
    };
  }, [signedIn, capturing, talkKey, client]);

  const setTalkKey = useCallback((code: string) => {
    writeKey(code);
    setKey(code);
    setCapturing(false);
  }, []);
  const setReleaseDelay = useCallback((ms: number) => {
    client.setReleaseDelay(ms);
    writeRelease(client.snapshot().releaseDelay);
  }, [client]);
  // The id of the channel joined is noted first, so the render the join
  // causes already follows it by id.
  const channelsNow = useRef(channels);
  channelsNow.current = channels;
  const note = useCallback((name: string) => {
    if (client.snapshot().phase !== 'idle' && client.snapshot().phase !== 'stopped') return;
    joinedAt.current = channelsNow.current.find((c) => c.name === name)?.id ?? null;
  }, [client]);
  const join = useCallback(() => {
    if (channel === null) return;
    note(channel);
    client.join(channel);
  }, [channel, client, note]);
  const watchOnly = useCallback(() => {
    if (channel === null) return;
    note(channel);
    client.join(channel, { watchOnly: true });
  }, [channel, client, note]);
  const leave = useCallback(() => {
    void client.leave();
  }, [client]);

  return {
    state,
    snapshot,
    shown,
    talkKey,
    capturing,
    secure: window.isSecureContext,
    features,
    client,
    setTalkKey,
    setReleaseDelay,
    capture: setCapturing,
    join,
    watchOnly,
    leave,
  };
}
