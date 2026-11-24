// Presence of the channel shown, for the native voice strip (issue #1376, D3-5
// of #771): the same voice presence socket the browser strip reads, through
// the platform's socket transport (on the desktop, a feed main holds), open
// for the channel shown whether or not voice is joined. It reads only
// presence; it never opens a room or asks for a device.
import { useEffect, useRef, useState } from 'react';
import * as api from '../../api';
import { soundAudience, sounds, voiceSounds } from '../../sounds';
import type { VoicePresence } from '../../schema/voice';
import { Feed, type FeedStatus } from '../../state/feed';
import { parsePresence } from '../presence';
import type { Snapshot } from '../useVoice';

type NativeSnapshot =
  | Exclude<Snapshot, { kind: 'presence' }>
  | (Extract<Snapshot, { kind: 'presence' }> & { readonly socketSchema: VoicePresence['schema'] | null });

const realClock = {
  setTimeout: (run: () => void, ms: number) => window.setTimeout(run, ms),
  clearTimeout: (handle: unknown) => {
    window.clearTimeout(handle as number);
  },
};

// The reason conchd gives when it closes a voice socket (1008) because its
// reader is no longer a member of the channel.
const noLongerMember = 'no longer a member of this channel';
const voiceNotAllowed = 'voice_not_allowed';

export function usePresence({
  channel,
  joined,
  voiceAllowed,
  onUnauthorized,
}: {
  // The channel whose presence is shown; null asks for none.
  readonly channel: string | null;
  // Only this app’s joined channel may make interface cues, not a roster
  // browsed while this account is in voice on another client.
  readonly joined: string | null;
  readonly voiceAllowed: boolean;
  readonly onUnauthorized: () => void;
}): NativeSnapshot {
  const [snapshot, setSnapshot] = useState<NativeSnapshot>({ kind: 'loading' });
  const unauthorized = useRef(onUnauthorized);
  unauthorized.current = onUnauthorized;
  const joinedRef = useRef(joined);
  joinedRef.current = joined;

  useEffect(() => {
    if (channel === null) {
      setSnapshot({ kind: 'loading' });
      return;
    }
    if (!voiceAllowed) {
      setSnapshot({ kind: 'not_allowed' });
      return;
    }
    let live = true;
    let first = true;
    let closeReason = '';
    let previous: VoicePresence | null = null;
    // A follow-up read can replace the roster before React delivers the frame.
    let socketSchema: VoicePresence['schema'] | null = null;
    const show = (presence: VoicePresence) => {
      const audience = soundAudience;
      if (joinedRef.current === channel && audience?.allows(presence.channel_id)) {
        voiceSounds(previous, presence, audience.me, (sound) => { sounds.play(sound); });
      }
      previous = presence;
      setSnapshot({ kind: 'presence', presence, socketSchema });
    };
    setSnapshot({ kind: 'loading' });
    const feed = new Feed({
      channel,
      clock: realClock,
      after: () => 0,
      // The voice socket sends a frame only when presence changes.
      silence: 300_000,
      open: (_channel, _after, events) =>
        api.openVoiceSocket(channel, {
          ...events,
          onClose: (code, why) => {
            closeReason = why ?? '';
            events.onClose(code, why);
          },
        }),
      onFrame: (data) => {
        const presence: VoicePresence | null = parsePresence(data);
        if (presence === null || !live) return;
        socketSchema = presence.schema;
        show(presence);
      },
      onStatus: (status: FeedStatus) => {
        if (!live) return;
        const was = first;
        first = false;
        if (status === 'unauthorized') unauthorized.current();
        else if (status === 'waiting' || (status === 'connecting' && !was)) {
          // A reconnect is a fresh baseline, never a backlog of cues.
          previous = null;
          setSnapshot({ kind: 'unreachable' });
        } else if (status === 'denied') settle();
      },
    });
    // A 1008 close says only that the server meant it; one read says why.
    // An answered roster or a network failure goes through the backoff.
    const settle = () => {
      previous = null;
      if (closeReason === noLongerMember) {
        setSnapshot({ kind: 'gone' });
        return;
      }
      void api.getVoicePresence(channel).then((result) => {
        if (!live) return;
        if (result.ok) {
          // retryLater reports waiting synchronously. Show the read after it
          // so the roster stays visible during the backoff.
          feed.retryLater();
          show(result.value);
        } else if (result.failure === 'unauthorized') unauthorized.current();
        else if (result.failure === 'network') feed.retryLater();
        else setSnapshot({ kind: result.code === voiceNotAllowed ? 'not_allowed' : 'gone' });
      });
    };
    feed.start();
    return () => {
      live = false;
      feed.stop();
    };
  }, [channel, voiceAllowed]);

  return snapshot;
}
