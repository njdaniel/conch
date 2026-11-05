// The card an AI administrator's approval is drawn as (issue #1334,
// docs/design/ai-administrators.md §3.5.1): a conch.admin_action.v1 payload on
// an approval shows the action, the route it would run, the target, the
// parameters and the agent's reason, and the two buttons. The facts are the
// server's, derived from the stored action; the reason is the agent's own
// words and is labelled so, since anything an agent read may have written it.
// Everything here is drawn as text. Choosing a button casts the viewer's
// decision on the approval; the server decides whether they may.
import { useState } from 'react';
import { personRefusal } from './admin/PersonActions';
import * as api from './api';
import type { AdminActionV1 } from './schema/admin';
import type { ApprovalV1 } from './schema/approval';

const paramsText = (params: unknown): string => (params === undefined || params === null ? 'none' : JSON.stringify(params, null, 2));

export function AdminActionCard({
  approval,
  action,
  requester,
  viewer,
  readOnly = false,
  onUnauthorized,
}: {
  readonly approval: ApprovalV1;
  readonly action: AdminActionV1;
  // The agent's name, as the page knows it.
  readonly requester: string;
  readonly viewer: number;
  // The viewer may see this approval but not decide it.
  readonly readOnly?: boolean;
  readonly onUnauthorized?: (() => void) | undefined;
}) {
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<string | null>(null);
  const [line, setLine] = useState<string | null>(null);
  const yes = approval.options.find((o) => o.kind === 'approve');
  const no = approval.options.find((o) => o.kind === 'reject');
  const open = (approval.state === 'pending' || approval.state === 'escalated') && done === null && !readOnly;
  const target = action.target;

  function decide(option: { readonly id: string; readonly label: string } | undefined) {
    if (option === undefined || busy) return;
    setBusy(true);
    setLine(null);
    void api.castDecision(approval.id, viewer, option.id, `Decided from the browser: ${option.label}.`).then((result) => {
      setBusy(false);
      if (result.ok) {
        setDone(`You chose: ${option.label}.`);
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized?.();
        return;
      }
      setLine(personRefusal(result, 'Nothing was decided.'));
    });
  }

  return (
    <article className="approval-card admin-action-card" aria-label="AI administrator request">
      <strong>{approval.title}</strong>
      <dl>
        <dt>Requested by</dt>
        <dd>{`${requester}, an AI administrator`}</dd>
        <dt>Action</dt>
        <dd>{action.action}</dd>
        <dt>Will run</dt>
        <dd>
          <code>{action.route}</code>
        </dd>
        <dt>Target</dt>
        <dd>{target === undefined ? 'the server' : `${target.kind} #${String(target.id)}${target.name === undefined ? '' : ` "${target.name}"`}`}</dd>
        <dt>Parameters</dt>
        <dd>
          <pre>{paramsText(action.params)}</pre>
        </dd>
        <dt>The agent&apos;s own words</dt>
        <dd>{action.reason === undefined || action.reason === '' ? 'It gave no reason.' : <q>{action.reason}</q>}</dd>
      </dl>
      <div>State: {approval.state}</div>
      {open ? (
        <div className="actions">
          <button
            type="button"
            disabled={busy || yes === undefined}
            onClick={() => {
              decide(yes);
            }}
          >
            Run it
          </button>
          <button
            type="button"
            disabled={busy || no === undefined}
            onClick={() => {
              decide(no);
            }}
          >
            Do not run
          </button>
        </div>
      ) : null}
      {readOnly && approval.state !== 'resolved' ? <p className="hint">You can see this request; you do not decide in this channel.</p> : null}
      {done === null ? null : <p role="status">{done}</p>}
      {line === null ? null : (
        <p role="status" className="notice">
          {line}
        </p>
      )}
    </article>
  );
}
