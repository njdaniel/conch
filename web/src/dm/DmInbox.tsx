// The DM inbox: a navigation of its own, beside Channels and apart from the
// member directory. Each row is one conversation: the other person, an unread
// count, and Hide (for the viewer only). Up and Down move between rows.
import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react';
import type { DMAcceptPolicy, DMConversationV1 } from '../schema/generated';
import { Avatar } from '../Icons';
import { timeOf } from '../time';
import { isDmOperationServed, type DmApi } from './api';
import { useDm, type DmValue } from './DmContext';
import { peerName } from './model';

const policies: readonly { readonly value: DMAcceptPolicy; readonly label: string }[] = [
  { value: 'everyone', label: 'Everyone' },
  { value: 'shared_channel', label: 'People I share a channel with' },
  { value: 'nobody', label: 'Nobody' },
];

function moveFocus(event: KeyboardEvent<HTMLUListElement>) {
  if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
  const rows = [...event.currentTarget.querySelectorAll<HTMLButtonElement>('button.dm-open')];
  const at = rows.findIndex((row) => row === document.activeElement);
  if (at < 0) return;
  event.preventDefault();
  rows[(at + (event.key === 'ArrowDown' ? 1 : rows.length - 1)) % rows.length]?.focus();
}

function Row({ conversation, selected, onOpen, onHide }: {
  readonly conversation: DMConversationV1;
  readonly selected: boolean;
  readonly onOpen: (button: HTMLButtonElement) => void;
  readonly onHide: (button: HTMLButtonElement) => void;
}) {
  const name = peerName(conversation);
  return (
    <li className={selected ? 'dm-row selected' : 'dm-row'}>
      <Avatar id={conversation.peer_id} name={name} />
      <button type="button" className="dm-open" aria-current={selected ? 'true' : undefined} onClick={(event) => { onOpen(event.currentTarget); }}>
        <span className="dm-name">{name}</span>
        {conversation.last_message_at === undefined ? null : <time className="hint" dateTime={conversation.last_message_at}>{timeOf(conversation.last_message_at)}</time>}
        {isDmOperationServed('markDmRead') && conversation.unread > 0 ? <span className="badge">{`${String(conversation.unread)} unread`}</span> : null}
      </button>
      <button type="button" className="dm-hide" aria-label={`Hide the conversation with ${name}`} onClick={(event) => { onHide(event.currentTarget); }}>
        Hide
      </button>
    </li>
  );
}

function ContactPreference({ read, save }: { readonly read: NonNullable<DmApi['preferences']>; readonly save: DmApi['setPreferences'] }) {
  const [accept, setAccept] = useState<DMAcceptPolicy | null>(null);
  const [problem, setProblem] = useState('');
  const [attempt, setAttempt] = useState(0);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    let current = true;
    void read().then((result) => {
      if (!current) return;
      if (result.ok) setAccept(result.value.accept);
      else setProblem('Could not load the setting.');
    });
    return () => { current = false; };
  }, [read, attempt]);

  async function choose(value: string) {
    const policy = policies.find((p) => p.value === value);
    if (policy === undefined || saving) return;
    setSaving(true);
    setProblem('');
    const result = await save(policy.value);
    if (result.ok) setAccept(result.value.accept);
    else setProblem('Could not save the setting.');
    setSaving(false);
  }

  return (
    <>
      <label className="dm-preference">
        Who can message me
        <select value={accept ?? ''} disabled={accept === null || saving} onChange={(event) => void choose(event.target.value)}>
          {accept === null ? <option value="">{problem === '' ? 'Loading…' : 'Unavailable'}</option> : null}
          {policies.map((p) => <option key={p.value} value={p.value}>{p.label}</option>)}
        </select>
      </label>
      {problem === '' ? null : <p role="alert" className="hint">{problem}</p>}
      {accept === null && problem !== '' ? <button type="button" onClick={() => { setProblem(''); setAttempt(attempt + 1); }}>Retry contact preference</button> : null}
    </>
  );
}

type OnOpen = (button: HTMLElement) => void;

export function DmInbox({ onOpen }: { readonly onOpen?: OnOpen }) {
  const dm = useDm();
  return dm === null ? null : <InboxPanel key={dm.scope} dm={dm} onOpen={onOpen} />;
}

function InboxPanel({ dm, onOpen }: { readonly dm: DmValue; readonly onOpen: OnOpen | undefined }) {
  const [problem, setProblem] = useState('');
  const [hiding, setHiding] = useState<number | null>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const restore = useRef<{ readonly button: HTMLButtonElement; readonly index: number } | null>(null);
  const { inbox, api } = dm;
  const { status, conversations, enabled, selected } = inbox.state;
  useLayoutEffect(() => {
    const target = restore.current;
    if (target === null || target.button.isConnected) return;
    restore.current = null;
    if (document.activeElement !== document.body || heading.current?.closest('[inert]') !== null) return;
    const rows = list.current?.querySelectorAll<HTMLButtonElement>('.dm-open');
    const next = rows?.item(Math.min(target.index, rows.length - 1));
    (next ?? heading.current).focus();
  }, [conversations]);
  // A server that serves no DMs shows nothing at all.
  if (status === 'unsupported') return null;
  async function hide(conversation: DMConversationV1, button: HTMLButtonElement, index: number) {
    if (hiding !== null) return;
    setProblem('');
    setHiding(conversation.id);
    if (document.activeElement === button) restore.current = { button, index };
    const failure = await inbox.setState(conversation, 'hidden');
    setHiding(null);
    if (failure !== null) {
      restore.current = null;
      setProblem('Could not hide the conversation.');
    }
  }
  return (
    <nav aria-label="Direct messages" className="dm-inbox">
      <h2 ref={heading} tabIndex={-1} data-dm-inbox-heading>
        Direct messages
        {isDmOperationServed('markDmRead') && inbox.unread > 0 ? <span className="badge">{`${String(inbox.unread)} unread`}</span> : null}
      </h2>
      {status === 'loading' ? <p role="status" className="empty">Loading messages…</p> : null}
      {status === 'failed' ? (
        <div className="problem">
          <p className="empty">Cannot load direct messages</p>
          <button type="button" onClick={() => void inbox.refresh()}>Try again</button>
        </div>
      ) : null}
      {status === 'own_sign_in_required' ? <p className="empty">Direct messages need your own sign-in.</p> : null}
      {status === 'revoked' ? <p role="alert" className="empty">Direct messages are no longer available to you.</p> : null}
      {status === 'ready' && !enabled ? <p className="empty">Direct messages are off.</p> : null}
      {status === 'ready' && conversations.length === 0 ? <p className="empty">No direct messages yet.{enabled ? ' Use Message on a person to start one.' : ''}</p> : null}
      {status === 'ready' && conversations.length > 0 ? (
        <ul ref={list} aria-label="Conversations" onKeyDown={moveFocus}>
          {conversations.map((c, index) => (
            <Row key={c.id} conversation={c} selected={selected === c.id} onOpen={(button) => {
              inbox.select(c.id);
              onOpen?.(button);
            }} onHide={(button) => void hide(c, button, index)} />
          ))}
        </ul>
      ) : null}
      {problem === '' ? null : <p role="alert" className="hint">{problem}</p>}
      {status === 'ready' && api.preferences !== undefined ? <ContactPreference read={api.preferences} save={api.setPreferences} /> : null}
    </nav>
  );
}
