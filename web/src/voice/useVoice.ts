// Everything the voice section runs on: the client, the talk key, the
// presence of the channel on screen (one read before joining, the socket
// after), and the page-level releases (blur, hidden, pagehide).
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react';
import * as api from '../api';
import type { VoicePresenceV1 } from '../schema/voice';
import { Feed, type FeedStatus } from '../state/feed';
import { VoiceClient, type VoiceState } from './connection';
import { defaultTalkKey, isTyping } from './keys';
import { parsePresence } from './presence';
import { newLiveKitRoom } from './sdk';

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

// What is known about the channel's voice before anyone has joined.
export type Snapshot =
  | { readonly kind: 'loading' }
  | { readonly kind: 'unknown' }
  | { readonly kind: 'presence'; readonly presence: VoicePresenceV1 };

export interface VoiceControl {
  readonly state: VoiceState;
  readonly snapshot: Snapshot;
  // The channel whose presence is shown: the one joined, else the one on screen.
  readonly shown: string | null;
  readonly talkKey: string;
  readonly capturing: boolean;
  readonly secure: boolean;
  readonly client: VoiceClient;
  readonly setTalkKey: (code: string) => void;
  readonly capture: (on: boolean) => void;
  readonly join: () => void;
  readonly leave: () => void;
}

export function useVoice({ signedIn, channel, onUnauthorized }: { signedIn: boolean; channel: string | null; onUnauthorized: () => void }): VoiceControl {
  const clientRef = useRef<VoiceClient | null>(null);
  if (clientRef.current === null) {
    clientRef.current = new VoiceClient({
      clock: realClock,
      random: Math.random,
      createSession: api.createVoiceSession,
      report: api.reportVoiceTransmit,
      newRoom: newLiveKitRoom,
    });
  }
  const client = clientRef.current;
  const state = useSyncExternalStore(client.subscribe, client.snapshot);
  const [talkKey, setKey] = useState(readKey);
  const [capturing, setCapturing] = useState(false);
  const [snapshot, setSnapshot] = useState<Snapshot>({ kind: 'loading' });

  const joined = state.phase !== 'idle' && state.phase !== 'stopped' ? state.channel : null;
  const shown = joined ?? (signedIn ? channel : null);

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

  // Presence of the shown channel.
  useEffect(() => {
    if (shown === null) return;
    let live = true;
    setSnapshot({ kind: 'loading' });
    if (joined === null) {
      void api.getVoicePresence(shown).then((result) => {
        if (!live) return;
        if (result.ok) setSnapshot({ kind: 'presence', presence: result.value });
        else if (result.failure === 'unauthorized') onUnauthorized();
        else setSnapshot({ kind: 'unknown' });
      });
      return () => {
        live = false;
      };
    }
    const feed = new Feed({
      channel: shown,
      clock: realClock,
      after: () => 0,
      // The voice socket sends a frame only when presence changes, so the
      // silence watchdog is long: a reconnect is a fresh snapshot.
      silence: 300_000,
      open: (c, _after, events) => api.openVoiceSocket(c, events),
      onFrame: (data) => {
        const presence = parsePresence(data);
        if (presence !== null && live) setSnapshot({ kind: 'presence', presence });
      },
      onStatus: (status: FeedStatus) => {
        if (status === 'unauthorized') onUnauthorized();
      },
    });
    feed.start();
    return () => {
      live = false;
      feed.stop();
    };
  }, [shown, joined, onUnauthorized]);

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
  const join = useCallback(() => {
    if (channel !== null) client.join(channel);
  }, [channel, client]);
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
    client,
    setTalkKey,
    capture: setCapturing,
    join,
    leave,
  };
}
