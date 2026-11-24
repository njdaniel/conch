import { createContext, useCallback, useContext, useLayoutEffect, useRef, type RefObject } from 'react';
import type { ChannelV0 } from '../schema/channel';
import type { VoicePresence } from '../schema/voice';
import type { Snapshot } from './useVoice';

export type ChannelJoinHandler = (channel: ChannelV0) => void;

// The sidebar calls the voice strip within the double-click gesture. The
// strip owns join eligibility and the join itself, just as it does for its button.
export const ChannelJoin = createContext<RefObject<ChannelJoinHandler | null> | null>(null);

// A fresh selection may still be loading voice presence on the second click.
// Give that socket a short chance to answer, within main's 1s gesture window.
// The timer can only cancel a request. The strip always offers its Join button.
export const shortcutWait = 750;

interface PendingJoin {
  readonly id: number;
  readonly name: string;
  readonly audio: AudioContext | null;
  readonly timer: number;
  readonly expires: number;
}

export function useChannelJoin(
  channel: ChannelV0 | undefined,
  enabled: boolean,
  snapshot: Snapshot,
  join: (presence: VoicePresence, audio: AudioContext | null) => void,
  openAudio?: () => AudioContext | null,
) {
  const handler = useContext(ChannelJoin);
  const pending = useRef<PendingJoin | null>(null);
  const take = useCallback(() => {
    const request = pending.current;
    pending.current = null;
    if (request !== null) window.clearTimeout(request.timer);
    return request;
  }, []);
  const cancel = useCallback(() => {
    const audio = take()?.audio;
    if (audio !== null && audio !== undefined) void audio.close().catch(() => undefined);
  }, [take]);

  useLayoutEffect(() => () => { cancel(); }, [cancel]);
  useLayoutEffect(() => {
    if (handler === null) return;
    handler.current = (target) => {
      if (!enabled || channel?.id !== target.id || channel.name !== target.name || pending.current !== null) return;
      if (snapshot.kind === 'presence' && snapshot.presence.channel_id === target.id) {
        const { presence } = snapshot;
        if (presence.configured && presence.available) join(presence, openAudio?.() ?? null);
      } else if (snapshot.kind === 'loading' || snapshot.kind === 'presence') {
        const expires = performance.now() + shortcutWait;
        const audio = openAudio?.() ?? null;
        pending.current = {
          id: target.id,
          name: target.name,
          audio,
          timer: window.setTimeout(cancel, shortcutWait),
          expires,
        };
      }
    };
    return () => {
      handler.current = null;
    };
  }, [handler, channel, enabled, snapshot, join, openAudio, cancel]);

  useLayoutEffect(() => {
    const request = pending.current;
    if (request === null) return;
    if (!enabled || channel?.id !== request.id || channel.name !== request.name || performance.now() >= request.expires) {
      cancel();
      return;
    }
    if (snapshot.kind === 'loading') return;
    if (snapshot.kind === 'presence') {
      const { presence } = snapshot;
      if (presence.channel_id !== request.id) return;
      if (presence.configured && presence.available) {
        take();
        join(presence, request.audio);
        return;
      }
    }
    cancel();
  }, [channel, enabled, snapshot, join, cancel, take]);
}
