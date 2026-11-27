import { useContext, useState } from 'react';
import type { IdentityInput } from '../identityMarks';
import { DmOpened, useDm } from './DmContext';
import { cannotMessage } from './model';

// A person can be messaged from a card or a row when they are a person, with a
// name of their own (not a guest, not an agent), and not the viewer. The
// server decides again, and answers one generic refusal for anyone who cannot
// be reached.
export function canMessage(person: IdentityInput | undefined, id: number, viewer: number | undefined): boolean {
  if (person === undefined || person.kind !== 'human') return false;
  if (person.guest !== undefined && person.guest !== null) return false;
  return id > 0 && id !== viewer;
}

// MessageAction opens, or resumes, the conversation with one person. It is
// offered only while the inbox is ready and DMs are on for the viewer.
export function MessageAction({ id, name, person, viewer, onOpened }: {
  readonly id: number;
  readonly name: string;
  readonly person: IdentityInput | undefined;
  readonly viewer?: number | undefined;
  readonly onOpened?: () => HTMLElement | null;
}) {
  const dm = useDm();
  const showConversation = useContext(DmOpened);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState('');
  if (dm === null || dm.inbox.state.status !== 'ready' || !dm.inbox.state.enabled || !canMessage(person, id, viewer ?? dm.me)) return null;
  const { open } = dm.inbox;
  async function start(button: HTMLButtonElement) {
    const focusAtStart = document.activeElement;
    setBusy(true);
    setProblem('');
    const result = await open(id, () => {
      // A slow open can finish after the person has moved on to another
      // control or opened a modal. The inbox still gets the row.
      if (!button.isConnected || button.closest('[hidden], [inert]') !== null) return false;
      const focusNow = document.activeElement;
      if (focusNow !== document.body && focusNow !== focusAtStart && focusNow !== button) return false;
      const opener = onOpened?.() ?? button;
      if (showConversation !== null) showConversation(opener);
      else if (opener.closest('[hidden], [inert]') === null) opener.focus();
      return true;
    });
    setBusy(false);
    if (result.ok) return;
    const failure = result.failure;
    setProblem(failure === 'unavailable' ? cannotMessage
      : failure === 'not_found' ? 'This person cannot be found.'
      : failure === 'dms_off' ? 'Direct messages are off.'
      : failure === 'own_sign_in_required' ? 'Direct messages need your own sign-in.'
      : failure === 'unauthorized' ? 'Direct messages are no longer available to you.'
      : 'Could not open the conversation. Try again.');
  }
  return (
    <span className="message-action">
      <button type="button" aria-label={`Message ${name}`} aria-disabled={busy} onClick={(event) => {
        if (!busy) void start(event.currentTarget);
      }}>
        Message
      </button>
      {problem === '' ? null : <span role="alert" className="hint">{problem}</span>}
    </span>
  );
}
