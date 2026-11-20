// The DM inbox: a navigation of its own, beside Channels and apart from the
// member directory. Each row is one conversation: the other person, an unread
// count, and Hide (for the viewer only). Up and Down move between rows.
import { useEffect, useState, type KeyboardEvent } from 'react';
import type { DMAcceptPolicy, DMConversationV1 } from '../schema/generated';
import { Avatar } from '../Icons';
import { timeOf } from '../time';
import { useDm } from './DmContext';

const policies: readonly { readonly value: DMAcceptPolicy; readonly label: string }[] = [
  { value: 'everyone', label: 'Everyone' },
  { value: 'shared_channel', label: 'People I share a channel with' },
  { value: 'nobody', label: 'Nobody' },
];

export const peerName = (c: DMConversationV1): string => (c.peer_name === undefined || c.peer_name.trim() === '' ? 'Former member' : c.peer_name);

function moveFocus(event: KeyboardEvent<HTMLUListElement>) {
  if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
  const rows = [...event.currentTarget.querySelectorAll<HTMLButtonElement>('button.dm-open')];
  const at = rows.findIndex((row) => row === document.activeElement);
  if (at < 0) return;
  event.preventDefault();
  rows[(at + (event.key === 'ArrowDown' ? 1 : rows.length - 1)) % rows.length]?.focus();
}

function Row({ conversation, selected, onOpen, onHide, hidden }: {
  readonly conversation: DMConversationV1;
  readonly selected: boolean;
  readonly onOpen: () => void;
  readonly onHide: () => void;
  readonly hidden: boolean;
}) {
  const name = peerName(conversation);
  return (
    <li className={selected ? 'dm-row selected' : 'dm-row'}>
      <Avatar id={conversation.peer_id} name={name} />
      <button type="button" className="dm-open" aria-current={selected ? 'true' : undefined} onClick={onOpen}>
        <span className="dm-name">{name}</span>
        {conversation.last_message_at === undefined ? null : <time className="hint">{timeOf(conversation.last_message_at)}</time>}
        {conversation.unread > 0 ? <span className="badge">{`${String(conversation.unread)} unread`}</span> : null}
      </button>
      <button type="button" className="dm-hide" aria-label={`${hidden ? 'Show' : 'Hide'} the conversation with ${name}`} onClick={onHide}>
        {hidden ? 'Show' : 'Hide'}
      </button>
    </li>
  );
}

export function DmInbox({ onOpen }: { readonly onOpen?: (conversation: number) => void }) {
  const dm = useDm();
  const [problem, setProblem] = useState('');
  const [accept, setAccept] = useState<DMAcceptPolicy | null>(null);
  const [preference, setPreference] = useState('');
  const ready = dm?.inbox.state.status === 'ready';
  const scope = dm?.scope;
  const preferences = dm?.api;
  // The setting is read once the inbox is, and again for another scope: what
  // the select shows is the server's, never a default standing in for it.
  useEffect(() => {
    setAccept(null);
    if (!ready || preferences === undefined) return;
    let current = true;
    void preferences.preferences().then((r) => {
      if (current && r.ok) setAccept(r.value.accept);
    });
    return () => {
      current = false;
    };
  }, [ready, scope, preferences]);
  if (dm === null) return null;
  const { inbox, api } = dm;
  const { status, conversations, enabled, selected, hidden } = inbox.state;
  // A server that serves no DMs shows nothing at all.
  if (status === 'unsupported') return null;
  const open = (id: number) => {
    inbox.select(id);
    onOpen?.(id);
  };
  async function hide(conversation: DMConversationV1, to: 'hidden' | 'open') {
    setProblem('');
    const failure = await inbox.setState(conversation, to);
    if (failure !== null) setProblem(to === 'hidden' ? 'Could not hide the conversation.' : 'Could not show the conversation.');
  }
  async function choose(value: DMAcceptPolicy) {
    setPreference('');
    const result = await api.setPreferences(value);
    if (result.ok) setAccept(result.value.accept);
    else setPreference('Could not save the setting.');
  }
  return (
    <nav aria-label="Direct messages" className="dm-inbox">
      <h2>
        Direct messages
        {inbox.unread > 0 ? <span className="badge">{`${String(inbox.unread)} unread`}</span> : null}
      </h2>
      {status === 'loading' ? <p className="empty">Loading messages…</p> : null}
      {status === 'failed' ? (
        <div className="problem">
          <p className="empty">Cannot load direct messages</p>
          <button type="button" onClick={inbox.refresh}>Try again</button>
        </div>
      ) : null}
      {status === 'own_sign_in_required' ? <p className="empty">Direct messages need your own sign-in.</p> : null}
      {status === 'ready' && !enabled ? <p className="empty">Direct messages are off.</p> : null}
      {status === 'ready' && conversations.length === 0 ? <p className="empty">No direct messages yet. Use Message on a person to start one.</p> : null}
      {status === 'ready' && conversations.length > 0 ? (
        <ul aria-label="Conversations" onKeyDown={moveFocus}>
          {conversations.map((c) => (
            <Row key={c.id} conversation={c} selected={selected === c.id} hidden={false} onOpen={() => { open(c.id); }} onHide={() => void hide(c, 'hidden')} />
          ))}
        </ul>
      ) : null}
      {problem === '' ? null : <p role="alert" className="hint">{problem}</p>}
      {status === 'ready' ? (
        <>
          {hidden === null ? (
            <button type="button" className="link-button" onClick={() => void inbox.showHidden()}>Show hidden conversations</button>
          ) : hidden.length === 0 ? (
            <p className="hint">No hidden conversations.</p>
          ) : (
            <ul aria-label="Hidden conversations" onKeyDown={moveFocus}>
              {hidden.map((c) => (
                <Row key={c.id} conversation={c} selected={false} hidden onOpen={() => { open(c.id); }} onHide={() => void hide(c, 'open')} />
              ))}
            </ul>
          )}
          <label className="dm-preference">
            Who can message me
            <select
              value={accept ?? ''}
              disabled={accept === null}
              onChange={(event) => void choose(event.target.value as DMAcceptPolicy)}
            >
              {accept === null ? <option value="">Loading…</option> : null}
              {policies.map((p) => <option key={p.value} value={p.value}>{p.label}</option>)}
            </select>
          </label>
          {preference === '' ? null : <p role="alert" className="hint">{preference}</p>}
        </>
      ) : null}
    </nav>
  );
}
