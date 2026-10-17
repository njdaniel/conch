// The Runs view, in the right panel: every run of the channel the person may
// see, newest first, from the REST list (a page at a time) and the run frames.
// Selecting one opens run detail in the centre. Every task, outcome and name is
// another person's or an agent's text and is rendered as a text node.
import type { ChannelMemberV1 } from '../schema/channel';
import type { RunV1 } from '../schema/run';
import { runEnded } from '../schema/run';
import { clockSeconds, runSpan, spanText } from './format';
import { visibleRuns, type RunFilter } from './state';
import type { Agents } from './useAgents';
import { useNow } from './useNow';

const filters: readonly RunFilter[] = ['all', 'open', 'ended'];

function authorName(members: readonly ChannelMemberV1[], id: number): string {
  return members.find((m) => m.principal_id === id)?.name ?? `#${String(id)}`;
}

function RunRow({ run, name, now, onOpen }: {
  readonly run: RunV1;
  readonly name: string;
  readonly now: number;
  readonly onOpen: () => void;
}) {
  return (
    <li>
      <button type="button" onClick={onOpen}>
        #{run.id} · {run.state}
      </button>{' '}
      <span className="ag-name">{name}</span> <span>task: {run.task}</span> <span>· started {clockSeconds(run.started_at)}</span>
      {run.ended_at === undefined ? (
        <span> · for {spanText(runSpan(run, now), false)}</span>
      ) : (
        <span> · ended {clockSeconds(run.ended_at)}</span>
      )}
      {run.outcome === undefined || run.outcome === '' ? null : <span className="ag-prose"> · {run.outcome}</span>}
    </li>
  );
}

export function RunsList({ agents }: { readonly agents: Agents }) {
  const { state, members } = agents;
  const view = state.runs;
  const shown = visibleRuns(view);
  // The duration of an open run moves every second; nothing ticks when none is
  // listed.
  const anyOpen = shown.some((r) => !runEnded(r.state));
  const now = useNow(anyOpen, 1000);

  return (
    <section aria-label="Runs">
      <h3>Runs</h3>
      <label>
        Filter runs{' '}
        <select
          value={view.filter}
          onChange={(event) => {
            const next = filters.find((f) => f === event.target.value);
            if (next !== undefined) agents.setRunFilter(next);
          }}
        >
          <option value="all">all</option>
          <option value="open">open</option>
          <option value="ended">ended</option>
        </select>
      </label>
      {shown.length === 0 ? (
        <p className="empty" role="status">
          {view.loading || (!view.loaded && view.note === null)
            ? 'Loading runs…'
            : view.note !== null && !view.loaded
              ? view.note
              : view.filter !== 'all' && view.list.length > 0
                ? `No ${view.filter} runs here`
                : 'No runs in this channel'}
        </p>
      ) : (
        <ul aria-label="Channel runs">
          {shown.map((run) => (
            <RunRow
              key={run.id}
              run={run}
              name={authorName(members, run.principal_id)}
              now={now}
              onOpen={() => {
                agents.openRun(run.id, run.session_id, run);
              }}
            />
          ))}
        </ul>
      )}
      {view.note !== null && view.loaded ? <p role="alert">{view.note}</p> : null}
      {view.more || (!view.loaded && view.note !== null) ? (
        <button type="button" disabled={view.loading} onClick={agents.moreRuns}>
          {view.loaded ? 'Show older runs' : 'Try again'}
        </button>
      ) : null}
    </section>
  );
}
