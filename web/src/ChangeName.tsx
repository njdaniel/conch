import { useState, type SyntheticEvent } from 'react';
import * as api from './api';
import type { PrincipalV0 } from './schema/principal';

// What the server's refusal means to the person, by its code. A name is
// unique by how it reads: one that differs from another only by case or by
// look-alike letters is taken (issue #441).
const refusals: Record<string, string> = {
  principal_exists: 'That name is taken, or looks too much like someone else’s',
  rate_limited: 'You have changed or tried to change your name too often today. Try again tomorrow',
  invalid_request: 'That name cannot be used: it needs a letter you can see, and no spaces at either end or control characters',
  human_required: 'An agent’s name is set by an operator',
};

type Line = { readonly text: string; readonly refused: boolean };

// "Change your name": a person renames themselves, one request per submit.
// The new name is the server's answer, handed to onRenamed; nothing changes
// on the page until the server has said yes.
export function ChangeName({
  id,
  name,
  onRenamed,
  onUnauthorized,
}: {
  readonly id: number;
  readonly name: string;
  readonly onRenamed: (principal: PrincipalV0) => void;
  readonly onUnauthorized: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(name);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    if (busy) return;
    if (value === name) {
      setLine({ text: 'That is already your name', refused: true });
      return;
    }
    setBusy(true);
    setLine(null);
    void api.renamePrincipal(id, value).then((result) => {
      setBusy(false);
      if (result.ok) {
        setLine({ text: `Your name is now ${result.value.name}`, refused: false });
        onRenamed(result.value);
        return;
      }
      switch (result.failure) {
        case 'unauthorized':
          onUnauthorized();
          return;
        case 'network':
          setLine({ text: 'Cannot reach the server', refused: true });
          return;
        case 'invalid':
          setLine({ text: (result.code === undefined ? undefined : refusals[result.code]) ?? 'The server refused the change', refused: true });
      }
    });
  }

  return (
    <div className="change-name">
      <button
        type="button"
        aria-expanded={open}
        aria-controls="change-name-form"
        onClick={() => {
          setOpen(!open);
          setValue(name);
          setLine(null);
        }}
      >
        Change your name
      </button>
      <form id="change-name-form" aria-label="Change your name" hidden={!open} onSubmit={submit}>
        <label htmlFor="change-name-field">New name</label>
        <input
          id="change-name-field"
          type="text"
          autoComplete="off"
          value={value}
          disabled={busy}
          onChange={(e) => {
            setValue(e.target.value);
          }}
        />
        <button type="submit" disabled={busy || value.trim() === ''}>
          Save name
        </button>
      </form>
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </div>
  );
}
