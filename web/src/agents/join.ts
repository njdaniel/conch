// Who is in the panel: the channel's members list joined with the latest
// roster. A member is where a principal comes from, so one with no session is
// still listed, as offline; a principal only the roster knows is added after
// the members (a member list read a moment earlier does not have it yet).
// Each principal appears once.
import type { ChannelMemberV1 } from '../schema/channel';
import type { PrincipalKind } from '../schema/principal';
import type { SessionState, SessionStatus, SessionV1 } from '../schema/session';

export interface Entry {
  readonly id: number;
  readonly name: string;
  readonly kind: PrincipalKind;
  readonly sessions: readonly SessionV1[];
  // How many sessions are online right now: the "(2 sessions)" after a name.
  readonly live: number;
  // The best liveness among the sessions; offline with none.
  readonly state: SessionState;
  // What an agent's row shows: the status of its lead session; offline with none.
  readonly status: SessionStatus;
}

export interface Joined {
  readonly people: readonly Entry[];
  readonly agents: readonly Entry[];
}

function stateRank(state: SessionState): number {
  return state === 'online' ? 2 : state === 'stale' ? 1 : 0;
}

// What needs a person first ranks highest. A status this client does not know
// ranks as online.
export function statusRank(status: SessionStatus): number {
  switch (status) {
    case 'blocked':
      return 7;
    case 'waiting':
      return 6;
    case 'running':
      return 5;
    case 'failed':
      return 4;
    case 'idle':
      return 3;
    case 'stale':
      return 1;
    case 'offline':
      return 0;
    default:
      return 2;
  }
}

// lead is the session a row stands for: the first of the highest status.
function lead(sessions: readonly SessionV1[]): SessionV1 | null {
  let best: SessionV1 | null = null;
  for (const s of sessions) {
    if (best === null || statusRank(s.status) > statusRank(best.status)) best = s;
  }
  return best;
}

export function entryName(name: string | undefined, id: number): string {
  return name === undefined || name.trim() === '' ? `#${String(id)}` : name;
}

function build(id: number, name: string, kind: PrincipalKind, sessions: readonly SessionV1[]): Entry {
  let state: SessionState = 'offline';
  let live = 0;
  for (const s of sessions) {
    if (s.state === 'online') live++;
    if (stateRank(s.state) > stateRank(state)) state = s.state;
  }
  return { id, name, kind, sessions, live, state, status: lead(sessions)?.status ?? 'offline' };
}

// joinMembers joins the members list with a roster's sessions. Members come in
// principal id order and then the roster's own order, so a refresh of the
// roster does not move rows under the reader. A member without a kind (an
// older server) is a person unless the roster says otherwise.
export function joinMembers(members: readonly ChannelMemberV1[], sessions: readonly SessionV1[]): Joined {
  const byPrincipal = new Map<number, { name: string; kind: PrincipalKind; sessions: SessionV1[] }>();
  const order: number[] = [];
  for (const m of [...members].sort((a, b) => a.principal_id - b.principal_id)) {
    if (byPrincipal.has(m.principal_id)) continue;
    byPrincipal.set(m.principal_id, { name: entryName(m.name, m.principal_id), kind: m.kind ?? 'human', sessions: [] });
    order.push(m.principal_id);
  }
  for (const s of sessions) {
    let row = byPrincipal.get(s.principal_id);
    if (row === undefined) {
      row = { name: entryName(s.name, s.principal_id), kind: s.kind, sessions: [] };
      byPrincipal.set(s.principal_id, row);
      order.push(s.principal_id);
    }
    // The roster is the newer word on what a principal is.
    row.kind = s.kind;
    row.sessions.push(s);
  }
  const people: Entry[] = [];
  const agents: Entry[] = [];
  for (const id of order) {
    const row = byPrincipal.get(id);
    if (row === undefined) continue;
    const entry = build(id, row.name, row.kind, row.sessions);
    (entry.kind === 'agent' ? agents : people).push(entry);
  }
  return { people, agents };
}
