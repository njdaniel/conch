// The approvals inbox of a conversation (issue #1334, docs/design/
// ai-administrators.md §3.5.1): an AI administrator's request is an ordinary
// approval in its approval channel, which the server does not post as a
// message, so the page reads the open approvals (GET /v1/approvals, the
// caller's channels only) and draws each conch.admin_action.v1 one for this
// channel as a card above the log. It reads when the channel opens and every
// few seconds while it is open. A card stays until the channel closes once seen,
// so a decision's answer is not pulled from under the person who made it; one
// the server no longer lists is shown as resolved, without buttons. Any other
// approval is not drawn here.
import { useEffect, useRef, useState } from 'react';
import { AdminActionCard } from './AdminActionCard';
import * as api from './api';
import type { ApprovalV1 } from './schema/approval';
import { adminActionV1, adminActionV1PayloadSchema, type AdminActionV1 } from './schema/admin';

const everyMs = 4000;

function adminActionOf(approval: ApprovalV1): AdminActionV1 | null {
  const payload = approval.payload;
  if (payload?.schema !== adminActionV1PayloadSchema || adminActionV1(payload.data).length > 0) return null;
  return payload.data as AdminActionV1;
}

export function AdminApprovals({
  channelId,
  viewer,
  names,
  onUnauthorized,
}: {
  readonly channelId: number | undefined;
  readonly viewer: number;
  // The agent's name, by id.
  readonly names: (id: number) => string;
  readonly onUnauthorized?: (() => void) | undefined;
}) {
  const [seen, setSeen] = useState<ReadonlyMap<number, ApprovalV1>>(new Map());
  const [readOnly, setReadOnly] = useState<ReadonlySet<number>>(new Set());
  const unauthorized = useRef(onUnauthorized);
  unauthorized.current = onUnauthorized;

  useEffect(() => {
    if (channelId === undefined) return;
    let live = true;
    const read = () => {
      void api.openApprovals().then((result) => {
        if (!live) return;
        if (!result.ok) {
          if (result.failure === 'unauthorized') unauthorized.current?.();
          // A guest is refused every time: stop asking rather than add a
          // refused, audited request every few seconds. Anything else (a
          // server hiccup, a network failure) is tried again, so an approval
          // is never hidden by one bad answer.
          if (result.failure === 'invalid' && result.code === 'guest_not_allowed') {
            live = false;
            clearInterval(timer);
          }
          return;
        }
        const open = new Map<number, ApprovalV1>();
        for (const a of result.value.approvals) if (a.channel_id === channelId && adminActionOf(a) !== null) open.set(a.id, a);
        setReadOnly(new Set(result.value.readOnly));
        setSeen((held) => {
          const next = new Map<number, ApprovalV1>();
          for (const [id, a] of held) next.set(id, open.has(id) ? open.get(id) ?? a : { ...a, state: 'resolved' });
          for (const [id, a] of open) next.set(id, a);
          return next;
        });
      });
    };
    // The timer first, so a first answer that refuses can stop it.
    const timer = setInterval(read, everyMs);
    read();
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [channelId]);

  if (seen.size === 0) return null;
  return (
    <section aria-label="Requests from AI administrators" className="admin-approvals">
      {[...seen.values()].map((approval) => {
        const action = adminActionOf(approval);
        return action === null ? null : (
          <AdminActionCard
            key={approval.id}
            approval={approval}
            action={action}
            requester={names(approval.requester_id)}
            viewer={viewer}
            readOnly={readOnly.has(approval.id)}
            onUnauthorized={onUnauthorized}
          />
        );
      })}
    </section>
  );
}
