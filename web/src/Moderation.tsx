import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import * as api from './api';
import { canDo } from './admin/permissions';
import type { ChannelMemberV1 } from './schema/channel';
import type { WhoAmIResponseV1 } from './schema/principal';
import type { MessageV2 } from './schema/message';

const Standing = createContext(false);
export function ModerationScope({ who, members, guest, children }: { readonly who: WhoAmIResponseV1; readonly members: readonly ChannelMemberV1[]; readonly guest: boolean; readonly children: ReactNode }) {
  return <Standing.Provider value={!guest && who.kind === 'human' && members.some((m) => m.principal_id === who.id && (m.manages === true || canDo(who, 'manage_channels')))}>{children}</Standing.Provider>;
}
export const useMayModerate = () => useContext(Standing);
export type ModerationOperation = 'kickVoice' | 'banMember' | 'unbanMember' | 'removeMessage';
export function moderationQuestion(op: ModerationOperation, name: string, channel: string): string {
  switch (op) {
    case 'kickVoice': return `Remove ${name} from voice in #${channel} now. They can rejoin; chat access is unchanged.`;
    case 'banMember': return `Remove ${name} from #${channel} and its nets and block this principal from being added again. Other channels are unaffected. A new account or guest visit is not identified by this ban.`;
    case 'unbanMember': return `Lift the ban on ${name} in #${channel}? Unban does not re-add them to the channel or its nets.`;
    case 'removeMessage': return `Remove ${name} in #${channel} for everyone in its audience. Future reads show a tombstone; server backups and copies already received cannot be recalled.`;
  }
}
export function ModerationConfirm({ op, id, name, channel, onClose, onDone, onUnauthorized }: { readonly op: ModerationOperation; readonly id: number; readonly name: string; readonly channel: string; readonly onClose: () => void; readonly onDone?: () => void; readonly onUnauthorized?: () => void }) {
  const cancel = useRef<HTMLButtonElement>(null);
  const lock = useRef(false);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  useEffect(() => { cancel.current?.focus(); }, []);
  async function save() {
    if (lock.current || done) return;
    lock.current = true; setBusy(true); setLine(null);
    const result = await api.moderate(channel, op, id);
    lock.current = false; setBusy(false);
    if (result.ok) {
      setDone(true);
      setLine(op === 'unbanMember' ? 'Ban lifted or already absent. Unban does not re-add them.' : op === 'banMember' ? 'Ban is in place, including if already banned.' : 'Removed or already absent/removed.');
      onDone?.();
    } else if (result.failure === 'unauthorized') { setLine('Your sign-in has expired. Sign in again.'); onUnauthorized?.(); }
    else setLine(result.code === 'voice_unavailable' ? 'Voice removal was incomplete. Retry; the participant may still be connected.' : result.message ?? (result.failure === 'network' ? 'Cannot reach the server. Try again.' : `The server refused the change${result.code === undefined ? '.' : ` (${result.code}).`}`));
  }
  return <div role="group" aria-label="Confirm moderation" className="member-confirm" onKeyDown={(event) => {
    if (event.key === 'Escape' && !busy) { event.preventDefault(); event.stopPropagation(); onClose(); }
  }}>
    <p>{moderationQuestion(op, name, channel)}</p>
    {line === null ? null : <p role="status">{line}</p>}
    <div className="actions">
      {!done ? <button type="button" disabled={busy} onClick={() => void save()}>{line === null ? 'Confirm' : 'Retry'}</button> : null}
      <button ref={cancel} type="button" disabled={busy} onClick={onClose}>{done ? 'Close' : 'Cancel'}</button>
    </div>
  </div>;
}

// A view's removal ledger contains safe envelopes only. It also covers reads
// started before a removal whose answers arrive after the live frame.
export function useTombstones(channelId?: number) {
  const [removed, setRemoved] = useState<ReadonlyMap<number, MessageV2>>(new Map());
  const latest = useRef(removed);
  useEffect(() => api.watchRemovals((message) => {
    if (channelId !== undefined && channelId !== message.channel_id) return;
    latest.current = new Map(latest.current).set(message.id, message);
    setRemoved(latest.current);
  }), [channelId]);
  const safe = useCallback((message: MessageV2) => latest.current.get(message.id) ?? message, []);
  return { removed, safe };
}
