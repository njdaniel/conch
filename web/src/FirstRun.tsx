import { useContext } from 'react';
import { headingId } from './Conversation';
import { JoiningSettings } from './joiningSettings';

// What an operator in no channel sees (issue #1032): on a new server, the
// three things to do first. /v1/channels lists only the caller's channels, so
// the page cannot tell a new server from one where this operator is in none;
// the words are true of both. It stands where the conversation would, and is gone
// once a channel exists. Its heading takes focus after sign-in as the
// conversation's does.
export function FirstRun({ onMakeChannel }: { readonly onMakeChannel: () => void }) {
  const joining = useContext(JoiningSettings);
  return (
    <div className="first-run">
      <h2 id={headingId} tabIndex={-1}>
        You&apos;re not in any channels yet
      </h2>
      <p className="hint">Make a channel to start talking, or ask an operator to add you to one.</p>
      <ul className="first-run-actions">
        <li>
          <button type="button" onClick={onMakeChannel}>
            Make a channel
          </button>
        </li>
        <li>
          <button type="button" disabled aria-describedby="first-run-invite-why">
            Invite someone
          </button>{' '}
          <span id="first-run-invite-why" className="hint">
            Needs a channel first
          </span>
        </li>
        <li>
          <button type="button" disabled={joining === null} onClick={joining?.open}>
            Decide how people join
          </button>
        </li>
      </ul>
    </div>
  );
}
