// The right panel: who is here (People) and what is running (Agents), from
// the members list joined with the latest roster frame. Every name, task,
// runtime and model is another person's or agent's text and is rendered as a
// text node.
import { useEffect, useState } from 'react';
import type { RunV1 } from '../schema/run';
import type { SessionV1 } from '../schema/session';
import { ago } from './format';
import { joinMembers, type Entry } from './join';
import type { Agents } from './useAgents';

function SessionRow({ session, runs, now, onOpen }: {
  readonly session: SessionV1;
  readonly runs: readonly RunV1[];
  readonly now: number;
  readonly onOpen: (runId: number, sessionId: number, known: RunV1 | null) => void;
}) {
  const runtime = [session.runtime, session.model].filter((x): x is string => x !== undefined && x !== '').join('/');
  return (
    <li className="ag-session">
      <span>#{session.id}</span> <span className="ag-status">{session.status}</span> <span>client: {session.client}</span>
      {runtime === '' ? null : <span> · {runtime}</span>}
      {session.outcome_unknown ? <span> · outcome unknown</span> : null}
      {session.task === undefined ? null : <span> · task: {session.task}</span>}
      <span> · {ago(now, session.last_seen_at)}</span>
      {session.current_run === undefined ? null : (
        <>
          {' '}
          <button
            type="button"
            onClick={() => {
              onOpen(session.current_run ?? 0, session.id, runs.find((r) => r.id === session.current_run) ?? null);
            }}
          >
            Open run #{session.current_run}
          </button>
        </>
      )}
      {runs.length === 0 ? null : (
        <ul className="ag-runs" aria-label={`Recent runs of session ${String(session.id)}`}>
          {runs.map((run) => (
            <li key={run.id}>
              <button
                type="button"
                onClick={() => {
                  onOpen(run.id, session.id, run);
                }}
              >
                Run #{run.id} · {run.state}
              </button>{' '}
              <span>task: {run.task}</span> <span>· {ago(now, run.started_at)}</span>
            </li>
          ))}
        </ul>
      )}
    </li>
  );
}

function AgentRow({ entry, expanded, onToggle, sessionRuns, now, onOpen }: {
  readonly entry: Entry;
  readonly expanded: boolean;
  readonly onToggle: () => void;
  readonly sessionRuns: Agents['state']['sessionRuns'];
  readonly now: number;
  readonly onOpen: (runId: number, sessionId: number, known: RunV1 | null) => void;
}) {
  const lead = entry.sessions.find((s) => s.status === entry.status);
  return (
    <li className="ag-agent">
      <button type="button" aria-expanded={expanded} onClick={onToggle}>
        <span aria-hidden="true">{expanded ? '▾' : '▸'}</span> <span className="ag-badge">Agent</span> <span className="ag-name">{entry.name}</span>
      </button>{' '}
      <span className="ag-status">{entry.status}</span>
      {entry.sessions.length > 1 ? <span> ({entry.sessions.length} sessions)</span> : null}
      {lead?.outcome_unknown === true ? <span> · outcome unknown</span> : null}
      {lead?.task === undefined ? null : <span> · task: {lead.task}</span>}
      {expanded ? (
        entry.sessions.length === 0 ? (
          <p className="hint">No sessions</p>
        ) : (
          <ul className="ag-sessions" aria-label={`Sessions of ${entry.name}`}>
            {entry.sessions.map((s) => (
              <SessionRow key={s.id} session={s} runs={sessionRuns[s.id] ?? []} now={now} onOpen={onOpen} />
            ))}
          </ul>
        )
      ) : null}
    </li>
  );
}

const unavailable: Record<'ended' | 'unavailable', string> = {
  ended: 'Session ended; could not start a new one',
  unavailable: 'Could not start a session',
};

export function MembersPanel({ agents, now }: { readonly agents: Agents; readonly now: number }) {
  const { state, session, members } = agents;
  const [expanded, setExpanded] = useState<ReadonlySet<number>>(new Set());
  const joined = joinMembers(members, state.roster);
  const live = state.link === 'live';

  // The runs of every unfolded agent's sessions are read when the socket
  // comes back and when a session appears: nothing is replayed.
  const wanted = joined.agents
    .filter((a) => expanded.has(a.id))
    .flatMap((a) => a.sessions.map((s) => s.id))
    .join(',');
  const { loadRuns } = agents;
  useEffect(() => {
    if (!live || wanted === '') return;
    for (const id of wanted.split(',')) loadRuns(Number(id));
  }, [live, wanted, loadRuns]);

  // Another channel has other agents; none stays unfolded.
  const channelId = state.channelId;
  useEffect(() => {
    setExpanded(new Set());
  }, [channelId]);

  if (session.phase === 'failed') {
    return (
      <div>
        <p role="status">Members unavailable</p>
        <p className="hint">{unavailable[session.why]}</p>
      </div>
    );
  }
  if (!live) {
    return state.settled ? (
      <p role="status">Members unavailable</p>
    ) : (
      <p role="status">Connecting…</p>
    );
  }
  return (
    <div className="ag-panel">
      <h3>People</h3>
      {joined.people.length === 0 ? (
        <p className="empty">Nobody</p>
      ) : (
        <ul aria-label="People">
          {joined.people.map((p) => (
            <li key={p.id}>
              <span className="ag-name">{p.name}</span>
              {p.live > 1 ? <span> ({p.live})</span> : null} <span className="ag-status">{p.state}</span>
            </li>
          ))}
        </ul>
      )}
      <h3>Agents</h3>
      {joined.agents.length === 0 ? (
        <p className="empty">No agents in this channel</p>
      ) : (
        <ul aria-label="Agents">
          {joined.agents.map((a) => (
            <AgentRow
              key={a.id}
              entry={a}
              expanded={expanded.has(a.id)}
              onToggle={() => {
                setExpanded((held) => {
                  const next = new Set(held);
                  if (!next.delete(a.id)) next.add(a.id);
                  return next;
                });
              }}
              sessionRuns={state.sessionRuns}
              now={now}
              onOpen={agents.openRun}
            />
          ))}
        </ul>
      )}
      {state.notice === null ? null : <p role="status">{state.notice}</p>}
    </div>
  );
}
