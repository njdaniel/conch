import { useContext } from 'react';
import { headingId } from './Conversation';
import { JoiningSettings } from './joiningSettings';

// What an operator sees on a server with no channels (issue #1032): the three
// things to do first. It stands where the conversation would, and is gone
// once a channel exists. Its heading takes focus after sign-in as the
// conversation's does.
export function FirstRun({ onMakeChannel }: { readonly onMakeChannel: () => void }) {
  const joining = useContext(JoiningSettings);
  return (
    <div className="first-run">
      <h2 id={headingId} tabIndex={-1}>
        Welcome to your server
      </h2>
      <p className="hint">There are no channels yet. Three things to do first:</p>
      <ul className="first-run-actions">
        <li>
          <button type="button" onClick={onMakeChannel}>
            Make your first channel
          </button>
        </li>
        <li>
          <button type="button" disabled aria-describedby="first-run-invite-why">
            Invite someone
          </button>{' '}
          <span id="first-run-invite-why" className="hint">
            Make a channel first
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
