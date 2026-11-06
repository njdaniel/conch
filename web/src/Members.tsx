import { ModerationConfirm, type ModerationOperation } from './Moderation';
import type { ChannelBanV1 } from './schema/generated';
import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode, type SyntheticEvent } from 'react';
import * as api from './api';
import { Avatar } from './Icons';
import { memberName } from './guestName';
import { ActionsMenu, useOpeners, type MenuItem } from './MessageMenu';
import type { Entry } from './agents/join';
import { PersonActions } from './admin/PersonActions';
import { memberActions } from './memberActions';
import type { PermissionRowV1 } from './schema/permission';
import type { ChannelMemberV1, ChannelV0, PutChannelMemberRequestV1 } from './schema/channel';
import type { ServerMemberV1, ServerMembersResponseV1, WhoAmIResponseV1 } from './schema/principal';

const addRefusal = 'No one by that name can be added here.';
const networkRefusal = 'Cannot reach the server. Try again.';
const changeRefusal = 'The server refused the change.';

export interface MemberControlsProps {
  readonly channel: ChannelV0;
  readonly channels: readonly ChannelV0[];
  readonly members: readonly ChannelMemberV1[];
  // id is the viewer's own: their own row offers no action on themself.
  readonly viewer: Pick<WhoAmIResponseV1, 'role' | 'owner' | 'permissions'> & { readonly guest: boolean; readonly id?: number };
  readonly refreshMembers: () => void;
  readonly onModerated?: (line: string) => void;
  // Counts the server's words that the directory changed (issue #993): an
  // open picker reads the other channels again and replaces its candidates.
  readonly directoryTick?: number;
  readonly onUnauthorized: () => void;
  // Set by Members (issue #1450): the open menu and pending question per
  // person, kept above the Online and Offline lists.
  readonly rowUi?: (principalId: number) => RowUi;
  readonly setRowUi?: (principalId: number, patch: Partial<RowUi>) => void;
}

function displayName(member: ChannelMemberV1): string {
  return memberName(member) ?? `#${String(member.principal_id)}`;
}

function refusalLine(result: api.InviteRefusal): string {
  if (result.failure === 'network') return networkRefusal;
  return result.message === undefined || result.message === '' ? changeRefusal : result.message;
}

function AddPeople(props: MemberControlsProps) {
  const { channel, channels, members, viewer, refreshMembers, directoryTick = 0, onUnauthorized } = props;
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

  // Reads the other channels' people and replaces the candidates. An answer to
  // an earlier read, or to another channel, is dropped.
  async function loadCandidates() {
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
    // A chosen person the new list lacks is no longer chosen.
    setSelected((held) => (held !== '' && found.has(Number(held)) ? held : ''));
  }

  async function openPicker() {
    const opening = !pickerOpen;
    setPickerOpen(opening);
    setLine(null);
    setSelected('');
    if (!opening) return;
    await loadCandidates();
  }

  // The directory changed (issue #993): an open picker is read again and its
  // candidates replaced, so a renamed or hidden person is not offered stale.
  const loadAgain = useRef(loadCandidates);
  loadAgain.current = loadCandidates;
  const seenTick = useRef(directoryTick);
  useEffect(() => {
    if (seenTick.current === directoryTick) return;
    seenTick.current = directoryTick;
    if (pickerOpen) void loadAgain.current();
  }, [directoryTick, pickerOpen]);

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

// MemberRow is one person's row in the members panel (issue #1094): the
// caller's content, the Manager and Approver badges, and one ⋯ menu with the
// actions this viewer may use on this person. The menu is the message log's
// (MessageMenu.tsx), opened by its button, Enter or Space on it, a right-click
// on the row or a long press. A row with no actions draws no button and
// suppresses no native menu. A refusal after an action stays in the row.
export interface RowUi {
  readonly menuOpen: boolean;
  readonly confirming: boolean;
  readonly moderating: ModerationOperation | null;
}
const noUi: RowUi = { menuOpen: false, confirming: false, moderating: null };

export function MemberRow({
  className,
  principalId,
  children,
  after,
  ...controls
}: MemberControlsProps & { readonly principalId: number; readonly className: string; readonly children: ReactNode; readonly after?: ReactNode }) {
  const { channel, members, viewer, refreshMembers, onUnauthorized } = controls;
  const member = members.find((m) => m.principal_id === principalId);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  const [localUi, setLocalUi] = useState<RowUi>(noUi);
  // The open menu and a pending question live above the Online and Offline
  // lists when Members hosts the row (issue #1450), so a person changing
  // presence, which remounts their row, does not close them.
  const ui = controls.rowUi?.(principalId) ?? localUi;
  const setUi = (patch: Partial<RowUi>) => {
    if (controls.setRowUi === undefined) setLocalUi((cur) => ({ ...cur, ...patch }));
    else controls.setRowUi(principalId, patch);
  };
  const menuOpen = ui.menuOpen;
  const setMenuOpen = (v: boolean) => { setUi({ menuOpen: v }); };
  const moderating = ui.moderating;
  const setModerating = (v: ModerationOperation | null) => { setUi({ moderating: v }); };
  const confirming = ui.confirming;
  const setConfirming = (v: boolean) => { setUi({ confirming: v }); };
  const more = useRef<HTMLButtonElement>(null);
  // Removal asks first (issue #1083): "Remove from channel" opens this
  // question in the row, with focus on Cancel; Escape and Cancel put focus
  // back on the row's ⋯ button.
  const cancel = useRef<HTMLButtonElement>(null);
  const questionId = useId();
  useEffect(() => {
    if (confirming) cancel.current?.focus();
  }, [confirming]);

  const operator = !viewer.guest && ((viewer.role === 'operator' && viewer.owner === true) || (viewer.permissions === undefined ? viewer.role === 'operator' : viewer.permissions.includes('manage_channels')));
  const manager = !operator && !viewer.guest && channel.manages === true;
  const human = member !== undefined && member.kind === 'human' && member.guest === undefined;

  async function run(request: PutChannelMemberRequestV1 | null) {
    if (busy || member === undefined) return;
    setConfirming(false);
    setBusy(true);
    setLine(null);
    const result = request === null ? await api.removeChannelMember(channel.name, member.principal_id) : await api.putChannelMember(channel.name, member.principal_id, request);
    setBusy(false);
    if (result.ok) {
      if (request === null) {
        setLine('Membership removed or already absent.');
        controls.onModerated?.('Membership removed or already absent.');
      }
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

  // The same rules as before the menu: an operator may remove anyone and, for
  // a human, change the two standings; a manager removes only a plain human
  // member who does not decide approvals. The server still decides.
  const items: MenuItem[] = [];
  if (member !== undefined && (operator || manager) && member.principal_id !== viewer.id) {
    const removable = operator || ((member.role === 'member' || member.guest !== undefined) && member.kind === 'human' && member.can_decide === false);
    if (member.kind === 'human') items.push({ key: 'voice', label: 'Remove from voice', disabled: busy, onSelect: () => { setModerating('kickVoice'); } });
    if (removable) {
      items.push({ key: 'ban', label: 'Ban from channel', disabled: busy, onSelect: () => { setModerating('banMember'); } });
      items.push({
        key: 'remove',
        label: 'Remove from channel',
        disabled: busy,
        onSelect: () => {
          setLine(null);
          setConfirming(true);
        },
      });
    }
    if (operator && human) {
      items.push({
        key: 'manager',
        label: member.manages === true ? 'Remove as manager' : 'Make manager',
        disabled: busy,
        onSelect: () => void run({ manages: member.manages !== true }),
      });
      items.push({
        key: 'decide',
        label: member.can_decide === true ? 'Stop deciding approvals' : 'Let decide approvals',
        disabled: busy,
        onSelect: () => void run({ can_decide: member.can_decide !== true }),
      });
    }
  }
  const openers = useOpeners(items.length > 0, () => {
    setMenuOpen(true);
  });

  function leave(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key !== 'Escape' || event.nativeEvent.isComposing) return;
    event.preventDefault();
    event.stopPropagation();
    setConfirming(false);
    more.current?.focus();
  }

  return (
    <li className={`${className} member-row${items.length > 0 ? ' has-actions' : ''}`} {...openers}>
      {children}
      {member?.manages === true ? <span className="ag-badge">Manager</span> : null}
      {member?.can_decide === true ? <span className="ag-badge">Approver</span> : null}
      {after}
      {items.length === 0 || member === undefined ? null : (
        <ActionsMenu label={`Actions for ${displayName(member)}`} items={items} open={menuOpen} onOpenChange={setMenuOpen} button={more} />
      )}
      {confirming && (operator || manager) && member !== undefined ? (
        <div role="group" aria-labelledby={questionId} className="member-confirm" onKeyDown={leave}>
          <p id={questionId}>{`Remove ${displayName(member)} from #${channel.name} and its nets? They lose this channel and its history. This does not ban them or revoke their account.`}</p>
          <div className="actions">
            <button
              type="button"
              disabled={busy}
              onClick={() => {
                more.current?.focus();
                void run(null);
              }}
            >
              Yes, remove
            </button>
            <button
              ref={cancel}
              type="button"
              onClick={() => {
                setConfirming(false);
                more.current?.focus();
              }}
            >
              Cancel
            </button>
          </div>
        </div>
      ) : null}
      {moderating === null || member === undefined || (!operator && !manager) ? null : <ModerationConfirm op={moderating} id={member.principal_id} name={displayName(member)} channel={channel.name} onUnauthorized={onUnauthorized} onDone={() => { controls.onModerated?.(moderating === 'banMember' ? 'Ban is in place, including if already banned.' : 'Removed from voice or already absent. They can rejoin.'); refreshMembers(); }} onClose={() => { setModerating(null); more.current?.focus(); }} />}
      {line === null ? null : <span role="status" className="member-line">{line}</span>}
    </li>
  );
}

function PeopleGroup({ title, people, controls }: { readonly title: string; readonly people: readonly Entry[]; readonly controls: MemberControlsProps }) {
  if (people.length === 0) return null;
  return (
    <>
      <h4>{`${title} — ${String(people.length)}`}</h4>
      <ul aria-label={`${title} — ${String(people.length)}`}>
        {people.map((person) => (
          <MemberRow key={`${String(controls.channel.id)}:${String(person.id)}`} {...controls} principalId={person.id} className={`ag-person state-${person.state}`}>
            <Avatar id={person.id} name={person.name} />
            <span className="ag-name">{person.name}</span>
            {person.live > 1 ? <span> ({person.live} sessions)</span> : null} <span className="ag-status">{person.state}</span>
          </MemberRow>
        ))}
      </ul>
    </>
  );
}

export function Members({ people, ...controls }: MemberControlsProps & { readonly people: readonly Entry[] }) {
  const [feedback, setFeedback] = useState<string | null>(null);
  const [rowUis, setRowUis] = useState<{ readonly channel: number; readonly byId: Readonly<Record<number, RowUi>> }>({ channel: controls.channel.id, byId: {} });
  const byId = rowUis.channel === controls.channel.id ? rowUis.byId : {};
  const rowUi = (id: number): RowUi => byId[id] ?? noUi;
  const setRowUi = (id: number, patch: Partial<RowUi>) => {
    setRowUis((cur) => {
      const base = cur.channel === controls.channel.id ? cur.byId : {};
      return { channel: controls.channel.id, byId: { ...base, [id]: { ...(base[id] ?? noUi), ...patch } } };
    });
  };
  const operator = !controls.viewer.guest && ((controls.viewer.role === 'operator' && controls.viewer.owner === true) || (controls.viewer.permissions === undefined ? controls.viewer.role === 'operator' : controls.viewer.permissions.includes('manage_channels')));
  const manager = !operator && !controls.viewer.guest && controls.channel.manages === true;
  const canAdd = operator || manager;
  const online = people.filter((p) => p.state !== 'offline');
  const offline = people.filter((p) => p.state === 'offline');
  return (
    <>
      <h3>People</h3>
      {feedback === null ? null : <p role="status">{feedback}</p>}
      {canAdd ? <BanList key={`bans-${String(controls.channel.id)}`} {...controls} operator={operator} /> : null}
      {canAdd ? <AddPeople key={controls.channel.id} {...controls} /> : null}
      {controls.viewer.guest ? <p className="hint">A guest visit cannot do this; create an account and ask an operator.</p> : null}
      {/* One line at most (issue #1020): what this viewer cannot do, in one. */}
      {!canAdd && !controls.viewer.guest ? <p className="hint">Only operators and this channel&apos;s managers add people.</p> : null}
      {canAdd && !operator && !controls.viewer.guest ? <p className="hint">Only operators choose who manages a channel or decides approvals.</p> : null}
      {people.length === 0 ? (
        <p className="empty">Nobody</p>
      ) : (
        <>
          <PeopleGroup title="Online" people={online} controls={{ ...controls, onModerated: setFeedback, rowUi, setRowUi }} />
          <PeopleGroup title="Offline" people={offline} controls={{ ...controls, onModerated: setFeedback, rowUi, setRowUi }} />
        </>
      )}
    </>
  );
}

// What the community member panel says of one person, from the directory's
// row. An agent is marked as one (ADR-013 O11) and a human shows their role
// (O4). The operator fields (owner, voice, who may invite, who invited them)
// are drawn only when the row carries them: the server sends them to a holder
// of manage_members alone, so the page cannot show a member what it was not
// told. The controls that act on them are another issue's (#992).
// With `manage`, the row has the ⋯ menu of memberActions.ts (issue #1020): the
// actions this viewer may take on this person, opened by its button, a
// right-click on the row (the native menu is suppressed on a row with actions
// only) or a long press.
export interface RowManage {
  readonly who: WhoAmIResponseV1;
  readonly origin: string;
  readonly rows: readonly PermissionRowV1[] | null;
  // Reads the permissions document again, when a menu opens.
  readonly readRows: () => void;
  readonly onChanged: (done?: string) => void;
  readonly onUnauthorized: () => void;
}

function CommunityRow({
  person,
  byId,
  viewer,
  manage,
}: {
  readonly person: ServerMemberV1;
  readonly byId: ReadonlyMap<number, ServerMemberV1>;
  readonly viewer: number;
  readonly manage?: RowManage | undefined;
}) {
  const inviter = person.invited_by === undefined ? undefined : byId.get(person.invited_by);
  const [menuOpen, setMenuOpen] = useState(false);
  const more = useRef<HTMLButtonElement>(null);
  const has = manage !== undefined && memberActions(manage.who, person).length > 0;
  // The row is inside the Settings drawer, a dialog, when it is the Members section.
  const openers = useOpeners(
    has,
    () => {
      setMenuOpen(true);
    },
    // A press inside the question or form a menu item opened is not a new menu.
    { insideDialog: true, skip: '.person-panel' },
  );
  return (
    <li className={`community-member${has ? ' member-row has-actions' : ''}`} {...openers}>
      <Avatar id={person.id} name={person.name} />
      <span className="community-name">{person.name}</span>
      {person.id === viewer ? <span className="ag-badge">You</span> : null}
      {person.kind === 'agent' ? <span className="ag-badge">Agent</span> : null}
      {person.kind === 'human' && person.role !== undefined ? <span className="ag-badge">{person.role === 'operator' ? 'Operator' : 'Member'}</span> : null}
      {person.owner === true ? <span className="ag-badge">Owner</span> : null}
      {person.voice_allowed === undefined ? null : <span className="hint">{person.voice_allowed ? ' · voice on' : ' · voice off'}</span>}
      {person.may_invite === undefined ? null : <span className="hint">{person.may_invite ? ' · can invite' : ' · cannot invite'}</span>}
      {person.invited_by === undefined ? null : <span className="hint">{` · invited by ${inviter?.name ?? `#${String(person.invited_by)}`}`}</span>}
      {manage === undefined ? null : (
        <PersonActions
          who={manage.who}
          person={person}
          origin={manage.origin}
          rows={manage.rows}
          readRows={manage.readRows}
          open={menuOpen}
          onOpenChange={setMenuOpen}
          button={more}
          onChanged={manage.onChanged}
          onUnauthorized={manage.onUnauthorized}
        />
      )}
    </li>
  );
}

// The community member panel (issues #983 and #991): everyone the server lists
// from GET /v1/directory/members, beside the open channel's own people. It is
// read when it is drawn and again when the person's own channels or
// permissions change and when the server says the directory changed; whatever the server leaves out (a viewer who may only know the
// people they share a channel with is told so) is not guessed at. There are no
// guests in it: the directory lists none.
export function CommunityMembers({
  viewer,
  channelIds,
  permissionsTick = 0,
  directoryTick,
  who,
  origin = '',
  title = 'Community members',
  level = 3,
  showTitle = true,
  active = true,
  header,
  onUnauthorized,
}: {
  readonly viewer: number;
  // With whoami, each row has the ⋯ menu of the actions this viewer may take
  // on that person (issue #1020, memberActions.ts). Without it the list is
  // read-only. origin builds the links the menu makes.
  readonly who?: WhoAmIResponseV1;
  readonly origin?: string;
  // The heading: the console's Members section names itself and takes h4.
  readonly title?: string;
  readonly level?: 3 | 4;
  // False where the page already names the section (a phone's section page).
  readonly showTitle?: boolean;
  // False while the Settings drawer that holds the console is shut: nothing is read.
  readonly active?: boolean;
  readonly header?: ReactNode;
  readonly channelIds: string;
  // Counts live changes of the person's own permissions (conch.permissions_changed.v1):
  // what they may see here may have changed, so the list is read again.
  readonly permissionsTick?: number;
  // Counts the server's words that the directory changed
  // (conch.directory_changed.v1): the list is read again and replaced.
  readonly directoryTick?: number;
  readonly onUnauthorized: () => void;
}) {
  const headingId = useId();
  const [answer, setAnswer] = useState<ServerMembersResponseV1 | 'failed' | null>(null);
  // The permissions document's rows, for the voice line beside the voice
  // switch (design §5); every operator may read it.
  const [rows, setRows] = useState<readonly PermissionRowV1[] | null>(null);
  // What the last change said, at the list's level: the row may be gone.
  const [note, setNote] = useState('');
  const noteLine = useRef<HTMLParagraphElement>(null);
  const wantsFocus = useRef(false);
  const seq = useRef(0);
  const rowReads = useRef(0);
  const live = useRef(true);
  const latest = useRef(onUnauthorized);
  latest.current = onUnauthorized;
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);

  const load = useCallback(() => {
    const mine = ++seq.current;
    void api.directoryMembers().then((result) => {
      if (mine !== seq.current || !live.current) return;
      if (result.ok) {
        setAnswer(result.value);
        return;
      }
      if (result.failure === 'unauthorized') {
        latest.current();
        return;
      }
      setAnswer('failed');
    });
  }, []);
  // The permissions document is read when a person's menu opens, so a switch
  // that would do nothing says so (the voice line); not with the list.
  const readRows = useCallback(() => {
    const mine = ++rowReads.current;
    void api.permissions().then((doc) => {
      if (mine === rowReads.current && live.current) setRows(doc.ok ? doc.value.rows : null);
    });
  }, []);
  useEffect(() => {
    if (active) load();
  }, [load, channelIds, permissionsTick, directoryTick, active]);
  // After a change, if the row that had focus went with the list's next read,
  // focus would drop to the page: the status line takes it.
  useEffect(() => {
    if (!wantsFocus.current || answer === null) return;
    wantsFocus.current = false;
    if (document.activeElement === null || document.activeElement === document.body || !document.body.contains(document.activeElement)) noteLine.current?.focus();
  }, [answer]);

  const byId = new Map<number, ServerMemberV1>();
  if (answer !== null && answer !== 'failed') for (const m of answer.members) byId.set(m.id, m);
  return (
    <section {...(showTitle ? { 'aria-labelledby': headingId } : { 'aria-label': title })} className="community-members">
      {!showTitle ? null : level === 4 ? <h4 id={headingId}>{title}</h4> : <h3 id={headingId}>{title}</h3>}
      {header}
      <p ref={noteLine} role="status" className="hint" tabIndex={-1}>
        {note}
      </p>
      {answer === null ? (
        <p className="empty">Loading…</p>
      ) : answer === 'failed' ? (
        <p role="status" className="notice">
          Cannot load the community members.
        </p>
      ) : (
        <>
          {answer.scope === 'shared_channels' ? <p className="hint">You see the people you share a channel with</p> : null}
          {answer.members.length === 0 ? (
            <p className="empty">Nobody to show</p>
          ) : (
            <ul aria-label="People">
              {answer.members.map((person) => (
                <CommunityRow
                  key={person.id}
                  person={person}
                  byId={byId}
                  viewer={viewer}
                  manage={who === undefined ? undefined : { who, origin, rows, readRows, onChanged: (done) => {
                      setNote(done ?? '');
                      wantsFocus.current = true;
                      load();
                    }, onUnauthorized: () => {
                      latest.current();
                    } }}
                />
              ))}
            </ul>
          )}
        </>
      )}
      {/* A list that is not told of directory changes keeps a Refresh; the
          community panel and the admin console's follow the frame. */}
      {directoryTick !== undefined ? null : (
        <button type="button" className="quiet" onClick={load}>
          Refresh
        </button>
      )}
    </section>
  );
}

function BanList({ channel, viewer, operator, onUnauthorized }: MemberControlsProps & { readonly operator: boolean }) {
  const [open, setOpen] = useState(false);
  const [bans, setBans] = useState<readonly ChannelBanV1[]>([]);
  const [line, setLine] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [selected, setSelected] = useState<ChannelBanV1 | null>(null);
  const opener = useRef<HTMLButtonElement>(null);
  async function load() {
    setBusy(true); setLine(null);
    const result = await api.listChannelBans(channel.name);
    setBusy(false);
    if (result.ok) setBans(result.value.bans);
    else if (result.failure === 'unauthorized') onUnauthorized();
    else setLine(refusalLine(result));
  }
  return <section aria-label="Channel bans" className="member-admin">
    <button ref={opener} type="button" aria-expanded={open} onClick={() => { setOpen(!open); if (!open) void load(); }}>Channel bans</button>
    {!open ? null : <>
      <p className="hint">Bans apply to a principal in this channel. A new account or guest visit is not identified by a ban. Unban does not re-add anyone.</p>
      <button type="button" disabled={busy} onClick={() => void load()}>Refresh bans</button>
      {busy ? <p role="status">Loading bans…</p> : line !== null ? <p role="alert">{line}</p> : bans.length === 0 ? <p>No channel bans.</p> : <ul>{bans.map((ban) => <li key={ban.principal_id}>{ban.name} (#{ban.principal_id}){operator || (viewer.id !== undefined && ban.created_by === viewer.id) ? <button type="button" onClick={() => { setSelected(ban); }}>Unban {ban.name}</button> : <span>Only the banning manager or an operator can lift this ban.</span>}</li>)}</ul>}
      {selected === null ? null : <ModerationConfirm key={selected.principal_id} op="unbanMember" id={selected.principal_id} name={selected.name} channel={channel.name} onUnauthorized={onUnauthorized} onDone={() => void load()} onClose={() => { setSelected(null); opener.current?.focus(); }} />}
    </>}
  </section>;
}
