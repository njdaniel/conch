import { useState } from 'react';
import type { IdentityInput } from '../identityMarks';
import { useDm } from './DmContext';
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
  readonly onOpened?: () => void;
}) {
  const dm = useDm();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState('');
  if (dm === null || dm.inbox.state.status !== 'ready' || !dm.inbox.state.enabled || !canMessage(person, id, viewer ?? dm.me)) return null;
  const { open } = dm.inbox;
  async function start() {
    setBusy(true);
    setProblem('');
    const result = await open(id);
    setBusy(false);
    if (result.ok) {
      onOpened?.();
      return;
    }
    setProblem(result.failure === 'unavailable' ? cannotMessage : result.failure === 'not_found' ? 'This person cannot be found.' : 'Could not open the conversation. Try again.');
  }
  return (
    <span className="message-action">
      <button type="button" aria-label={`Message ${name}`} disabled={busy} onClick={() => void start()}>
        Message
      </button>
      {problem === '' ? null : <span role="alert" className="hint">{problem}</span>}
    </span>
  );
}
