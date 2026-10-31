// The channel list kept in step with the server (issue #1027): the events
// socket says "your channels may have changed" and the page reads the list
// again and replaces what it holds. One read at a time: a trigger that comes
// during a read schedules one more after it, never a parallel one. A person in
// no channel has no events socket, so the list is also read when the window
// regains focus or becomes visible, and every 30 s while it is empty.
import { useCallback, useEffect, useRef } from 'react';
import * as api from './api';
import type { ChannelsAction, ChannelsState } from './state/channels';

// How often an empty list is read again.
export const emptyListInterval = 30_000;

export function useChannelRefresh({
  signedIn,
  channelsNow,
  empty,
  dispatchChannels,
  renamed,
  forget,
  onUnauthorized,
  reloadReadState,
}: {
  readonly signedIn: boolean;
  // The newest list, as the page holds it.
  readonly channelsNow: { readonly current: ChannelsState };
  // Whether the loaded list is empty.
  readonly empty: boolean;
  readonly dispatchChannels: (action: ChannelsAction) => void;
  // A channel the answer names differently from the page's copy.
  readonly renamed: (id: number, name: string) => void;
  // A channel the answer no longer lists: everything the page keeps of it is
  // dropped (uploads in flight, attachment drafts).
  readonly forget: (id: number, name: string) => void;
  readonly onUnauthorized: () => void;
  // Reads the counts again, once the list is replaced.
  readonly reloadReadState: () => void;
}): () => void {
  const running = useRef(false);
  const again = useRef(false);
  const live = useRef(false);
  const latest = useRef({ renamed, forget, onUnauthorized, reloadReadState, dispatchChannels });
  latest.current = { renamed, forget, onUnauthorized, reloadReadState, dispatchChannels };

  useEffect(() => {
    live.current = signedIn;
    running.current = false;
    again.current = false;
    return () => {
      live.current = false;
    };
  }, [signedIn]);

  const refresh = useCallback(() => {
    if (!live.current || !channelsNow.current.loaded) return;
    if (running.current) {
      again.current = true;
      return;
    }
    running.current = true;
    again.current = false;
    // The list as it stood when the read began. An answer that comes after
    // the page changed its list by another way (a 1008 loss, a rename, a
    // channel made) may be older than that change: it is not applied, and the
    // list is read once more instead.
    const began = channelsNow.current.channels;
    void api.listChannels().then((result) => {
      running.current = false;
      if (!live.current) return;
      if (result.ok && channelsNow.current.channels !== began) {
        again.current = true;
      } else if (result.ok) {
        const held = channelsNow.current.channels;
        const next = result.value;
        for (const c of next) {
          const was = held.find((h) => h.id === c.id);
          if (was !== undefined && was.name !== c.name) latest.current.renamed(c.id, c.name);
        }
        for (const was of held) {
          if (!next.some((c) => c.id === was.id)) latest.current.forget(was.id, was.name);
        }
        latest.current.dispatchChannels({ type: 'refreshed', channels: next });
        latest.current.reloadReadState();
      } else if (result.failure === 'unauthorized') {
        latest.current.onUnauthorized();
        return;
      }
      if (again.current) {
        again.current = false;
        refresh();
      }
    });
  }, [channelsNow]);

  useEffect(() => {
    if (!signedIn) return;
    const onFocus = () => {
      refresh();
    };
    const onVisible = () => {
      if (document.visibilityState === 'visible') refresh();
    };
    window.addEventListener('focus', onFocus);
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.removeEventListener('focus', onFocus);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [signedIn, refresh]);

  useEffect(() => {
    if (!signedIn || !empty) return;
    const timer = window.setInterval(refresh, emptyListInterval);
    return () => {
      window.clearInterval(timer);
    };
  }, [signedIn, empty, refresh]);

  return refresh;
}
