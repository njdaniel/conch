import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { addGroup, deleteGroup, groupNameProblem, maxGroupName, maxGroups, renameGroup, type Arrangement, type Group } from './arrangement';

// The controls of the channel arrangement (issue #1493) that are not a
// channel's own menu items: the "Edit groups" panel and "Move to group…".
// Everything here changes how the person's own list is drawn and nothing else.

export interface ArrangementControls {
  readonly arrangement: Arrangement;
  readonly edit: (change: (a: Arrangement) => Arrangement) => void;
}

function escape(event: KeyboardEvent, close: () => void) {
  if (event.key !== 'Escape' || event.nativeEvent.isComposing) return;
  event.preventDefault();
  event.stopPropagation();
  close();
}

// Create, rename and delete groups. Deleting a group sends its channels back
// to the list; no channel is touched.
export function GroupEditor({ controls, onClose, onNote }: { readonly controls: ArrangementControls; readonly onClose: () => void; readonly onNote: (text: string) => void }) {
  const { arrangement, edit } = controls;
  const [name, setName] = useState('');
  const [problem, setProblem] = useState<string | null>(null);
  const field = useRef<HTMLInputElement>(null);
  useEffect(() => {
    field.current?.focus();
  }, []);
  const full = arrangement.groups.length >= maxGroups;

  function add() {
    const refused = full ? `At most ${String(maxGroups)} groups.` : groupNameProblem(arrangement, name);
    setProblem(refused);
    if (refused !== null) return;
    edit((a) => addGroup(a, name));
    onNote(`Group ${name.trim()} added`);
    setName('');
  }

  return (
    <div className="group-editor" role="group" aria-label="Edit groups" onKeyDown={(event) => { escape(event, onClose); }}>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          add();
        }}
      >
        <label>
          New group name
          <input ref={field} type="text" value={name} maxLength={maxGroupName} autoComplete="off" onChange={(event) => { setName(event.target.value); setProblem(null); }} />
        </label>
        <button type="submit">Add group</button>
      </form>
      {problem === null ? null : <p className="notice" role="alert">{problem}</p>}
      {arrangement.groups.length === 0 ? <p className="hint">No groups yet. A group collects channels under a heading you can fold away.</p> : null}
      <ul>
        {arrangement.groups.map((group) => (
          <GroupRow key={group.id} group={group} arrangement={arrangement} edit={edit} onNote={onNote} />
        ))}
      </ul>
      <button type="button" onClick={onClose}>Done</button>
    </div>
  );
}

function GroupRow({ group, arrangement, edit, onNote }: { readonly group: Group; readonly arrangement: Arrangement; readonly edit: ArrangementControls['edit']; readonly onNote: (text: string) => void }) {
  const [name, setName] = useState(group.name);
  const [problem, setProblem] = useState<string | null>(null);
  const id = useId();
  useEffect(() => {
    setName(group.name);
  }, [group.name]);
  return (
    <li>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          const refused = groupNameProblem(arrangement, name, group.id);
          setProblem(refused);
          if (refused !== null) return;
          edit((a) => renameGroup(a, group.id, name));
          onNote(`Group renamed to ${name.trim()}`);
        }}
      >
        <input aria-label={`Name of group ${group.name}`} aria-describedby={problem === null ? undefined : `${id}-problem`} type="text" value={name} maxLength={maxGroupName} autoComplete="off" onChange={(event) => { setName(event.target.value); setProblem(null); }} />
        <button type="submit">Rename</button>
        <button
          type="button"
          aria-label={`Delete group ${group.name}`}
          onClick={() => {
            edit((a) => deleteGroup(a, group.id));
            onNote(`Group ${group.name} deleted; its channels are back in the list`);
          }}
        >
          Delete
        </button>
      </form>
      {problem === null ? null : <p id={`${id}-problem`} className="notice" role="alert">{problem}</p>}
    </li>
  );
}

// "Move to group…": a choice of group, or none, in place under the row.
export function MoveToGroup({ channel, favorite, groups, current, onMove, onClose }: { readonly channel: string; readonly favorite: boolean; readonly groups: readonly Group[]; readonly current: string | null; readonly onMove: (groupId: string | null) => void; readonly onClose: () => void }) {
  const [choice, setChoice] = useState(current ?? '');
  const select = useRef<HTMLSelectElement>(null);
  useEffect(() => {
    select.current?.focus();
  }, []);
  return (
    <form
      className="move-to-group"
      aria-label={`Move ${channel} to a group`}
      onKeyDown={(event) => { escape(event, onClose); }}
      onSubmit={(event) => {
        event.preventDefault();
        onMove(choice === '' ? null : choice);
      }}
    >
      <label>
        Group
        <select ref={select} value={choice} onChange={(event) => { setChoice(event.target.value); }}>
          <option value="">No group</option>
          {groups.map((g) => (
            <option key={g.id} value={g.id}>{g.name}</option>
          ))}
        </select>
      </label>
      {favorite ? <p className="hint">It stays under Favorites while it is a favorite.</p> : null}
      <div className="channel-settings-buttons">
        <button type="submit">Move</button>
        <button type="button" onClick={onClose}>Cancel</button>
      </div>
    </form>
  );
}
