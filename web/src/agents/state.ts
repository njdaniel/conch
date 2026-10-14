// What the panel and run detail know, as pure transitions (no DOM, no socket).
// Everything about a run comes from reads and from events frames, never from
// what the person asked: a request that was sent is "requested" until its
// frame says otherwise, and a run's state is only what a RunV1 says (a frame,
// or a read of the same document).
import type { AgentActivityV1 } from '../schema/activity';
import type { InterventionState, InterventionV1 } from '../schema/intervention';
import { runEnded, type RunV1 } from '../schema/run';
import type { SessionV1 } from '../schema/session';

// connecting: nothing heard yet from this socket. live: `roster` is the latest
// the server sent. down: the socket dropped or was refused; the roster is not
// trusted and is emptied until a new one arrives.
export type Link = 'connecting' | 'live' | 'down';

// How many of a session's runs the panel lists, and how much activity run
// detail holds (the oldest go first).
export const recentRuns = 10;
export const keptActivity = 1000;

export interface RunDetail {
  readonly runId: number;
  readonly sessionId: number;
  readonly run: RunV1 | null;
  // Counts the run frames applied. A read of the run lands only if no frame
  // did since it was asked for: the frame is the newer word.
  readonly runStamp: number;
  readonly activity: readonly AgentActivityV1[];
  readonly interventions: readonly InterventionV1[];
  readonly sending: boolean;
  // One line about the last read or send that failed, or null.
  readonly problem: string | null;
}

export interface AgentsState {
  readonly channelId: number | null;
  readonly link: Link;
  // Set once the socket has said anything, so the panel does not announce
  // "unavailable" before it has tried.
  readonly settled: boolean;
  // Counts the times the socket became live; run detail reads again on each.
  readonly liveGen: number;
  readonly roster: readonly SessionV1[];
  // Each session's newest runs, newest first.
  readonly sessionRuns: Readonly<Record<number, readonly RunV1[]>>;
  readonly detail: RunDetail | null;
  // Why run detail closed, or another line for the panel.
  readonly notice: string | null;
}

export const initialAgents: AgentsState = {
  channelId: null,
  link: 'connecting',
  settled: false,
  liveGen: 0,
  roster: [],
  sessionRuns: {},
  detail: null,
  notice: null,
};

export type AgentsAction =
  | { readonly type: 'reset'; readonly channelId: number | null }
  | { readonly type: 'down' }
  | { readonly type: 'roster'; readonly channelId: number; readonly sessions: readonly SessionV1[] }
  | { readonly type: 'run'; readonly run: RunV1 }
  | { readonly type: 'activity'; readonly entry: AgentActivityV1 }
  | { readonly type: 'intervention'; readonly intervention: InterventionV1 }
  | { readonly type: 'sessionRuns'; readonly session: number; readonly runs: readonly RunV1[] }
  | { readonly type: 'open'; readonly runId: number; readonly sessionId: number; readonly run: RunV1 | null }
  | { readonly type: 'close'; readonly notice: string | null }
  | { readonly type: 'runRead'; readonly runId: number; readonly stamp: number; readonly run: RunV1 }
  | { readonly type: 'activityRead'; readonly runId: number; readonly entries: readonly AgentActivityV1[] }
  | { readonly type: 'interventionsRead'; readonly runId: number; readonly list: readonly InterventionV1[] }
  | { readonly type: 'problem'; readonly runId: number; readonly line: string }
  | { readonly type: 'sending'; readonly runId: number }
  | { readonly type: 'sent'; readonly runId: number; readonly intervention: InterventionV1 }
  | { readonly type: 'sendFailed'; readonly runId: number; readonly line: string };

// An intervention only moves forward, so an older document of one never takes
// it back.
function rank(state: InterventionState): number {
  return state === 'requested' ? 1 : state === 'acknowledged' ? 2 : 3;
}

// upsertIntervention puts iv in the history by id. The server sends documents
// whole and may repeat one, so applying the same one again changes nothing.
export function upsertIntervention(list: readonly InterventionV1[], iv: InterventionV1): InterventionV1[] {
  const out = [...list];
  const at = out.findIndex((x) => x.id === iv.id);
  if (at < 0) return [...out, iv].sort((a, b) => a.id - b.id);
  const held = out[at];
  if (held !== undefined && rank(iv.state) >= rank(held.state)) out[at] = iv;
  return out;
}

// mergeActivity merges incoming into the entries held, by id, oldest first,
// keeping the newest keptActivity.
export function mergeActivity(held: readonly AgentActivityV1[], incoming: readonly AgentActivityV1[]): AgentActivityV1[] {
  const byId = new Map<number, AgentActivityV1>();
  for (const e of held) byId.set(e.id, e);
  for (const e of incoming) byId.set(e.id, e);
  const merged = [...byId.values()].sort((a, b) => a.id - b.id);
  return merged.length > keptActivity ? merged.slice(merged.length - keptActivity) : merged;
}

// cacheRun keeps the list of a session's newest runs current. A session whose
// list was never read has none to keep. A run does not leave a terminal state,
// so a document that would take it back is an older one.
function cacheRun(lists: AgentsState['sessionRuns'], run: RunV1): AgentsState['sessionRuns'] {
  const list = lists[run.session_id];
  if (list === undefined) return lists;
  const at = list.findIndex((r) => r.id === run.id);
  let next: RunV1[];
  if (at >= 0) {
    next = [...list];
    const held = list[at];
    if (held !== undefined && (!runEnded(held.state) || runEnded(run.state))) next[at] = run;
  } else {
    next = [...list, run].sort((a, b) => b.id - a.id).slice(0, recentRuns);
  }
  return { ...lists, [run.session_id]: next };
}

function applyRun(detail: RunDetail, run: RunV1): RunDetail {
  if (run.id !== detail.runId) return detail;
  const held = detail.run;
  if (held !== null && runEnded(held.state) && !runEnded(run.state)) return detail;
  return { ...detail, run, sessionId: run.session_id };
}

export function agentsReducer(state: AgentsState, action: AgentsAction): AgentsState {
  const detail = state.detail;
  switch (action.type) {
    case 'reset':
      return { ...initialAgents, channelId: action.channelId, liveGen: state.liveGen };
    case 'down':
      return { ...state, link: 'down', settled: true, roster: [] };
    case 'roster':
      if (action.channelId !== state.channelId) return state;
      return {
        ...state,
        link: 'live',
        settled: true,
        liveGen: state.link === 'live' ? state.liveGen : state.liveGen + 1,
        roster: action.sessions,
      };
    case 'run': {
      if (action.run.channel_id !== state.channelId) return state;
      const sessionRuns = cacheRun(state.sessionRuns, action.run);
      if (detail === null || detail.runId !== action.run.id) return { ...state, sessionRuns };
      return { ...state, sessionRuns, detail: applyRun({ ...detail, runStamp: detail.runStamp + 1 }, action.run) };
    }
    case 'activity':
      if (detail === null || detail.runId !== action.entry.run_id) return state;
      return { ...state, detail: { ...detail, activity: mergeActivity(detail.activity, [action.entry]) } };
    case 'intervention':
      if (detail === null || detail.runId !== action.intervention.run_id) return state;
      return { ...state, detail: { ...detail, interventions: upsertIntervention(detail.interventions, action.intervention) } };
    case 'sessionRuns':
      return { ...state, sessionRuns: { ...state.sessionRuns, [action.session]: action.runs.slice(0, recentRuns) } };
    case 'open':
      return {
        ...state,
        notice: null,
        detail: {
          runId: action.runId,
          sessionId: action.sessionId,
          run: action.run,
          runStamp: 0,
          activity: [],
          interventions: [],
          sending: false,
          problem: null,
        },
      };
    case 'close':
      return { ...state, detail: null, notice: action.notice };
    case 'runRead':
      if (detail === null || detail.runId !== action.runId || detail.runStamp !== action.stamp) return state;
      return { ...state, detail: applyRun(detail, action.run) };
    case 'activityRead':
      if (detail === null || detail.runId !== action.runId) return state;
      return {
        ...state,
        detail: { ...detail, activity: mergeActivity(detail.activity, action.entries.filter((e) => e.run_id === action.runId)) },
      };
    case 'interventionsRead': {
      if (detail === null || detail.runId !== action.runId) return state;
      let list = detail.interventions;
      for (const iv of action.list) if (iv.run_id === action.runId) list = upsertIntervention(list, iv);
      return { ...state, detail: { ...detail, interventions: list } };
    }
    case 'problem':
      if (detail === null || detail.runId !== action.runId) return state;
      return { ...state, detail: { ...detail, problem: action.line } };
    case 'sending':
      if (detail === null || detail.runId !== action.runId) return state;
      return { ...state, detail: { ...detail, sending: true, problem: null } };
    case 'sent':
      if (detail === null || detail.runId !== action.runId) return state;
      return {
        ...state,
        detail: { ...detail, sending: false, problem: null, interventions: upsertIntervention(detail.interventions, action.intervention) },
      };
    case 'sendFailed':
      if (detail === null || detail.runId !== action.runId) return state;
      return { ...state, detail: { ...detail, sending: false, problem: action.line } };
  }
}
