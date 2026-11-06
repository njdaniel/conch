import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import * as api from './api';
import { canDo } from './admin/permissions';
import type { ChannelMemberV1 } from './schema/channel';
import type { WhoAmIResponseV1 } from './schema/principal';
import type { MessageV2 } from './schema/message';

// Standing is the viewer's moderation standing: here, in the channel on screen
// (from its members), and the two facts that carry to another channel, which
// the channel list then settles: a signed-in human, and an operator.
interface StandingValue { readonly here: boolean; readonly human: boolean; readonly operator: boolean }
const Standing = createContext<StandingValue>({ here: false, human: false, operator: false });
export function ModerationScope({ who, members, guest, children }: { readonly who: WhoAmIResponseV1; readonly members: readonly ChannelMemberV1[]; readonly guest: boolean; readonly children: ReactNode }) {
  const human = !guest && who.kind === 'human';
  const operator = human && canDo(who, 'manage_channels');
  const here = human && members.some((m) => m.principal_id === who.id && (m.manages === true || operator));
  return <Standing.Provider value={{ here, human, operator }}>{children}</Standing.Provider>;
}
export const useMayModerate = () => useContext(Standing).here;
// useMayModerateIn is the standing in a named channel that may not be the one
// on screen (the voice channel a roster belongs to): the screen's own answer
// for that channel, an operator's anywhere, else the channel list's `manages`.
export function useMayModerateIn(channel: string | null, viewed: string | null, channels: readonly { readonly name: string; readonly manages?: boolean }[]): boolean {
  const standing = useContext(Standing);
  if (channel === null) return false;
  if (channel === viewed) return standing.here;
  return standing.human && (standing.operator || channels.find((c) => c.name === channel)?.manages === true);
}
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
