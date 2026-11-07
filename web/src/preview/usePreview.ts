// The preview of Preview as member (issue #1517): one at a time, in this tab's
// memory, on the administrator's Session. It never touches storage and holds
// no timer, socket or heartbeat of its own: the administrator's sockets keep
// running as the administrator, and their content-free signals call signal()
// to read the target's view again.
//
// Every answer is for the epoch it was asked in. Starting, exiting, ending
// and every read of the whole view move the epoch, so a late answer for a
// preview that is over, or for a view since replaced, is dropped and can fill
// nothing.
import { useCallback, useEffect, useRef, useState } from 'react';
import * as api from '../api';
import { loadPreview, type PreviewLoad, type PreviewStore } from './store';

export interface PreviewTarget {
  readonly id: number;
  readonly name: string;
}

export type PreviewState =
  | { readonly phase: 'idle' }
  | { readonly phase: 'loading'; readonly target: PreviewTarget }
  // problem says a read since the store was made did not reach the server;
  // the store shown is the last one read.
  | { readonly phase: 'on'; readonly store: PreviewStore; readonly selected: string | null; readonly problem: string | null };

export const cannotReach = 'Cannot reach the server. The preview shown is the last one read.';
export const endedLine = (name: string) => `Preview ended: ${name} can no longer be previewed.`;
export const lostStandingLine = 'Preview ended: you can no longer preview members.';
export const voiceStartedLine = 'Preview ended: voice started. Leave voice before previewing.';
export const failedStartLine = (name: string) => `Cannot preview ${name}: the server did not answer. Nothing was changed.`;

export interface UsePreview {
  readonly state: PreviewState;
  // Why the last preview ended or did not start, shown once in the normal view.
  readonly notice: string | null;
  readonly dismissNotice: () => void;
  readonly start: (target: PreviewTarget) => void;
  readonly exit: () => void;
  // The view read again and replaced, whole.
  readonly refresh: () => void;
  // The channel whose people the panel shows.
  readonly select: (channel: string) => void;
  // The server's word that the view may have moved: read again quietly.
  readonly signal: () => void;
}

export function usePreview({
  signedIn,
  mayPreview,
  inVoice,
  onUnauthorized,
}: {
  readonly signedIn: boolean;
  // Whether the administrator holds what Preview as member needs now.
  readonly mayPreview: boolean;
  // Whether the administrator is in voice. A preview never starts then, and
  // one open when voice starts ends: the two are not mixed.
  readonly inVoice: boolean;
  readonly onUnauthorized: () => void;
}): UsePreview {
  const [state, setState] = useState<PreviewState>({ phase: 'idle' });
  const [notice, setNotice] = useState<string | null>(null);
  const now = useRef<PreviewState>(state);
  now.current = state;
  const epoch = useRef(0);
  const latest = useRef(onUnauthorized);
  latest.current = onUnauthorized;
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
      epoch.current++;
    };
  }, []);

  const inVoiceNow = useRef(inVoice);
  inVoiceNow.current = inVoice;
  const set = useCallback((next: PreviewState) => {
    now.current = next;
    setState(next);
  }, []);

  // end drops the preview store and says why.
  const end = useCallback(
    (line: string | null) => {
      epoch.current++;
      set({ phase: 'idle' });
      setNotice(line);
    },
    [set],
  );

  // settle is what a failed read comes to. A refusal ends the preview; no
  // answer leaves what is shown, with a line; a 401 signs the page out.
  const settle = useCallback(
    (result: Extract<PreviewLoad, { ok: false }>, name: string, shown: PreviewState) => {
      if (result.failure === 'unauthorized') {
        end(null);
        latest.current();
      } else if (result.failure === 'refused') {
        end(endedLine(name));
      } else if (shown.phase === 'on') {
        set({ ...shown, problem: cannotReach });
      } else {
        end(failedStartLine(name));
      }
    },
    [end, set],
  );

  const start = useCallback(
    (target: PreviewTarget) => {
      if (now.current.phase !== 'idle' || inVoiceNow.current) return;
      const mine = ++epoch.current;
      setNotice(null);
      set({ phase: 'loading', target });
      void loadPreview(api.previewAs(target.id), null, Date.now())
        .then((result) => {
          if (mine !== epoch.current || !live.current) return;
          if (result.ok) set({ phase: 'on', store: result.store, selected: result.store.channels[0]?.name ?? null, problem: null });
          else settle(result, target.name, now.current);
        });
    },
    [set, settle],
  );

  const read = useCallback(
    (open: string | null) => {
      const shown = now.current;
      if (shown.phase !== 'on') return;
      const mine = ++epoch.current;
      const target = shown.store.target;
      void loadPreview(api.previewAs(target.id), open ?? shown.selected, Date.now()).then((result) => {
        if (mine !== epoch.current || !live.current) return;
        if (!result.ok) {
          settle(result, target.name, now.current);
          return;
        }
        const names = result.store.channels.map((c) => c.name);
        const wanted = open ?? shown.selected;
        set({ phase: 'on', store: result.store, selected: wanted !== null && names.includes(wanted) ? wanted : (names[0] ?? null), problem: null });
      });
    },
    [set, settle],
  );

  const refresh = useCallback(() => {
    read(null);
  }, [read]);

  const select = useCallback(
    (channel: string) => {
      const shown = now.current;
      if (shown.phase !== 'on' || shown.selected === channel) return;
      set({ ...shown, selected: channel });
      // The people of that channel as the target sees them; the whole view is
      // read again with it, so nothing shown is older than the rest.
      read(channel);
    },
    [read, set],
  );

  const exit = useCallback(() => {
    end(null);
  }, [end]);

  // Losing the standing, or the sign-in, ends the preview at once.
  useEffect(() => {
    if (state.phase === 'idle') return;
    if (!signedIn) end(null);
    else if (!mayPreview) end(lostStandingLine);
    else if (inVoice) end(voiceStartedLine);
  }, [signedIn, mayPreview, inVoice, state.phase, end]);

  return { state, notice, dismissNotice: () => { setNotice(null); }, start, exit, refresh, select, signal: refresh };
}
