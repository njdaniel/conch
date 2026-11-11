import { createContext, useCallback, useContext, useEffect, useMemo, useReducer, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import * as api from './api';
import { EmojiPicker } from './EmojiPicker';
import { emojiNow } from './emoji/emoji';
import { canDo } from './admin/permissions';
import { guestName } from './guestName';
import { MAX_REACTION_BATCH_MESSAGES, DEFAULT_REACTOR_LIMIT, type ReactionSummaryV1, type ReactionCountV1, type ReactionV1 } from './schema/reaction';
import { compareReactionTime, emptyReactions, optimisticReactions, reactionReducer } from './state/reactions';
import type { Person } from './useLiveChannel';

interface Intent { readonly emoji: string; readonly set: boolean; readonly revision: number }
interface Controls {
  readonly version: (id: number) => number;
  readonly counts: (id: number) => readonly ReactionCountV1[];
  readonly pending: ReadonlyMap<number, Intent>;
  readonly errors: ReadonlyMap<number, string>;
  readonly ready: ReadonlySet<number>;
  readonly cooling: ReadonlySet<number>;
  readonly unsupported: boolean;
  readonly change: (id: number, emoji: string, set: boolean) => void;
  readonly retry: (id: number) => void;
  readonly accept: (summary: ReactionSummaryV1, revision: number) => void;
}
const Context = createContext<Controls | null>(null);
export function useHasReactions(): boolean {
  const controls = useContext(Context);
  return controls !== null && !controls.unsupported;
}
const none: readonly ReactionCountV1[] = [];

function reactionFailure(result: { readonly code?: string; readonly retryAfter?: number }): string {
  switch (result.code) {
    case 'rate_limited': return `Too many reactions. Try again after ${String(result.retryAfter ?? 60)} seconds.`;
    case 'reaction_not_permitted': return 'You may not react to this message.';
    case 'invalid_reaction': return 'This emoji is not supported by the server.';
    case 'too_many_reactions': return 'Reaction limit reached for this message or your account.';
    case 'forbidden': return 'You do not have permission to react here.';
    case 'message_not_found':
    case api.channelNotFound: return 'This message is no longer available.';
    default: return 'Reaction could not be saved. Try again.';
  }
}

export function ReactionScope({ channel, channelId, ids, children, onUnauthorized }: {
  readonly channel: string; readonly channelId: number; readonly ids: readonly number[];
  readonly children: ReactNode; readonly onUnauthorized?: () => void;
}) {
  const [state, dispatch] = useReducer(reactionReducer, emptyReactions);
  const now = useRef(state);
  now.current = state;
  const idsNow = useRef(ids);
  idsNow.current = ids;
  const [pending, setPending] = useState<ReadonlyMap<number, Intent>>(new Map());
  const locks = useRef(new Map<number, Intent>());
  const failures = useRef(new Map<number, Intent>());
  const [errors, setErrors] = useState<ReadonlyMap<number, string>>(new Map());
  const [cooling, setCooling] = useState<ReadonlySet<number>>(new Set());
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  const [ready, setReady] = useState<ReadonlySet<number>>(new Set());
  const hydrated = useRef(new Set<number>());
  const confirming = useRef(new Set<number>());
  const buffered = useRef(new Map<number, ReactionSummaryV1>());
  const [unsupported, setUnsupported] = useState(false);
  const [epoch, setEpoch] = useState(0);
  const [connected, setConnected] = useState(true);
  const alive = useRef(true);
  const generation = useRef(0);
  useEffect(() => { alive.current = true; return () => { alive.current = false; for (const timer of timers.current) clearTimeout(timer); }; }, []);
  function error(id: number, line?: string) {
    setErrors((old) => {
      const next = new Map(old);
      if (line === undefined) next.delete(id); else next.set(id, line);
      return next;
    });
  }
  useEffect(() => api.watchReactions(channel, channelId, (event) => {
    if (event.kind === 'summary') {
      if (!idsNow.current.includes(event.summary.message_id)) return;
      if (now.current.unversioned.has(event.summary.message_id)) {
        // Omitted batch entries (not visible to this reader) have no timestamp; a visible message with no reactions is answered with a timestamped empty summary. An apparently newer frame
        // may have been computed before that empty read: confirm it in a batch
        // instead of resurrecting a removed reaction from a delayed frame.
        const held = buffered.current.get(event.summary.message_id);
        if (held === undefined || compareReactionTime(event.summary.as_of, held.as_of) > 0) buffered.current.set(event.summary.message_id, event.summary);
        if (!confirming.current.has(event.summary.message_id)) {
          confirming.current.add(event.summary.message_id);
          hydrated.current.delete(event.summary.message_id);
          setEpoch((old) => old + 1);
        }
      } else dispatch({ type: 'summary', summary: event.summary, channelId });
    } else {
      // A close may be revocation. Drop participant/count state immediately;
      // the existing session/channel checks own signing out and access loss.
      setConnected(event.kind === 'open');
      if (event.kind === 'closed') {
        generation.current++;
        locks.current.clear(); failures.current.clear();
        confirming.current.clear(); buffered.current.clear();
        setPending(new Map()); setErrors(new Map()); setCooling(new Set());
        dispatch({ type: 'reset' });
        setReady(new Set());
      }
      hydrated.current.clear();
      setEpoch((old) => old + 1);
    }
  }), [channel, channelId]);

  const key = ids.join(',');
  useEffect(() => {
    if (unsupported || !connected) return;
    let active = true;
    const started = generation.current;
    const held = new Set(idsNow.current);
    hydrated.current = new Set([...hydrated.current].filter((id) => held.has(id)));
    setReady((old) => new Set([...old].filter((id) => held.has(id))));
    setErrors((old) => new Map([...old].filter(([id]) => held.has(id))));
    for (const id of failures.current.keys()) if (!held.has(id)) failures.current.delete(id);
    for (const id of confirming.current) if (!held.has(id)) confirming.current.delete(id);
    for (const id of buffered.current.keys()) if (!held.has(id)) buffered.current.delete(id);
    for (const id of locks.current.keys()) if (!held.has(id)) locks.current.delete(id);
    setPending(new Map(locks.current));
    dispatch({ type: 'prune', ids: held });
    const missing = idsNow.current.filter((id) => !hydrated.current.has(id));
    async function hydrate() {
      for (let i = 0; i < missing.length; i += MAX_REACTION_BATCH_MESSAGES) {
        const batch = missing.slice(i, i + MAX_REACTION_BATCH_MESSAGES);
        const before = now.current;
        const framesBefore = new Map(buffered.current);
        const result = await api.readReactions(channel, batch);
        if (!active || started !== generation.current) return;
        for (const id of batch) confirming.current.delete(id);
        if (!result.ok) {
          if (result.failure === 'unauthorized') { onUnauthorized?.(); return; }
          if (result.code === 'reactions_unsupported' || result.code === 'reactions_require_auth') { setUnsupported(true); return; }
          for (const id of batch) error(id, 'Cannot load reactions. Try again.');
          continue;
        }
        dispatch({ type: 'batch', ids: batch, summaries: result.value.summaries, channelId, before });
        for (const id of batch) {
          hydrated.current.add(id); error(id);
          const frame = buffered.current.get(id);
          buffered.current.delete(id);
          if (frame === undefined) continue;
          if (result.value.summaries.some((summary) => summary.message_id === id)) {
            dispatch({ type: 'summary', summary: frame, channelId });
          } else if (frame !== framesBefore.get(id)) {
            // An omitted entry has no clock to compare with a newer frame.
            // Confirm once more rather than resurrect a delayed reaction.
            buffered.current.set(id, frame);
            confirming.current.add(id);
            hydrated.current.delete(id);
            setEpoch((old) => old + 1);
          }
        }
        setReady((old) => new Set([...old, ...batch]));
      }
    }
    void hydrate();
    return () => { active = false; };
  }, [key, channel, channelId, epoch, unsupported, connected, onUnauthorized]);

  function change(id: number, emoji: string, set: boolean) {
    if (locks.current.has(id) || !ready.has(id) || cooling.has(id)) return;
    const intent = { emoji, set, revision: now.current.revisions.get(id) ?? 0 };
    locks.current.set(id, intent);
    setPending(new Map(locks.current));
    error(id);
    const started = generation.current;
    void api.writeReaction(channel, id, emoji, set).then((result) => {
      if (!alive.current || started !== generation.current || !idsNow.current.includes(id)) return;
      locks.current.delete(id);
      setPending(new Map(locks.current));
      if (result.ok) {
        failures.current.delete(id);
        accept(result.value, intent.revision);
      } else if (result.failure === 'unauthorized') onUnauthorized?.();
      else {
        failures.current.set(id, intent);
        if (result.code === 'rate_limited') {
          setCooling((old) => new Set(old).add(id));
          const timer = setTimeout(() => {
            timers.current.delete(timer);
            if (alive.current && started === generation.current) setCooling((old) => new Set([...old].filter((held) => held !== id)));
          }, (result.retryAfter ?? 60) * 1000);
          timers.current.add(timer);
        }
        error(id, reactionFailure(result));
        if (result.code === 'message_not_found' || result.code === api.channelNotFound) {
          dispatch({ type: 'prune', ids: new Set(idsNow.current.filter((held) => held !== id)) });
          setReady((old) => new Set([...old].filter((held) => held !== id)));
        }
      }
    });
  }
  function accept(summary: ReactionSummaryV1, revision: number) {
    if (now.current.unversioned.has(summary.message_id) && now.current.revisions.get(summary.message_id) !== revision) {
      hydrated.current.delete(summary.message_id);
      setEpoch((old) => old + 1);
    } else dispatch({ type: 'summary', summary, channelId, authoritative: true });
  }
  function retry(id: number) {
    const failed = failures.current.get(id);
    if (failed !== undefined && ready.has(id)) change(id, failed.emoji, failed.set);
    else { hydrated.current.delete(id); setEpoch((old) => old + 1); }
  }
  const controls: Controls = {
    version: (id) => state.revisions.get(id) ?? 0,
    counts: (id) => {
      const counts = state.summaries.get(id)?.reactions ?? none;
      const intent = pending.get(id);
      return intent !== undefined && (state.revisions.get(id) ?? 0) === intent.revision ? optimisticReactions(counts, intent.emoji, intent.set) : counts;
    },
    pending, errors, ready, cooling, unsupported, change, retry, accept,
  };
  return <Context.Provider value={controls}>{children}</Context.Provider>;
}

export function ReactionBar({ id, channel, label, people, onUnauthorized, logNavigation = false, children, enabled = true }: {
  readonly logNavigation?: boolean;
  readonly children?: ReactNode;
  readonly enabled?: boolean;
  readonly id: number; readonly channel: string; readonly label: string;
  readonly people: ReadonlyMap<number, Person>; readonly onUnauthorized?: () => void;
}) {
  const controls = useContext(Context);
  const [picker, setPicker] = useState(false);
  const [listing, setListing] = useState<string | null>(null);
  const opener = useRef<HTMLButtonElement | null>(null);
  // The main log enters reactions with Right Arrow and keeps explicit -1
  // stops. Thread controls use normal Tab access; both support toolbar arrows.
  const toolbar = useRef<HTMLDivElement>(null);
  const canAdd = useRef(false);
  const reactionPair = useCallback((node: HTMLSpanElement | null) => {
    if (node === null) return;
    // Ref cleanup runs before React removes the pair. Recover focus while
    // the active chip or reactor-list button is still in the document.
    return () => {
      if (!node.contains(document.activeElement)) return;
      const add = toolbar.current?.querySelector<HTMLButtonElement>('button.reaction-add');
      if (canAdd.current && add !== undefined && add !== null) add.focus();
      else toolbar.current?.closest<HTMLElement>('li[data-message-id]')?.focus();
    };
  }, []);
  function onControlKey(event: KeyboardEvent<HTMLDivElement>) {
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey || event.nativeEvent.isComposing) return;
    const buttons = Array.from(toolbar.current?.querySelectorAll<HTMLButtonElement>('button[data-reaction-control]') ?? []).filter((button) => !button.disabled && button.getAttribute('aria-disabled') !== 'true');
    const at = buttons.indexOf(event.target as HTMLButtonElement);
    if (at < 0) return; // Picker and reactor dialogs keep their own keys.
    let next: number;
    switch (event.key) {
      case 'ArrowRight': next = Math.min(at + 1, buttons.length - 1); break;
      case 'ArrowLeft': next = at - 1; break;
      case 'Home': next = 0; break;
      case 'End': next = buttons.length - 1; break;
      case 'Escape': next = -1; break;
      default: return;
    }
    event.preventDefault();
    event.stopPropagation();
    if (next < 0) toolbar.current?.closest<HTMLElement>('li[data-message-id]')?.focus();
    else buttons[next]?.focus();
  }
  const names = useMemo(() => new Map(emojiNow()?.all.map((e) => [e.key, e.name]) ?? []), []);
  const close = useCallback(() => { setPicker(false); setListing(null); opener.current?.focus(); }, []);
  if (controls === null || controls.unsupported || !enabled) return <>{children}</>;
  const counts = controls.counts(id);
  const busy = controls.pending.has(id);
  const cooling = controls.cooling.has(id);
  const ready = controls.ready.has(id);
  canAdd.current = ready && !cooling;
  const problem = controls.errors.get(id);
  return <div ref={toolbar} onKeyDown={onControlKey} className="reaction-controls" role="group" aria-label={`Reactions for ${label}`} aria-live="off">
    <div className="message-reaction-actions">
      {children}
      {/* Keep the opener focusable during a save so choosing an emoji returns
          focus reliably; aria-disabled and the click guard block repeat opens. */}
      <button type="button" tabIndex={logNavigation ? -1 : 0} data-reaction-control="" className="reaction-add" aria-label={`Add reaction to ${label}`} aria-expanded={picker} aria-busy={busy} aria-disabled={busy || !ready || cooling} disabled={!ready || cooling} onClick={(event) => { if (busy) return; opener.current = event.currentTarget; setListing(null); setPicker(!picker); }}>Add reaction</button>
    </div>
    {counts.length > 0 || problem !== undefined || (picker && ready) || (listing !== null && ready) ? <div className="reactions">
      {counts.map((r) => <span ref={reactionPair} className="reaction-pair" key={r.emoji}>
        <button type="button" tabIndex={logNavigation ? -1 : 0} data-reaction-control="" className="reaction-chip" aria-label={`${names.get(r.emoji) ?? 'Emoji'}: ${String(r.count)} reaction${r.count === 1 ? '' : 's'}`} aria-pressed={Boolean(r.me)} aria-busy={busy} disabled={busy || !ready || cooling} onClick={() => { controls.change(id, r.emoji, !r.me); }}><span aria-hidden="true">{r.emoji} {r.count}</span></button>
        <button type="button" tabIndex={logNavigation ? -1 : 0} data-reaction-control="" className="reaction-list-button" aria-label={`Who reacted with ${names.get(r.emoji) ?? 'this emoji'}`} onClick={(event) => { opener.current = event.currentTarget; setPicker(false); setListing(r.emoji); }}>Who?</button>
      </span>)}
      {problem === undefined ? null : <span role="status" className="reaction-error">{problem} <button type="button" tabIndex={logNavigation ? -1 : 0} data-reaction-control="" disabled={busy || cooling} onClick={() => { controls.retry(id); }}>Retry reaction</button></span>}
      {picker && ready ? <EmojiPicker onClose={close} onPick={(emoji) => { close(); controls.change(id, emoji.key, true); }} /> : null}
      {listing === null || !ready ? null : <ReactorPage key={`${channel}/${listing}`} channel={channel} id={id} emoji={listing} name={names.get(listing) ?? 'emoji'} people={people} version={controls.version(id)} onClose={close} onUnauthorized={onUnauthorized} />}
    </div> : null}
    {busy ? <span className="vh">Saving reaction…</span> : !ready && problem === undefined ? <span className="vh">Loading reactions…</span> : null}
  </div>;
}

function ReactorPage({ channel, id, emoji, name, people, version, onClose, onUnauthorized }: {
  readonly channel: string; readonly id: number; readonly emoji: string; readonly name: string;
  readonly people: ReadonlyMap<number, Person>; readonly version: number; readonly onClose: () => void; readonly onUnauthorized?: () => void;
}) {
  const controls = useContext(Context);
  const [mayModerate, setMayModerate] = useState(false);
  const [confirm, setConfirm] = useState<{ readonly principal: number; readonly all: boolean } | null>(null);
  const [saving, setSaving] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    void Promise.all([api.whoAmI(), api.listMembers(channel)]).then(([who, members]) => {
      if (!live) return;
      if (!who.ok || !members.ok) {
        if ((!who.ok && who.failure === 'unauthorized') || (!members.ok && members.failure === 'unauthorized')) { onClose(); onUnauthorized?.(); }
        return;
      }
      setMayModerate(who.value.kind === 'human' && members.value.some((member) => member.principal_id === who.value.id && (member.manages === true || canDo(who.value, 'manage_channels'))));
    });
    return () => { live = false; };
  }, [channel, onClose, onUnauthorized]);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  function moderate() {
    if (confirm === null || saving) return;
    setSaving(true); setProblem(null);
    void api.moderateReaction(channel, id, confirm.principal, confirm.all ? undefined : emoji).then((result) => {
      if (!mounted.current) return;
      setSaving(false);
      if (result.ok) { controls?.accept(result.value, version); setConfirm(null); setAttempt((old) => old + 1); }
      else if (result.failure === 'unauthorized') { onClose(); onUnauthorized?.(); }
      else { setProblem('Could not remove reactions.'); if (result.code === 'forbidden') setMayModerate(false); }
    });
  }
  const [after, setAfter] = useState(0);
  const [page, setPage] = useState<readonly ReactionV1[]>([]);
  const [next, setNext] = useState(0);
  const [loading, setLoading] = useState(true);
  const [failed, setFailed] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const close = useRef<HTMLButtonElement>(null);
  useEffect(() => { close.current?.focus(); }, []);
  useEffect(() => {
    let live = true;
    setLoading(true); setFailed(false); setPage([]); setNext(0);
    void api.listReactors(channel, id, emoji, after, DEFAULT_REACTOR_LIMIT).then((result) => {
      if (!live) return;
      setLoading(false);
      if (!result.ok) {
        if (result.failure === 'unauthorized') { onClose(); onUnauthorized?.(); }
        else setFailed(true);
      } else { setPage(result.value.reactors); setNext(result.value.next_after ?? 0); }
    });
    return () => { live = false; };
  }, [channel, id, emoji, after, attempt, version, onUnauthorized, onClose]);
  return <div className="reactor-page" role="dialog" aria-label={`Who reacted with ${name}`} onKeyDown={(event) => { if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onClose(); } }}>
    <strong>{emoji} {name}</strong> <button ref={close} type="button" onClick={onClose}>Close reactors</button>
    {loading ? <p role="status">Loading reactors…</p> : null}
    {failed ? <p role="status">Cannot load reactors. <button type="button" onClick={() => { setAttempt((old) => old + 1); }}>Retry list</button></p> : null}
    <ul>{page.map((r) => <li key={r.principal_id}>{r.guest_reactor === undefined ? people.get(r.principal_id)?.name ?? `hidden-${String(r.principal_id)}` : guestName(r.guest_reactor)}{mayModerate ? <span> <button type="button" disabled={saving} onClick={() => { setConfirm({ principal: r.principal_id, all: false }); }}>Remove this reaction</button> <button type="button" disabled={saving} onClick={() => { setConfirm({ principal: r.principal_id, all: true }); }}>Remove all their reactions</button></span> : null}</li>)}</ul>
    {confirm === null ? null : <div role="group" aria-label="Confirm reaction removal"><p>Remove {confirm.all ? 'all reactions of this person' : 'this reaction'} for everyone who can see the message; the person may react again.</p><button type="button" disabled={saving} onClick={moderate}>{saving ? 'Removing…' : 'Confirm removal'}</button><button type="button" disabled={saving} onClick={() => { setConfirm(null); }}>Cancel removal</button></div>}
    {problem === null ? null : <p role="status">{problem}</p>}
    {!loading && !failed && page.length === 0 ? <p>No reactions on this page.</p> : null}
    {after > 0 ? <button type="button" disabled={loading} onClick={() => { setAfter(0); }}>First page</button> : null}
    {next > after ? <button type="button" disabled={loading} onClick={() => { setAfter(next); }}>Next reactors</button> : null}
  </div>;
}
