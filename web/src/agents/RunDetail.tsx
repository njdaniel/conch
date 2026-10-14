// Run detail, in the centre area: the run's fields, its activity (a REST read,
// then live frames) and the history of interventions asked of it, and the two
// asks a person can make. Nothing here claims an outcome: an ask shows as
// Requested until its frame says more, and the run's state is only what a
// RunV1 says. Every field is another person's or an agent's text.
import { useState } from 'react';
import type { InterventionKind, InterventionV1 } from '../schema/intervention';
import { runEnded } from '../schema/run';
import { clockSeconds, ago } from './format';
import { safeLink } from './links';
import type { RunDetail as Detail } from './state';
import type { Agents } from './useAgents';

// The ask's state as the history words it.
export function interventionWord(iv: InterventionV1): string {
  switch (iv.state) {
    case 'requested':
      return 'Requested';
    case 'acknowledged':
      return 'Acknowledged';
    case 'applied':
      return 'Applied';
    case 'failed':
      return `Failed: ${iv.reason ?? ''}`;
  }
}

const kindWord: Record<InterventionKind, string> = { message: 'Message', cancel: 'Cancel', pause: 'Pause', resume: 'Resume' };

type Asking = 'none' | 'message' | 'cancel';

// A message is one line with no control character but tab; the server refuses
// them too.
function hasControl(text: string): boolean {
  for (let i = 0; i < text.length; i++) {
    const code = text.charCodeAt(i);
    if ((code < 32 && code !== 9) || (code >= 127 && code < 160)) return true;
  }
  return false;
}

function Controls({ detail, declared, inRoster, onAsk }: {
  readonly detail: Detail;
  readonly declared: ReadonlySet<InterventionKind>;
  readonly inRoster: boolean;
  readonly onAsk: Agents['ask'];
}) {
  const [asking, setAsking] = useState<Asking>('none');
  const [draft, setDraft] = useState('');
  const [line, setLine] = useState('');
  const run = detail.run;
  if (run === null) return null;
  if (runEnded(run.state)) return <p>The run has ended</p>;
  if (!inRoster) return <p className="hint">Interventions unavailable: the session is not in the roster</p>;
  const busy = detail.sending;
  const canMessage = declared.has('message');
  const canCancel = declared.has('cancel');

  return (
    <div className="ag-controls">
      <div>
        <button
          type="button"
          disabled={!canMessage || busy}
          aria-describedby={canMessage ? undefined : 'ag-no-message'}
          aria-expanded={asking === 'message'}
          onClick={() => {
            setAsking('message');
            setLine('');
          }}
        >
          Message
        </button>
        {canMessage ? null : (
          <span id="ag-no-message" className="hint">
            Not supported by this agent
          </span>
        )}{' '}
        <button
          type="button"
          disabled={!canCancel || busy}
          aria-describedby={canCancel ? undefined : 'ag-no-cancel'}
          aria-expanded={asking === 'cancel'}
          onClick={() => {
            setAsking('cancel');
          }}
        >
          Cancel
        </button>
        {canCancel ? null : (
          <span id="ag-no-cancel" className="hint">
            Not supported by this agent
          </span>
        )}
      </div>
      {asking === 'message' && canMessage ? (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (draft.trim() === '') {
              setLine('Type a message to send');
            } else if (hasControl(draft)) {
              setLine('A message holds no control characters other than tab');
            } else {
              onAsk({ kind: 'message', body: draft });
              setDraft('');
              setAsking('none');
            }
          }}
        >
          <label>
            Message to the agent
            <input
              type="text"
              value={draft}
              maxLength={4000}
              onChange={(event) => {
                setDraft(event.target.value);
              }}
            />
          </label>{' '}
          <button type="submit" disabled={busy}>
            Send
          </button>{' '}
          <button
            type="button"
            onClick={() => {
              setDraft('');
              setAsking('none');
            }}
          >
            Discard
          </button>
          {line === '' ? null : <p role="alert">{line}</p>}
        </form>
      ) : null}
      {asking === 'cancel' && canCancel ? (
        <div role="alertdialog" aria-label="Cancel this run?">
          <p>Cancel this run?</p>
          <button
            type="button"
            disabled={busy}
            onClick={() => {
              onAsk({ kind: 'cancel' });
              setAsking('none');
            }}
          >
            Yes, cancel the run
          </button>{' '}
          <button
            type="button"
            onClick={() => {
              setAsking('none');
            }}
          >
            Keep running
          </button>
        </div>
      ) : null}
    </div>
  );
}

export function RunDetail({ agents, now }: { readonly agents: Agents; readonly now: number }) {
  const detail = agents.state.detail;
  if (detail === null) return null;
  const run = detail.run;
  const session = agents.state.roster.find((s) => s.id === detail.sessionId);
  const declared = new Set<InterventionKind>(session?.interventions ?? []);
  const href = safeLink(run?.link);
  const ended = run !== null && runEnded(run.state);

  return (
    <article aria-label="Run detail" className="ag-run">
      <header>
        <button type="button" onClick={agents.closeRun}>
          Back to conversation
        </button>
        <h2>Run #{detail.runId}</h2>
      </header>
      {run === null ? (
        <p role="status">Loading…</p>
      ) : (
        <dl>
          <dt>State</dt>
          <dd>
            <span data-testid="run-state">{run.state}</span>
            {session?.outcome_unknown === true && !ended ? <span> · outcome unknown</span> : null}
          </dd>
          <dt>Task</dt>
          <dd>{run.task}</dd>
          {run.link === undefined ? null : (
            <>
              <dt>Link</dt>
              <dd>
                {href === null ? (
                  run.link
                ) : (
                  <a href={href} target="_blank" rel="noopener noreferrer">
                    {run.link}
                  </a>
                )}
              </dd>
            </>
          )}
          <dt>Started</dt>
          <dd>
            {clockSeconds(run.started_at)} ({ago(now, run.started_at)})
          </dd>
          {run.ended_at === undefined ? null : (
            <>
              <dt>Ended</dt>
              <dd>{clockSeconds(run.ended_at)}</dd>
            </>
          )}
          {run.outcome === undefined || run.outcome === '' ? null : (
            <>
              <dt>Outcome</dt>
              <dd className="ag-prose">{run.outcome}</dd>
            </>
          )}
        </dl>
      )}
      {agents.state.link === 'live' ? null : <p className="stale">Live updates paused: reconnecting…</p>}
      {detail.problem === null ? null : <p role="alert">{detail.problem}</p>}
      <h3>Activity ({detail.activity.length})</h3>
      <ol className="ag-activity" aria-label="Activity">
        {detail.activity.map((e) => (
          <li key={e.id}>
            <time>{clockSeconds(e.at)}</time> <span className="ag-kind">{e.kind}</span> <span>{e.summary}</span>
            {e.outcome === undefined || e.outcome === '' ? null : <span className="ag-prose"> → {e.outcome}</span>}
          </li>
        ))}
      </ol>
      <h3>Interventions ({detail.interventions.length})</h3>
      <ol className="ag-interventions" aria-label="Interventions">
        {detail.interventions.map((iv) => (
          <li key={iv.id}>
            <span>{kindWord[iv.kind]}</span> · <span data-testid={`intervention-${String(iv.id)}`}>{interventionWord(iv)}</span>
            {iv.body === undefined || iv.body === '' ? null : <span className="ag-prose"> · {iv.body}</span>}
          </li>
        ))}
      </ol>
      <Controls key={detail.runId} detail={detail} declared={declared} inRoster={session !== undefined && agents.state.link === 'live'} onAsk={agents.ask} />
    </article>
  );
}
