// Voice occupancy (issue #596, docs/design/voice-occupancy.md §6): how many
// people are in each channel's voice room, for every channel the viewer may
// see voice in, so that "meet in voice" can be found from any channel. One
// socket per signed-in page, not one per channel.
//
// It is counts and nothing else. It joins nothing: this file reaches neither
// the SDK nor the voice client, and asks for no device. Seeing that people
// are in voice somewhere only says so; opening that channel is the reader's
// choice, and joining it is the strip's Join voice, as everywhere.
import { useEffect, useRef, useState } from 'react';
import * as api from '../api';
import { voiceOccupancyV1, type VoiceOccupancyV1 } from '../schema/voice';
import { Feed, type FeedStatus } from '../state/feed';

const realClock = {
  setTimeout: (run: () => void, ms: number) => window.setTimeout(run, ms),
  clearTimeout: (handle: unknown) => {
    window.clearTimeout(handle as number);
  },
};

// How many are in each channel's voice, by channel id. A channel with nobody
// in voice, or one the viewer may not see voice in, has no entry.
export type InVoice = ReadonlyMap<number, number>;

export const nobody: InVoice = new Map();

// parseOccupancy reads one socket frame; null for anything that is not an
// occupancy document of this version.
export function parseOccupancy(data: unknown): VoiceOccupancyV1 | null {
  if (typeof data !== 'string') return null;
  try {
    const body: unknown = JSON.parse(data);
    return voiceOccupancyV1(body).length === 0 ? (body as VoiceOccupancyV1) : null;
  } catch {
    return null;
  }
}

// countsOf is what a document says, as the page shows it: the channels with
// someone in voice. Voice not set up, or LiveKit out of reach, is nobody
// anywhere: a count last seen is not shown as if it were current.
export function countsOf(doc: VoiceOccupancyV1): InVoice {
  if (!doc.configured || !doc.available) return nobody;
  const counts = new Map<number, number>();
  for (const c of doc.channels) if (c.in_voice > 0) counts.set(c.channel_id, c.in_voice);
  return counts;
}

// same says whether two sets of counts are equal, so that a frame that
// changes nothing the page shows does not draw it again.
function same(a: InVoice, b: InVoice): boolean {
  if (a.size !== b.size) return false;
  for (const [id, n] of a) if (b.get(id) !== n) return false;
  return true;
}

// How long after a 1008 close the read that says why is made.
export const settleDelay = 1000;

export function useOccupancy({
  signedIn,
  voiceAllowed,
  reachable,
  onUnauthorized,
}: {
  readonly signedIn: boolean;
  // Whether the viewer may see and use voice: a human whose access includes
  // it. Without it no socket is opened and nothing is read.
  readonly voiceAllowed: boolean;
  // Whether conchd is reached: the status line's Connected.
  readonly reachable: boolean;
  readonly onUnauthorized: () => void;
}): InVoice {
  const [counts, setCounts] = useState<InVoice>(nobody);
  const feedRef = useRef<Feed | null>(null);
  const feedStatus = useRef<FeedStatus | null>(null);

  useEffect(() => {
    setCounts(nobody);
    // conchd refuses a person without voice (and an agent) before the
    // upgrade, which reaches the page as a close 1006 that the feed would
    // retry for ever (#585): no socket is opened for them.
    if (!signedIn || !voiceAllowed) return;
    let live = true;
    const show = (doc: VoiceOccupancyV1) => {
      const next = countsOf(doc);
      setCounts((prev) => (same(prev, next) ? prev : next));
    };
    // Whether the counts shown are from a read made since the last attempt.
    let fresh = false;
    const feed = new Feed({
      // The socket is the viewer's, not a channel's.
      channel: '',
      clock: realClock,
      after: () => 0,
      // A frame comes only when a count changes, so the silence watchdog is
      // long: a reconnect is a fresh document.
      silence: 300_000,
      open: (_channel, _after, events) => api.openOccupancySocket(events),
      onFrame: (data) => {
        const doc = parseOccupancy(data);
        if (doc === null || !live) return;
        show(doc);
      },
      onStatus: (status: FeedStatus) => {
        if (!live) return;
        feedStatus.current = status;
        if (status === 'unauthorized') onUnauthorized();
        else if (status === 'connecting') fresh = false;
        // Out of reach, the counts from before may be wrong by now: none are
        // shown until the socket is back. A wait after a read that answered
        // keeps what the read said.
        else if (status === 'waiting' && !fresh) setCounts(nobody);
        else if (status === 'denied') settle();
      },
    });
    // A 1008 close says only that the server meant it. One read says why. A
    // refusal (voice withdrawn, say) is the answer, and nothing more is
    // asked; a read that answers, or that got no answer, sends the feed back
    // through its backoff rather than retrying at once.
    //
    // The read waits a second first. This socket is closed the moment a
    // credential is revoked, and a sign out everywhere from another tab of
    // this browser is said by that tab ("Signed out in another tab") once its
    // own answer comes: a read at once would get its 401 first and say the
    // credential stopped working instead.
    let pause: unknown = null;
    const settle = () => {
      setCounts(nobody);
      pause = realClock.setTimeout(() => {
        pause = null;
        void api.getVoiceOccupancy().then((result) => {
          if (!live) return;
          if (result.ok) {
            show(result.value);
            fresh = true;
            feed.retryLater();
          } else if (result.failure === 'unauthorized') onUnauthorized();
          else if (result.code === undefined) feed.retryLater();
        });
      }, settleDelay);
    };
    feedStatus.current = null;
    feedRef.current = feed;
    feed.start();
    return () => {
      live = false;
      if (pause !== null) realClock.clearTimeout(pause);
      feed.stop();
      feedRef.current = null;
      feedStatus.current = null;
    };
  }, [signedIn, voiceAllowed, onUnauthorized]);

  // Back in reach (the status line says Connected): a feed waiting out its
  // backoff, up to 30 s, tries again at once.
  useEffect(() => {
    if (reachable && feedStatus.current === 'waiting') feedRef.current?.start();
  }, [reachable]);

  return signedIn && voiceAllowed ? counts : nobody;
}
