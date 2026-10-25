import { useEffect, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { Avatar } from './Icons';
import { memberName } from './guestName';
import type { Entry } from './agents/join';
import type { ChannelMemberV1, ChannelV0, PutChannelMemberRequestV1 } from './schema/channel';
import type { WhoAmIResponseV1 } from './schema/principal';

const addRefusal = 'No one by that name can be added here.';
const networkRefusal = 'Cannot reach the server. Try again.';
const changeRefusal = 'The server refused the change.';

export interface MemberControlsProps {
  readonly channel: ChannelV0;
  readonly channels: readonly ChannelV0[];
  readonly members: readonly ChannelMemberV1[];
  readonly viewer: Pick<WhoAmIResponseV1, 'role'> & { readonly guest: boolean };
  readonly refreshMembers: () => void;
  readonly onUnauthorized: () => void;
}

function displayName(member: ChannelMemberV1): string {
  return memberName(member) ?? `#${String(member.principal_id)}`;
}

function refusalLine(result: api.InviteRefusal): string {
  if (result.failure === 'network') return networkRefusal;
  return result.message === undefined || result.message === '' ? changeRefusal : result.message;
}

function AddPeople(props: MemberControlsProps) {
  const { channel, channels, members, viewer, refreshMembers, onUnauthorized } = props;
  const [name, setName] = useState('');
  const [adding, setAdding] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [candidates, setCandidates] = useState<readonly ChannelMemberV1[]>([]);
  const [selected, setSelected] = useState('');
  const request = useRef(0);

  useEffect(() => {
    request.current++;
    setName('');
    setAdding(false);
    setLine(null);
    setPickerOpen(false);
    setLoading(false);
    setCandidates([]);
    setSelected('');
  }, [channel.id]);

  async function addByName(event: SyntheticEvent) {
    event.preventDefault();
    if (adding || name.trim() === '') return;
    setAdding(true);
    setLine(null);
    const result = await api.addChannelMember(channel.name, name);
    setAdding(false);
    if (result.ok) {
      setName('');
      refreshMembers();
      return;
    }
    if (result.failure === 'unauthorized') {
      onUnauthorized();
      return;
    }
    if (result.failure === 'invalid') refreshMembers();
    setLine(result.failure === 'network' ? networkRefusal : addRefusal);
  }

  async function openPicker() {
    const opening = !pickerOpen;
    setPickerOpen(opening);
    setLine(null);
    setSelected('');
    if (!opening) return;
    const mine = ++request.current;
    setLoading(true);
    const other = channels.filter((c) => c.id !== channel.id);
    const results = await Promise.all(other.map((c) => api.listMembers(c.name)));
    if (mine !== request.current) return;
    setLoading(false);
    if (results.some((r) => !r.ok && r.failure === 'unauthorized')) {
      onUnauthorized();
      return;
    }
    if (results.some((r) => !r.ok)) {
      setLine('Could not read people from the other channels.');
      return;
    }
    const here = new Set(members.map((m) => m.principal_id));
    const found = new Map<number, ChannelMemberV1>();
    for (const result of results) {
      if (!result.ok) continue;
      for (const member of result.value) {
        const addable = viewer.role === 'operator' || (member.kind === 'human' && member.guest === undefined);
        if (addable && !here.has(member.principal_id) && !found.has(member.principal_id)) found.set(member.principal_id, member);
      }
    }
    setCandidates([...found.values()]);
  }

  async function addSelected(event: SyntheticEvent) {
    event.preventDefault();
    const id = Number(selected);
    if (adding || !Number.isInteger(id) || id <= 0) return;
    setAdding(true);
    setLine(null);
    const result = await api.putChannelMember(channel.name, id, {});
    setAdding(false);
    if (result.ok) {
      setCandidates((held) => held.filter((m) => m.principal_id !== id));
      setSelected('');
      refreshMembers();
      return;
    }
    if (result.failure === 'unauthorized') {
      onUnauthorized();
      return;
    }
    if (result.failure === 'invalid') refreshMembers();
    setLine(refusalLine(result));
  }

  return (
    <section aria-label="Add people" className="member-admin">
      <form aria-label="Add person by name" onSubmit={(event) => void addByName(event)}>
        <label htmlFor={`add-member-name-${String(channel.id)}`}>Exact name</label>
        <input
          id={`add-member-name-${String(channel.id)}`}
          type="text"
          autoComplete="off"
          value={name}
          readOnly={adding}
          onChange={(event) => {
            setName(event.target.value);
          }}
        />
        <button type="submit" disabled={adding || name.trim() === ''}>
          Add by name
        </button>
      </form>
      <button type="button" aria-expanded={pickerOpen} onClick={() => void openPicker()}>
        Add from another channel
      </button>
      {pickerOpen ? (
        <form aria-label="Add person from another channel" onSubmit={(event) => void addSelected(event)}>
          <label htmlFor={`add-member-shared-${String(channel.id)}`}>Person from another channel</label>
          <select
            id={`add-member-shared-${String(channel.id)}`}
            value={selected}
            disabled={loading || adding}
            onChange={(event) => {
              setSelected(event.target.value);
            }}
          >
            <option value="">Choose a person</option>
            {candidates.map((member) => (
              <option key={member.principal_id} value={String(member.principal_id)}>
                {displayName(member)}
              </option>
            ))}
          </select>
          <button type="submit" disabled={loading || adding || selected === ''}>
            Add selected person
          </button>
          {!loading && candidates.length === 0 ? <p className="hint">No one in your other channels can be added here.</p> : null}
        </form>
      ) : null}
      {line === null ? null : <p role="status">{line}</p>}
    </section>
  );
}

export function MemberControls(props: MemberControlsProps & { readonly principalId: number }) {
  const { channel, members, viewer, refreshMembers, onUnauthorized, principalId } = props;
  const member = members.find((m) => m.principal_id === principalId);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  if (member === undefined) return null;

  const operator = viewer.role === 'operator';
  const manager = !operator && !viewer.guest && channel.manages === true;
  const name = displayName(member);
  const human = member.kind === 'human' && member.guest === undefined;

  async function run(request: PutChannelMemberRequestV1 | null) {
    if (busy || member === undefined) return;
    setBusy(true);
    setLine(null);
    const result = request === null ? await api.removeChannelMember(channel.name, member.principal_id) : await api.putChannelMember(channel.name, member.principal_id, request);
    setBusy(false);
    if (result.ok) {
      refreshMembers();
      return;
    }
    if (result.failure === 'unauthorized') {
      onUnauthorized();
      return;
    }
    if (result.failure === 'invalid') refreshMembers();
    setLine(refusalLine(result));
  }

  let explanation: string | null = null;
  let removable = operator;
  if (manager) {
    removable = member.role === 'member' && member.kind === 'human' && member.can_decide === false;
    if (!removable) {
      if (member.kind === 'agent') explanation = 'Agents are managed by operators.';
      else if (member.role === 'operator') explanation = 'Operators can only be removed by an operator.';
      else if (member.can_decide === true) explanation = 'Approvers are removed by an operator.';
    }
  }

  return (
    <>
      {member.manages === true ? <span className="ag-badge">Manager</span> : null}
      {member.can_decide === true ? <span className="ag-badge">Approver</span> : null}
      {operator || manager ? (
        <span className="member-controls">
          {removable ? (
            <button type="button" disabled={busy} onClick={() => void run(null)}>
              Remove {name}
            </button>
          ) : null}
          {operator && human ? (
            <>
              <button type="button" disabled={busy} onClick={() => void run({ manages: member.manages !== true })}>
                {member.manages === true ? `Remove ${name} as manager` : `Make ${name} a manager`}
              </button>
              <label>
                <input
                  type="checkbox"
                  checked={member.can_decide === true}
                  disabled={busy}
                  onChange={(event) => void run({ can_decide: event.target.checked })}
                />
                {name} decides approvals
              </label>
            </>
          ) : null}
          {explanation === null ? null : <span className="hint">{explanation}</span>}
          {line === null ? null : <span role="status">{line}</span>}
        </span>
      ) : null}
    </>
  );
}

function PeopleGroup({ title, people, controls }: { readonly title: string; readonly people: readonly Entry[]; readonly controls: MemberControlsProps }) {
  if (people.length === 0) return null;
  return (
    <>
      <h4>{`${title} — ${String(people.length)}`}</h4>
      <ul aria-label={`${title} — ${String(people.length)}`}>
        {people.map((person) => (
          <li key={person.id} className={`ag-person state-${person.state}`}>
            <Avatar id={person.id} name={person.name} />
            <span className="ag-name">{person.name}</span>
            {person.live > 1 ? <span> ({person.live} sessions)</span> : null} <span className="ag-status">{person.state}</span>
            <MemberControls key={`${String(controls.channel.id)}:${String(person.id)}`} {...controls} principalId={person.id} />
          </li>
        ))}
      </ul>
    </>
  );
}

export function Members({ people, ...controls }: MemberControlsProps & { readonly people: readonly Entry[] }) {
  const operator = controls.viewer.role === 'operator';
  const manager = !operator && !controls.viewer.guest && controls.channel.manages === true;
  const canAdd = operator || manager;
  const online = people.filter((p) => p.state !== 'offline');
  const offline = people.filter((p) => p.state === 'offline');
  return (
    <>
      <h3>People</h3>
      {canAdd ? <AddPeople key={controls.channel.id} {...controls} /> : null}
      {!canAdd ? <p className="hint">Only operators and this channel&apos;s managers add people.</p> : null}
      {controls.viewer.guest ? <p className="hint">A guest visit cannot do this; create an account and ask an operator.</p> : null}
      {!operator ? (
        <>
          <p className="hint">Only operators choose who manages a channel.</p>
          <p className="hint">Only operators choose who decides approvals.</p>
        </>
      ) : null}
      {people.length === 0 ? (
        <p className="empty">Nobody</p>
      ) : (
        <>
          <PeopleGroup title="Online" people={online} controls={controls} />
          <PeopleGroup title="Offline" people={offline} controls={controls} />
        </>
      )}
    </>
  );
}
