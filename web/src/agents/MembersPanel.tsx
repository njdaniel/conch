// The right panel: who is here (People, online then offline) and what is
// running (Agents and Runs, folded to one line when there are none), from
// the members list joined with the latest roster frame. Every name, task,
// runtime and model is another person's or agent's text and is rendered as a
// text node.
import { useEffect, useState } from 'react';
import { MemberRow, Members, type MemberControlsProps } from '../Members';
import type { RunV1 } from '../schema/run';
import type { SessionV1 } from '../schema/session';
import { ago } from './format';
import { joinMembers, type Entry } from './join';
import { RunsList } from './RunsList';
import type { Agents } from './useAgents';

function SessionRow({ session, runs, now, onOpen }: {
  readonly session: SessionV1;
  readonly runs: readonly RunV1[];
  readonly now: number;
  readonly onOpen: (runId: number, sessionId: number, known: RunV1 | null) => void;
}) {
  const runtime = [session.runtime, session.model].filter((x): x is string => x !== undefined && x !== '').join('/');
  const current = session.current_run;
  return (
    <li className="ag-session">
      <span>#{session.id}</span> <span className="ag-status">{session.status}</span> <span>client: {session.client}</span>
      {runtime === '' ? null : <span> · {runtime}</span>}
      {session.outcome_unknown ? <span> · outcome unknown</span> : null}
      {session.task === undefined ? null : <span> · task: {session.task}</span>}
      <span> · {ago(now, session.last_seen_at)}</span>
      {current === undefined ? null : (
        <>
          {' '}
          <button
            type="button"
            onClick={() => {
              onOpen(current, session.id, runs.find((r) => r.id === current) ?? null);
            }}
          >
            Open run #{current}
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

function AgentRow({ entry, expanded, onToggle, sessionRuns, now, onOpen, controls }: {
  readonly entry: Entry;
  readonly expanded: boolean;
  readonly onToggle: () => void;
  readonly sessionRuns: Agents['state']['sessionRuns'];
  readonly now: number;
  readonly onOpen: (runId: number, sessionId: number, known: RunV1 | null) => void;
  readonly controls: MemberControlsProps;
}) {
  const lead = entry.sessions.find((s) => s.status === entry.status);
  return (
    <MemberRow key={`${String(controls.channel.id)}:${String(entry.id)}`} {...controls} principalId={entry.id} className={`ag-agent state-${entry.state}`}
      after={
        expanded ? (
          entry.sessions.length === 0 ? (
            <p className="hint">No sessions</p>
          ) : (
            <ul className="ag-sessions" aria-label={`Sessions of ${entry.name}`}>
              {entry.sessions.map((s) => (
                <SessionRow key={s.id} session={s} runs={sessionRuns[s.id] ?? []} now={now} onOpen={onOpen} />
              ))}
            </ul>
          )
        ) : null
      }
    >
      <button type="button" className="quiet" aria-expanded={expanded} onClick={onToggle}>
        <span aria-hidden="true">{expanded ? '▾' : '▸'}</span> <span className="ag-badge">Agent</span> <span className="ag-name">{entry.name}</span>
      </button>{' '}
      <span className="ag-status">{entry.status}</span>
      {entry.sessions.length > 1 ? <span> ({entry.sessions.length} sessions)</span> : null}
      {lead?.outcome_unknown === true ? <span> · outcome unknown</span> : null}
      {lead?.task === undefined ? null : <span> · task: {lead.task}</span>}
    </MemberRow>
  );
}

const unavailable: Record<'ended' | 'unavailable', string> = {
  ended: 'Session ended; could not start a new one',
  unavailable: 'Could not start a session',
};

type PanelControls = Omit<MemberControlsProps, 'members' | 'refreshMembers'>;

export function MembersPanel({ agents, now, controls }: { readonly agents: Agents; readonly now: number; readonly controls: PanelControls | null }) {
  const { state, session, members } = agents;
  const [expanded, setExpanded] = useState<ReadonlySet<number>>(new Set());
  const joined = joinMembers(members, state.roster);
  const live = state.link === 'live';
  const memberControls: MemberControlsProps | null = controls === null ? null : { ...controls, members, refreshMembers: agents.refreshMembers };

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

  // Most channels have neither agents nor runs: one line instead of two empty
  // sections, once the runs are known to be none. While they load, or after a
  // failed read (which offers Try again), the sections show.
  const runs = state.runs;
  const folded = joined.agents.length === 0 && runs.loaded && !runs.loading && runs.note === null && runs.list.length === 0;

  if (session.phase === 'failed') {
    return (
      <div>
        <p role="status">Members unavailable</p>
        <p className="hint">{unavailable[session.why]}</p>
      </div>
    );
  }
  if (!live) {
    // Nothing is open, so nothing is being connected to (issue #1032). A
    // session that failed to start is said above, whatever is open.
    if (controls === null) return <p role="status">No channel open</p>;
    return state.settled ? (
      <p role="status">Members unavailable</p>
    ) : (
      <p role="status">Connecting…</p>
    );
  }
  if (memberControls === null) return <p role="status">Members unavailable</p>;
  return (
    <div className="ag-panel">
      <Members {...memberControls} people={joined.people} />
      {folded ? (
        <p className="empty">No agents or runs in this channel.</p>
      ) : (
        <>
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
                  controls={memberControls}
                />
              ))}
            </ul>
          )}
          <RunsList agents={agents} />
        </>
      )}
      {state.notice === null ? null : <p role="status">{state.notice}</p>}
    </div>
  );
}
