// Everything the panel and run detail run on: the browser's own session (UX-18),
// the events socket of the channel on screen, the members list, and the reads
// and the one write that run detail makes. The socket is the chat's own
// reconnect machine (state/feed.ts), given the events address: a fresh ticket
// at every attempt, the same backoff.
import { useCallback, useEffect, useReducer, useRef, useState } from 'react';
import * as api from '../api';
import type { ChannelDocV1, ChannelMemberV1 } from '../schema/channel';
import type { CreateInterventionRequestV1 } from '../schema/intervention';
import type { RunV1 } from '../schema/run';
import { Feed, firstDelay, maxDelay, type FeedStatus } from '../state/feed';
import { parseEventFrame } from './frames';
import { agentsReducer, initialAgents, maxRunPages, recentRuns, runsPageSize, type AgentsState, type RunFilter } from './state';

// The name the browser gives its own session: what a roster reader sees as the
// program behind it.
export const sessionClient = 'browser';

const realClock = {
  setTimeout: (run: () => void, ms: number) => window.setTimeout(run, ms),
  clearTimeout: (handle: unknown) => {
    window.clearTimeout(handle as number);
  },
};

// The reason conchd gives when it closes an events socket because the session
// it was keeping online has ended.
const sessionEnded = 'session ended';

// idle: not signed in. starting: being created. ready: id is the session.
// failed: none could be created (or a replacement could not), and why.
export type OwnSession =
  | { readonly phase: 'idle' }
  | { readonly phase: 'starting' }
  | { readonly phase: 'ready'; readonly id: number }
  | { readonly phase: 'failed'; readonly why: 'ended' | 'unavailable' };

export interface OwnSessionControl {
  readonly session: OwnSession;
  // release ends the session now, while the credential is still held. Sign-out
  // calls it before the credential goes.
  readonly release: () => void;
  // replace is for a session the server ended: it is forgotten, not ended
  // again, and one new one is made, once until the socket has been healthy.
  readonly replace: () => void;
  // healthy says the events socket delivered a roster, so a later end of the
  // session may be answered with a new one again.
  readonly healthy: () => void;
}

// useOwnSession creates the browser's session after sign-in and ends it on
// sign-out (release) and on pagehide, with keepalive. If that request is lost
// the server marks the session offline by itself.
export function useOwnSession(active: boolean, onUnauthorized: () => void): OwnSessionControl {
  const [session, setSession] = useState<OwnSession>({ phase: 'idle' });
  const held = useRef<number | null>(null);
  // Counts the sessions asked for; an answer for an older one is ended at once.
  const generation = useRef(0);
  const replaced = useRef(false);
  // The wait before the next replacement, doubling from 1 s to 30 s, so a
  // server that keeps ending sessions after a roster cannot drive the page at
  // network speed. A session that lived 30 s starts it over.
  const gap = useRef(0);
  const born = useRef(0);
  const timer = useRef<number | null>(null);

  const release = useCallback(() => {
    generation.current++;
    if (timer.current !== null) window.clearTimeout(timer.current);
    timer.current = null;
    const id = held.current;
    held.current = null;
    if (id !== null) api.endSession(id);
  }, []);

  const start = useCallback(
    (why: 'ended' | 'unavailable') => {
      const mine = ++generation.current;
      setSession({ phase: 'starting' });
      void api.createSession(sessionClient).then((result) => {
        if (mine !== generation.current) {
          // Signed out, or replaced, while it was being made.
          if (result.ok) api.endSession(result.value.id);
          return;
        }
        if (result.ok) {
          held.current = result.value.id;
          born.current = Date.now();
          setSession({ phase: 'ready', id: result.value.id });
        } else if (result.failure === 'unauthorized') {
          onUnauthorized();
        } else {
          setSession({ phase: 'failed', why });
        }
      });
    },
    [onUnauthorized],
  );

  useEffect(() => {
    if (!active) {
      setSession({ phase: 'idle' });
      return;
    }
    replaced.current = false;
    gap.current = 0;
    start('unavailable');
    const onHide = () => {
      release();
    };
    window.addEventListener('pagehide', onHide);
    return () => {
      window.removeEventListener('pagehide', onHide);
      release();
    };
  }, [active, start, release]);

  const replace = useCallback(() => {
    if (replaced.current) {
      // One new session per healthy stretch: a server that keeps ending them
      // cannot make the page create sessions in a loop.
      generation.current++;
      held.current = null;
      setSession({ phase: 'failed', why: 'ended' });
      return;
    }
    replaced.current = true;
    held.current = null;
    if (Date.now() - born.current >= maxDelay) gap.current = 0;
    const wait = gap.current;
    gap.current = Math.min(Math.max(gap.current * 2, firstDelay), maxDelay);
    if (wait === 0) {
      start('ended');
      return;
    }
    const mine = ++generation.current;
    setSession({ phase: 'starting' });
    timer.current = window.setTimeout(() => {
      timer.current = null;
      if (mine === generation.current) start('ended');
    }, wait);
  }, [start]);

  const healthy = useCallback(() => {
    replaced.current = false;
  }, []);

  return { session, release, replace, healthy };
}

export interface Agents {
  readonly state: AgentsState;
  readonly session: OwnSession;
  readonly members: readonly ChannelMemberV1[];
  readonly release: () => void;
  readonly loadRuns: (session: number) => void;
  readonly openRun: (runId: number, sessionId: number, known: RunV1 | null) => void;
  // closeRun leaves run detail; a notice, if given, is the line the panel
  // keeps saying why.
  readonly closeRun: (notice?: string) => void;
  // moreRuns reads the next older page of the Runs view, or the first when it
  // failed.
  readonly moreRuns: () => void;
  readonly setRunFilter: (filter: RunFilter) => void;
  readonly ask: (request: CreateInterventionRequestV1) => void;
}

// What a refusal says, in the reader's terms; the server's code picks the line.
function refusalLine(code: string | undefined): string {
  switch (code) {
    case 'intervention_unsupported':
      return 'This session does not accept that request';
    case 'too_many_interventions':
      return 'Too many requests are waiting on this run; wait for the agent to answer';
    case 'invalid_transition':
      return 'The run has ended';
    case 'human_required':
      return 'Only a person can ask a run to do something';
    case 'run_not_found':
      return 'This run is gone, or you can no longer see it';
    default:
      return 'The request was not accepted';
  }
}

export const runGone = 'This run is gone, or you can no longer see it';

export function useAgents({
  signedIn,
  channel,
  channelId,
  nameOf,
  onUnauthorized,
  onChannel,
  onMissing,
}: {
  readonly signedIn: boolean;
  readonly channel: string | null;
  readonly channelId: number | null;
  // The channel's name now, by id: what a reconnect and a read name, so a
  // channel renamed while open (issue #728) keeps its socket.
  readonly nameOf: (channelId: number) => string | null;
  readonly onUnauthorized: () => void;
  // The channel as it now is, from the events socket (conch.channel.v1): it
  // was renamed.
  readonly onChannel: (channel: ChannelDocV1) => void;
  // A read for the channel answered channel_not_found: it may have been
  // renamed under this page.
  readonly onMissing: (channelId: number) => void;
}): Agents {
  const [state, dispatch] = useReducer(agentsReducer, initialAgents);
  const [members, setMembers] = useState<readonly ChannelMemberV1[]>([]);
  const own = useOwnSession(signedIn, onUnauthorized);
  const sessionId = own.session.phase === 'ready' ? own.session.id : null;
  const { replace, healthy, release } = own;
  // The effects below are keyed on the channel's id, and read its name
  // (nameOf) when they ask.
  const open = channel !== null;

  // The newest state, for code that outlives a render (the stamp a read is
  // asked at, the activity id a re-read starts after).
  const now = useRef(state);
  now.current = state;

  // Counts the resets of the view; an answer asked before the latest one is
  // not for the view on screen, even when it is for a channel of the same name
  // (general, then ops, then general again).
  const epoch = useRef(0);

  // The members list: where each principal comes from. It is read again when
  // the socket comes back and when the roster names someone it does not hold.
  // In between, the socket sends the whole list each time it changes (someone
  // removed, hidden or renamed, #515), so the panel never has to ask.
  const [membersAttempt, setMembersAttempt] = useState(0);
  // Counts the lists the socket has sent; a read asked before the latest one
  // may be older than it, and is dropped.
  const membersSent = useRef(0);
  useEffect(() => {
    setMembers([]);
  }, [channelId]);
  useEffect(() => {
    if (!open || channelId === null) return;
    const id = channelId;
    const name = nameOf(id);
    if (name === null) return;
    let live = true;
    const asked = membersSent.current;
    void api.listMembers(name).then((result) => {
      if (!live) return;
      if (result.ok) {
        if (asked === membersSent.current) setMembers(result.value);
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else if (result.code === api.channelNotFound) onMissing(id);
    });
    return () => {
      live = false;
    };
  }, [open, channelId, nameOf, membersAttempt, onUnauthorized, onMissing]);

  const heldIds = members.map((m) => m.principal_id).join(',');
  const unknownKey = (() => {
    const known = new Set(heldIds === '' ? [] : heldIds.split(',').map(Number));
    return state.roster
      .filter((s) => !known.has(s.principal_id))
      .map((s) => s.principal_id)
      .sort((a, b) => a - b)
      .join(',');
  })();
  const askedFor = useRef('');
  useEffect(() => {
    if (unknownKey === '' || askedFor.current === unknownKey) return;
    askedFor.current = unknownKey;
    setMembersAttempt((n) => n + 1);
  }, [unknownKey]);

  // The events socket of the channel on screen, reopened when the channel or
  // the session changes. Nothing is replayed, so every reconnect takes the next
  // roster as the state.
  useEffect(() => {
    epoch.current++;
    dispatch({ type: 'reset', channelId });
    askedFor.current = '';
  }, [channelId]);

  useEffect(() => {
    if (sessionId === null) {
      // The session is being replaced or ended: the roster of its socket is
      // gone until the next one's first.
      if (now.current.link === 'live') dispatch({ type: 'down' });
      return;
    }
    if (!open || channelId === null) return;
    const id = channelId;
    const name = nameOf(id);
    if (name === null) return;
    let live = true;
    let reason = '';
    let denials = firstDelay;
    let retry: unknown = null;

    function onStatus(status: FeedStatus) {
      if (!live) return;
      switch (status) {
        case 'waiting':
          dispatch({ type: 'down' });
          break;
        case 'unauthorized':
          dispatch({ type: 'down' });
          onUnauthorized();
          break;
        case 'denied':
          dispatch({ type: 'down' });
          if (reason === sessionEnded) {
            replace();
          } else {
            // Membership lost, or too slow a reader: a new socket gets a fresh
            // roster, after a wait that grows while it keeps being refused.
            const wait = denials;
            denials = Math.min(denials * 2, maxDelay);
            retry = realClock.setTimeout(() => {
              retry = null;
              if (live) feed.start();
            }, wait);
          }
          break;
        default:
          break;
      }
    }

    const feed = new Feed({
      channel: name,
      clock: realClock,
      after: () => 0,
      // Each attempt names the channel as it is called then.
      open: (_channel, _after, events) =>
        api.openEventsSocket(nameOf(id) ?? name, sessionId, {
          ...events,
          onClose: (code, why) => {
            reason = why ?? '';
            events.onClose(code, why);
          },
        }),
      onFrame: (data) => {
        const frame = parseEventFrame(data);
        if (frame === null) return;
        switch (frame.kind) {
          case 'roster':
            // A roster of another channel is not this one's.
            if (frame.roster.channel_id !== id) return;
            denials = firstDelay;
            healthy();
            dispatch({ type: 'roster', channelId: id, sessions: frame.roster.sessions });
            break;
          case 'members':
            if (frame.members.channel_id !== id) return;
            membersSent.current++;
            setMembers(frame.members.members);
            break;
          case 'run':
            dispatch({ type: 'run', run: frame.run });
            break;
          case 'activity':
            dispatch({ type: 'activity', entry: frame.entry });
            break;
          case 'intervention':
            dispatch({ type: 'intervention', intervention: frame.intervention });
            break;
          case 'channel':
            // This channel renamed (issue #728); the socket stays open.
            if (frame.channel.channel_id !== id) return;
            onChannel(frame.channel);
            break;
        }
      },
      onStatus,
    });
    feed.start();
    return () => {
      live = false;
      if (retry !== null) realClock.clearTimeout(retry);
      feed.stop();
    };
  }, [open, channelId, nameOf, sessionId, onUnauthorized, onChannel, replace, healthy]);

  // The members line is read again each time the socket comes back.
  useEffect(() => {
    if (state.liveGen > 0) setMembersAttempt((n) => n + 1);
  }, [state.liveGen]);

  // A session's newest runs: one request per session.
  const loadRuns = useCallback(
    (session: number) => {
      if (channel === null) return;
      const asked = channel;
      const id = channelId;
      void api.listSessionRuns(asked, session, recentRuns).then((result) => {
        if (!result.ok) {
          if (result.failure === 'unauthorized') onUnauthorized();
          else if (result.code === api.channelNotFound && id !== null) onMissing(id);
          return;
        }
        dispatch({ type: 'sessionRuns', session, runs: result.value });
      });
    },
    [channel, channelId, onUnauthorized, onMissing],
  );

  // The Runs view: the first page when the socket is first live and again
  // each time it comes back (frames were missed), older pages on request.
  // An answer for a channel no longer on screen is dropped.
  const askRuns = useCallback(
    (before: number) => {
      if (channel === null) return;
      const asked = channel;
      const mine = epoch.current;
      const refresh = before === 0 && now.current.runs.loaded;
      if (!refresh) {
        if (now.current.runs.loading || now.current.runs.pages >= maxRunPages) return;
        dispatch({ type: 'runsAsk' });
      }
      const id = channelId;
      void api.listChannelRuns(asked, runsPageSize, before).then((result) => {
        if (epoch.current !== mine) return;
        if (result.ok) {
          dispatch({ type: 'runsPage', before, runs: result.value });
        } else if (result.failure === 'unauthorized') {
          onUnauthorized();
        } else {
          if (result.code === api.channelNotFound && id !== null) onMissing(id);
          dispatch({ type: 'runsFailed', before, line: 'Could not list runs' });
        }
      });
    },
    [channel, channelId, onUnauthorized, onMissing],
  );
  // Keyed on liveGen alone: a channel switch reads again with the roster of
  // the new socket, not once more before it.
  const askRunsNow = useRef(askRuns);
  askRunsNow.current = askRuns;
  useEffect(() => {
    if (state.liveGen > 0) askRunsNow.current(0);
  }, [state.liveGen]);
  const moreRuns = useCallback(() => {
    const view = now.current.runs;
    // Before the first page is in, this is the retry of it.
    if (!view.loaded) askRuns(0);
    else if (view.more) askRuns(view.before);
  }, [askRuns]);
  const setRunFilter = useCallback((filter: RunFilter) => {
    dispatch({ type: 'runsFilter', filter });
  }, []);

  // Run detail reads. The run alone is how the pane learns it can no longer
  // see the run; activity and interventions are read in full on open and from
  // the last entry held on a reconnect, since nothing is replayed.
  const runId = state.detail?.runId ?? null;
  const readRun = useCallback(
    (id: number) => {
      const stamp = now.current.detail?.runStamp ?? 0;
      void api.getRun(id).then((result) => {
        if (result.ok) {
          dispatch({ type: 'runRead', runId: id, stamp, run: result.value });
        } else if (result.failure === 'unauthorized') {
          onUnauthorized();
        } else if (result.code === 'run_not_found') {
          dispatch({ type: 'close', notice: runGone });
        } else if (result.failure === 'network') {
          dispatch({ type: 'problem', runId: id, line: 'Cannot reach the server' });
        }
      });
    },
    [onUnauthorized],
  );

  useEffect(() => {
    if (runId === null) return;
    let live = true;
    const held = now.current.detail?.activity ?? [];
    const after = held[held.length - 1]?.id ?? 0;
    readRun(runId);
    void api.listActivity(runId, after).then((result) => {
      if (!live) return;
      if (result.ok) dispatch({ type: 'activityRead', runId, entries: result.value });
      else if (result.failure === 'unauthorized') onUnauthorized();
    });
    void api.listInterventions(runId).then((result) => {
      if (!live) return;
      if (result.ok) dispatch({ type: 'interventionsRead', runId, list: result.value });
      else if (result.failure === 'unauthorized') onUnauthorized();
    });
    return () => {
      live = false;
    };
  }, [runId, state.liveGen, readRun, onUnauthorized]);

  // While the socket is down the run is read once more: that read is how the
  // pane learns the run can no longer be seen (membership removed).
  const down = state.link === 'down';
  useEffect(() => {
    if (runId !== null && down) readRun(runId);
  }, [runId, down, readRun]);

  const openRun = useCallback((id: number, session: number, known: RunV1 | null) => {
    dispatch({ type: 'open', runId: id, sessionId: session, run: known });
  }, []);

  const closeRun = useCallback((notice?: string) => {
    dispatch({ type: 'close', notice: notice ?? null });
  }, []);

  // One POST per click. A send is never repeated here, and nothing shows as
  // done until the intervention's own frame says so.
  const ask = useCallback(
    (request: CreateInterventionRequestV1) => {
      const detail = now.current.detail;
      if (detail === null || detail.sending) return;
      const id = detail.runId;
      dispatch({ type: 'sending', runId: id });
      void api.createIntervention(id, request).then((result) => {
        if (result.ok) {
          dispatch({ type: 'sent', runId: id, intervention: result.value });
        } else if (result.failure === 'unauthorized') {
          onUnauthorized();
        } else if (result.failure === 'network') {
          dispatch({ type: 'sendFailed', runId: id, line: 'Cannot reach the server; the request may not have been sent' });
        } else {
          dispatch({ type: 'sendFailed', runId: id, line: refusalLine(result.code) });
          // A run that ended: what state it is in is for a read to say.
          if (result.code === 'invalid_transition') readRun(id);
        }
      });
    },
    [onUnauthorized, readRun],
  );

  return { state, session: own.session, members, release, loadRuns, openRun, closeRun, moreRuns, setRunFilter, ask };
}
