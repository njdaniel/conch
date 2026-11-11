// Presence of the channel shown, for the native voice strip (issue #1376, D3-5
// of #771): the same voice presence socket the browser strip reads, through
// the platform's socket transport (on the desktop, a feed main holds), open
// for the channel shown whether or not voice is joined. It reads only
// presence; it never opens a room or asks for a device.
import { useEffect, useRef, useState } from 'react';
import * as api from '../../api';
import type { VoicePresenceV2 } from '../../schema/voice';
import { Feed, type FeedStatus } from '../../state/feed';
import { parsePresence } from '../presence';
import type { Snapshot } from '../useVoice';

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
  voiceAllowed,
  onUnauthorized,
}: {
  // The channel whose presence is shown; null asks for none.
  readonly channel: string | null;
  readonly voiceAllowed: boolean;
  readonly onUnauthorized: () => void;
}): Snapshot {
  const [snapshot, setSnapshot] = useState<Snapshot>({ kind: 'loading' });
  const unauthorized = useRef(onUnauthorized);
  unauthorized.current = onUnauthorized;

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
        const presence: VoicePresenceV2 | null = parsePresence(data);
        if (presence === null || !live) return;
        setSnapshot({ kind: 'presence', presence });
      },
      onStatus: (status: FeedStatus) => {
        if (!live) return;
        const was = first;
        first = false;
        if (status === 'unauthorized') unauthorized.current();
        else if (status === 'waiting' || (status === 'connecting' && !was)) setSnapshot({ kind: 'unreachable' });
        else if (status === 'denied') settle();
      },
    });
    // A 1008 close says only that the server meant it; one read says why.
    const settle = () => {
      if (closeReason === noLongerMember) {
        setSnapshot({ kind: 'gone' });
        return;
      }
      void api.getVoicePresence(channel).then((result) => {
        if (!live) return;
        if (result.ok) {
          setSnapshot({ kind: 'presence', presence: result.value });
          feed.retryLater();
        } else if (result.failure === 'unauthorized') unauthorized.current();
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
