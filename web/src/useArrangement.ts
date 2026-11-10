import { useEffect, useMemo, useRef, useState } from 'react';
import * as api from './api';
import { emptyArrangement, parseArrangement, pruneArrangement, type Arrangement } from './arrangement';
import type { ChannelV0 } from './schema/channel';

// The signed-in person's channel arrangement (issue #1493): read for this
// account on this server, shown pruned to the channels they hold now, and
// written back when they change it or when pruning dropped something. Another
// account or server reads its own, and nothing of the last is left. Nothing is
// written for no account (id 0).

const load = (origin: string, account: number): Arrangement => (account === 0 ? emptyArrangement : parseArrangement(api.readArrangement(origin, account)));

export interface ArrangementState {
  // What to draw: pruned to the channels held, once the list has been read.
  readonly arrangement: Arrangement;
  readonly edit: (change: (a: Arrangement) => Arrangement) => void;
}

export function useArrangement(origin: string, account: number, channels: readonly ChannelV0[], loaded: boolean): ArrangementState {
  const scope = `${origin}\n${String(account)}`;
  const [state, setState] = useState(() => ({ scope, value: load(origin, account) }));
  let current = state;
  if (state.scope !== scope) {
    current = { scope, value: load(origin, account) };
    setState(current);
  }
  const dirty = useRef(false);

  const shown = useMemo(() => (loaded ? pruneArrangement(current.value, channels) : emptyArrangement), [loaded, current.value, channels]);

  // What pruning dropped is dropped from storage too, on every list change.
  useEffect(() => {
    if (!loaded || shown === current.value) return;
    dirty.current = true;
    setState((previous) => (previous.scope === scope ? { scope, value: shown } : previous));
  }, [loaded, shown, current.value, scope]);

  useEffect(() => {
    if (!dirty.current) return;
    dirty.current = false;
    if (account !== 0) api.writeArrangement(origin, account, current.value);
  }, [origin, account, current.value]);

  function edit(change: (a: Arrangement) => Arrangement) {
    setState((previous) => {
      const next = change(pruneArrangement(previous.value, channels));
      if (next === previous.value) return previous;
      dirty.current = true;
      return { scope: previous.scope, value: next };
    });
  }
  return { arrangement: shown, edit };
}
