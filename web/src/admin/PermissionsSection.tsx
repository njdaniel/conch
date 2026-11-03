// The console's Roles and permissions section (ADR-014 design §4, §6, §8.2,
// §10.2, §10.3, issue #1019): the two preset cards, the matrix of permissions
// by role, and the save path.
//
// Everything here is drawn from GET /v1/permissions: the rows and their order,
// the fixed cells, the columns the caller may edit (`editable`) and the
// presets. The page holds no copy of the preset table. Everything also keys on
// whoami (permissions.ts), and the server decides each request again; hiding
// or disabling is never enforcement.
//
// A write is a patch: only the cells that changed, with the revision just
// read. A stale answer reads the document again and says what changed, and
// never sends the patch a second time by itself. Every line about a refusal is
// the page's own, chosen by the server's code and the permission it names; the
// server's message is never shown (it is only searched for the name of a
// permission the page already knows).
import { useCallback, useContext, useEffect, useId, useMemo, useRef, useState } from 'react';
import * as api from '../api';
import { SettingsOpen } from '../joiningSettings';
import { usePhone } from '../phone';
import type { IssuedByOperatorV1, KnownPermission, KnownPermissionPreset, Permission, PermissionChangeV1, PermissionPreset, PermissionRowV1, PermissionsResponseV1, PermissionsWarning, PutPermissionsRequestV1 } from '../schema/permission';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { canDo, reasonFor, refusalLine } from './permissions';
import { presetChanges } from './ServerSection';

type Role = 'member' | 'operator';
const roles: readonly Role[] = ['member', 'operator'];
const roleLabels: Readonly<Record<Role, string>> = { member: 'Members', operator: 'Operators' };

// The sentences the issue and the design fix, word for word.
export const actAsMembersSentence = "can sign in as, and post as, any member who isn't an owner";
export const auditSentence = "sees every member and every private channel's name in the audit trail";
export const actAsMembersNotEnded = 'Taking this away does not end credentials and hooks already issued; the answer lists them.';
export const ownersLine = 'Owners hold every permission.';
export const managersBlock = 'In their channels: add and remove members, rename, stop a share, moderate. Set per channel in Channels.';

interface RowInfo {
  readonly label: string;
  readonly text: string;
}

// The page's words for each permission it knows (design §4.1, shortened), in
// catalog order. A permission not here is shown by its name and is read-only.
const info: Readonly<Record<KnownPermission, RowInfo>> = {
  create_public_channels: { label: 'Create public channels', text: 'Create a public channel, and manage it.' },
  create_private_channels: { label: 'Create private channels', text: 'Create a private channel, and manage it.' },
  make_member_links: { label: 'Make member links', text: 'Make links that bring people onto the server.' },
  see_member_directory: { label: 'See the member directory', text: 'See everyone on the server, not just the people in shared channels.' },
  use_voice: { label: 'Use voice', text: 'Join voice in the channels they are in.' },
  share_screen: { label: 'Share a screen', text: 'Share a screen in voice. Needs voice.' },
  manage_members: { label: 'Manage members', text: 'Create, rename, disable and remove people and agents; list and revoke their credentials and hooks; run invites and the brakes.' },
  act_as_members: { label: 'Act as members', text: `Holders ${actAsMembersSentence}. Needs manage members.` },
  manage_channels: { label: 'Manage channels', text: 'Administer every channel, and see the names of channels they are not in.' },
  change_server_settings: { label: 'Change server settings', text: 'Change joining, open sign-up, the default link expiry and what members may do.' },
  read_audit_log: { label: 'Read the audit log', text: `Holders ${auditSentence}.` },
  use_camera: { label: 'Use a camera', text: 'Turn on a camera in voice. Needs voice.' },
};

function known(permission: string): permission is KnownPermission {
  return Object.hasOwn(info, permission);
}

// labelOf is a permission's name on the page, or the value itself when the
// page has no words for it (a permission a newer server added).
export function labelOf(permission: string): string {
  return known(permission) ? info[permission].label : permission;
}

function cellOf(row: PermissionRowV1, role: Role): boolean {
  return role === 'member' ? row.member : row.operator;
}

function fixedOf(row: PermissionRowV1, role: Role): boolean {
  return (role === 'member' ? row.member_fixed : row.operator_fixed) === true;
}

const keyOf = (permission: string, role: Role) => `${permission}:${role}`;

const presetNames: Readonly<Record<KnownPermissionPreset, string>> = { personal: 'Personal server', group: 'Group server' };
const presetShort: Readonly<Record<KnownPermissionPreset, string>> = { personal: 'Personal', group: 'Group' };

function knownPreset(preset: string): preset is KnownPermissionPreset {
  return Object.hasOwn(presetNames, preset);
}

// presetLabel is a preset's words from names, or the value itself when the
// page has none for it (a preset a newer server added, issue #1051).
function presetLabel(names: Readonly<Record<KnownPermissionPreset, string>>, preset: PermissionPreset): string {
  return knownPreset(preset) ? names[preset] : preset;
}

// issuedLabel is an issued item's kind in the page's words, or the value
// itself when the page has none for it (a kind a newer server added).
function issuedLabel(kind: string): string {
  return kind === 'credential' ? 'Credential' : kind === 'hook' ? 'Hook' : kind;
}

// warningText is a document warning in the words of this section; one the
// page has no words for is shown by its name.
function warningText(warning: PermissionsWarning): string {
  switch (warning) {
    case 'open_signup_on':
      return 'Members cannot make member links, yet open sign-up is on, so the server’s address still admits anyone.';
    case 'default_channel_exposes_members':
      return 'Members cannot see the member directory, yet a default channel still shows them to each other.';
    default:
      return String(warning);
  }
}

function labels(permissions: readonly string[]): string {
  return permissions.map(labelOf).join(', ');
}

// lockedReason says why a cell cannot be changed here, or null when it can.
// `value` is the cell as shown (with an unsaved edit).
function lockedReason(doc: PermissionsResponseV1, who: WhoAmIResponseV1, row: PermissionRowV1, role: Role, value: boolean, busy: boolean): string | null {
  if (!known(row.permission)) return 'This page has no words for this permission, so it is shown by its name and cannot be changed here.';
  if (fixedOf(row, role)) {
    if (role === 'member') return 'Fixed: members cannot hold this permission.';
    return value ? 'Fixed: operators always hold this permission.' : 'Fixed: operators never hold this permission.';
  }
  if (!canDo(who, 'change_server_settings')) return reasonFor('change_server_settings');
  if (!doc.editable.includes(role)) return role === 'operator' ? 'Only an owner changes what operators may do.' : 'You may not change this column.';
  // A grant never exceeds the granter (design §4.4): the server refuses it, so
  // the page does not offer it.
  if (!value && !canDo(who, row.permission)) return 'You do not hold this permission yourself, so you cannot give it to others.';
  if (busy) return 'Saving…';
  return null;
}

// ruleBreak finds, in the cells as they would stand, the first rule of the
// design §4.4 they break, in the page's own words; null when none.
function ruleBreak(rows: readonly PermissionRowV1[], valueOf: (row: PermissionRowV1, role: Role) => boolean): { readonly permission: string; readonly line: string } | null {
  const byName = new Map(rows.map((row) => [row.permission, row]));
  for (const row of rows) {
    if (valueOf(row, 'member') && !valueOf(row, 'operator')) {
      return { permission: row.permission, line: `“${labelOf(row.permission)}” cannot be on for members while it is off for operators.` };
    }
  }
  const needs: readonly (readonly [Permission, Permission])[] = [
    ['share_screen', 'use_voice'],
    ['use_camera', 'use_voice'],
    ['act_as_members', 'manage_members'],
  ];
  for (const [dependent, base] of needs) {
    const on = byName.get(dependent);
    const under = byName.get(base);
    if (on === undefined || under === undefined) continue;
    for (const role of roles) {
      if (valueOf(on, role) && !valueOf(under, role)) {
        return { permission: dependent, line: `“${labelOf(dependent)}” needs “${labelOf(base)}” in the same column.` };
      }
    }
  }
  return null;
}

// permissionIn finds the first permission the page knows by name in the
// server's message. The message itself is never shown.
function permissionIn(message: string | undefined): Permission | null {
  if (message === undefined || !message.includes('does not enforce')) return null;
  for (const name of Object.keys(info)) if (known(name) && message.includes(name)) return name;
  return null;
}

// movedCells says, in the page's words, which cells differ between two
// versions of the document.
export function movedCells(before: PermissionsResponseV1, after: PermissionsResponseV1): string[] {
  const was = new Map(before.rows.map((row) => [row.permission, row]));
  const said: string[] = [];
  for (const row of after.rows) {
    const old = was.get(row.permission);
    for (const role of roles) {
      if (old === undefined) {
        said.push(`${labelOf(row.permission)} for ${roleLabels[role].toLowerCase()} is new`);
      } else if (cellOf(old, role) !== cellOf(row, role)) {
        said.push(`${labelOf(row.permission)} for ${roleLabels[role].toLowerCase()} is now ${cellOf(row, role) ? 'on' : 'off'}`);
      }
    }
  }
  return said;
}

interface MatrixProps {
  readonly doc: PermissionsResponseV1;
  readonly who: WhoAmIResponseV1;
  readonly phone: boolean;
  readonly column: Role;
  // The unsaved cells, keyed permission:role.
  readonly edits: Readonly<Record<string, boolean>>;
  readonly stale: boolean;
  readonly flagged: string | null;
  readonly busy: boolean;
  readonly onToggle: (permission: Permission, role: Role) => void;
}

// A cell: a checkbox that stays focusable when it is locked (aria-disabled,
// not disabled), so its reason is reachable on hover and on keyboard focus.
function Cell({
  id,
  row,
  role,
  props,
  label,
}: {
  readonly id: string;
  readonly row: PermissionRowV1;
  readonly role: Role;
  readonly props: MatrixProps;
  readonly label: string;
}) {
  const { doc, who, edits, stale, busy, onToggle } = props;
  const edit = edits[keyOf(row.permission, role)];
  const value = edit ?? cellOf(row, role);
  const reason = lockedReason(doc, who, row, role, value, busy);
  const reasonId = `${id}-reason`;
  const changed = edit !== undefined;
  return (
    <span className={`cell${reason === null ? '' : ' locked'}${changed ? (stale ? ' edited stale' : ' edited') : ''}`} title={reason ?? undefined}>
      <input
        id={id}
        type="checkbox"
        role={props.phone ? 'switch' : 'checkbox'}
        data-cell={keyOf(row.permission, role)}
        aria-label={label}
        aria-disabled={reason === null ? undefined : true}
        aria-describedby={reason === null ? undefined : reasonId}
        aria-invalid={props.flagged === row.permission ? true : undefined}
        checked={value}
        onChange={() => {
          if (reason === null && known(row.permission)) onToggle(row.permission, role);
        }}
      />
      {changed ? <span className="edit-mark">{stale ? ' (unsaved, may be stale)' : ' (unsaved)'}</span> : null}
      {reason === null ? null : (
        <span id={reasonId} role="tooltip" className="reason">
          {reason}
        </span>
      )}
    </span>
  );
}

function Describe({ row }: { readonly row: PermissionRowV1 }) {
  return (
    <>
      <strong>{labelOf(row.permission)}</strong>
      <span className="what">{known(row.permission) ? info[row.permission].text : 'A permission this page has no words for.'}</span>
      {row.permission === 'act_as_members' ? <span className="what">{actAsMembersNotEnded}</span> : null}
    </>
  );
}

// PermissionMatrix is the matrix: a table at desktop width, and on a phone one
// list of permissions with one switch per row for the column chosen above.
export function PermissionMatrix(props: MatrixProps) {
  const { doc, phone, column } = props;
  const id = useId();
  if (phone) {
    return (
      <ul className="matrix-list" aria-label={`${roleLabels[column]} permissions`}>
        {doc.rows.map((row) => {
          return (
            <li key={row.permission} className={props.flagged === row.permission ? 'flagged' : undefined}>
              <label className="switch-row">
                <span className="describe">
                  <Describe row={row} />
                </span>
                <Cell id={`${id}-${row.permission}`} row={row} role={column} props={props} label={`${labelOf(row.permission)}, ${roleLabels[column]}`} />
              </label>
            </li>
          );
        })}
        <li className="owners-line">{ownersLine}</li>
      </ul>
    );
  }
  return (
    <table className="matrix">
      <caption className="sr-only">Permissions by role</caption>
      <thead>
        <tr>
          <th scope="col">Permission</th>
          <th scope="col">Members</th>
          <th scope="col">Operators</th>
          <th scope="col">Owners</th>
        </tr>
      </thead>
      <tbody>
        {doc.rows.map((row) => (
          <tr key={row.permission} className={props.flagged === row.permission ? 'flagged' : undefined}>
            <th scope="row">
              <Describe row={row} />
            </th>
            {roles.map((role) => (
              <td key={role}>
                <Cell id={`${id}-${row.permission}-${role}`} row={row} role={role} props={props} label={`${labelOf(row.permission)}, ${roleLabels[role]}`} />
              </td>
            ))}
            <td>
              <span className="cell locked">
                <input type="checkbox" checked readOnly aria-disabled="true" aria-label={`${labelOf(row.permission)}, Owners`} aria-describedby={`${id}-${row.permission}-owners`} />
                <span id={`${id}-${row.permission}-owners`} role="tooltip" className="reason">
                  {ownersLine}
                </span>
              </span>
            </td>
          </tr>
        ))}
      </tbody>
      <tfoot>
        <tr>
          <td colSpan={4}>{ownersLine}</td>
        </tr>
      </tfoot>
    </table>
  );
}

type Line = { readonly text: string; readonly refused: boolean } | null;

export interface PermissionsSectionProps {
  readonly who: WhoAmIResponseV1;
  // Counts the times the server said the permissions changed.
  readonly refresh: number;
  // The people the page knows by name, for the issued_to_others list.
  readonly names?: ReadonlyMap<number, string>;
  // A request to bring a cell into view and focus (the Server section's
  // summary lines link here); n counts the requests, so each is met once.
  readonly focus?: { readonly permission: Permission; readonly n: number };
  readonly onUnauthorized: () => void;
}

export function PermissionsSection({ who, refresh, names, focus, onUnauthorized }: PermissionsSectionProps) {
  const shown = useContext(SettingsOpen);
  const phone = usePhone();
  const [doc, setDoc] = useState<PermissionsResponseV1 | null>(null);
  const [readLine, setReadLine] = useState<string | null>(null);
  const [edits, setEdits] = useState<Readonly<Record<string, boolean>>>({});
  const [stale, setStale] = useState(false);
  const [moved, setMoved] = useState<readonly string[] | null>(null);
  const [column, setColumn] = useState<Role>('member');
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line>(null);
  const [flagged, setFlagged] = useState<string | null>(null);
  // The list a write answered with; it stays (empty once all are revoked) until dismissed.
  const [issued, setIssued] = useState<readonly IssuedByOperatorV1[] | null>(null);
  const root = useRef<HTMLElement>(null);
  // Counts the reads and answers started, so an answer to an earlier one is
  // dropped: the newest request wins.
  const reads = useRef(0);
  const docRef = useRef<PermissionsResponseV1 | null>(null);
  const editsRef = useRef(edits);
  editsRef.current = edits;
  const whoRef = useRef(who);
  whoRef.current = who;
  const handled = useRef(0);

  const canWrite = canDo(who, 'change_server_settings');

  const take = useCallback((next: PermissionsResponseV1) => {
    docRef.current = next;
    setDoc(next);
  }, []);

  // read asks again; `after`, if given, is the line to say once the answer is in.
  const read = useCallback((after?: string) => {
    const mine = ++reads.current;
    void api.permissions().then((result) => {
      if (mine !== reads.current) return;
      if (result.ok) {
        const before = docRef.current;
        const next = result.value;
        // An edit the document now holds is no longer a change.
        const kept: Record<string, boolean> = {};
        const dropped: string[] = [];
        for (const [key, value] of Object.entries(editsRef.current)) {
          const [permission, role] = key.split(':');
          const row = next.rows.find((r) => r.permission === permission);
          if (row === undefined || (role !== 'member' && role !== 'operator') || cellOf(row, role) === value) continue;
          // A cell that is no longer the caller's to change loses its edit:
          // it could not be sent, and must not wait to be.
          if (lockedReason(next, whoRef.current, row, role, cellOf(row, role), false) !== null) {
            dropped.push(`${labelOf(row.permission)} for ${roleLabels[role].toLowerCase()} is no longer yours to change, so your edit to it was dropped`);
            continue;
          }
          kept[key] = value;
        }
        setEdits(kept);
        take(next);
        setReadLine(null);
        if (after !== undefined) setLine({ text: after, refused: true });
        if (before !== null && (before.revision !== next.revision || dropped.length > 0)) {
          setMoved([...(before.revision === next.revision ? [] : movedCells(before, next)), ...dropped]);
          setStale(Object.keys(kept).length > 0);
        } else if (Object.keys(kept).length === 0) {
          setStale(false);
        }
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else {
        if (after !== undefined) setLine(null);
        setReadLine(result.failure === 'network' ? 'Cannot reach the server.' : 'Could not read the permissions.');
      }
    });
  }, [onUnauthorized, take]);

  // Read afresh each time Settings opens, and when the server says the
  // permissions changed.
  useEffect(() => {
    if (shown) read();
  }, [shown, refresh, read]);

  // Losing the permission closes a confirmation that was open: it never sends.
  const operatorEditable = doc?.editable.includes('operator') ?? false;
  useEffect(() => {
    if (!canWrite || !operatorEditable) setConfirming(false);
  }, [canWrite, operatorEditable]);

  // A link from the Server section brings a cell into view, once.
  const focusN = focus?.n ?? 0;
  const focusWhat = focus?.permission;
  const loaded = doc !== null;
  useEffect(() => {
    if (focusN === 0 || focusN === handled.current || !loaded || focusWhat === undefined) return;
    if (phone && column !== 'member') {
      setColumn('member');
      return;
    }
    const cell = root.current?.querySelector<HTMLElement>(`[data-cell="${keyOf(focusWhat, 'member')}"]`);
    if (cell === null || cell === undefined) return;
    handled.current = focusN;
    cell.focus();
    if (typeof cell.scrollIntoView === 'function') cell.scrollIntoView({ block: 'center' });
  }, [focusN, focusWhat, loaded, phone, column]);

  const valueOf = useCallback(
    (row: PermissionRowV1, role: Role) => editsRef.current[keyOf(row.permission, role)] ?? cellOf(row, role),
    [],
  );

  // The patch: the cells that differ from the document, in its order.
  const patch = useMemo(() => {
    const out: PermissionChangeV1[] = [];
    if (doc === null) return out;
    for (const row of doc.rows) {
      if (!known(row.permission)) continue;
      for (const role of roles) {
        const edit = edits[keyOf(row.permission, role)];
        if (edit !== undefined && edit !== cellOf(row, role)) out.push({ permission: row.permission, role, granted: edit });
      }
    }
    return out;
  }, [doc, edits]);

  const turnsOnActAs = patch.some((c) => c.permission === 'act_as_members' && c.role === 'operator' && c.granted);

  function toggle(permission: Permission, role: Role) {
    if (doc === null || busy || !canWrite) return;
    const row = doc.rows.find((r) => r.permission === permission);
    if (row === undefined) return;
    const key = keyOf(permission, role);
    const now = edits[key] ?? cellOf(row, role);
    // Back to what the document holds is no change at all.
    const next = Object.fromEntries(Object.entries(edits).filter(([k]) => k !== key));
    if (now === cellOf(row, role)) next[key] = !now;
    setEdits(next);
    setConfirming(false);
    setLine(null);
    setFlagged(null);
    if (Object.keys(next).length === 0) setStale(false);
  }

  // After a write answered: the document and what it left.
  function answered(result: Awaited<ReturnType<typeof api.putPermissions>>, done: string) {
    setBusy(false);
    setConfirming(false);
    if (result.ok) {
      // Anything read before this answer is older than it.
      reads.current++;
      take(result.value);
      setEdits({});
      setStale(false);
      setMoved(null);
      setFlagged(null);
      setLine({ text: done, refused: false });
      if (result.value.issued_to_others !== undefined) setIssued(result.value.issued_to_others);
      return;
    }
    if (result.failure === 'unauthorized') {
      onUnauthorized();
      return;
    }
    const nothing = 'Nothing was saved.';
    if (result.code === 'permissions_stale') {
      // Never retried blind: read again, say what changed, decide again.
      const said = refusalLine(result, nothing);
      setLine({ text: `${said} Reading them again…`, refused: true });
      read(`${said} They have been read again; check your changes and save again.`);
      return;
    }
    if (result.code === 'permissions_conflict' && doc !== null) {
      const broken = ruleBreak(doc.rows, valueOf);
      if (broken !== null) {
        setFlagged(broken.permission);
        setLine({ text: `${broken.line} ${nothing}`, refused: true });
        return;
      }
      const named = permissionIn(result.message);
      if (named !== null) {
        setFlagged(named);
        setLine({ text: `This server does not enforce “${labelOf(named)}” yet, so that cell cannot be changed. ${nothing}`, refused: true });
        return;
      }
    }
    setLine({ text: refusalLine(result, nothing), refused: true });
  }

  function send(request: PutPermissionsRequestV1, done: string) {
    setBusy(true);
    setLine(null);
    setFlagged(null);
    void api.putPermissions(request).then((result) => {
      answered(result, done);
    });
  }

  function save() {
    if (doc === null || busy || !canWrite || patch.length === 0) return;
    if (turnsOnActAs && !confirming) {
      setConfirming(true);
      return;
    }
    send({ revision: doc.revision, changes: patch }, 'Saved.');
  }

  function applyPreset(preset: PermissionPreset) {
    if (doc === null || busy || !canWrite || patch.length > 0 || !doc.editable.includes('member')) return;
    send({ revision: doc.revision, preset }, `Applied ${presetLabel(presetNames, preset)}.`);
  }

  const dirty = patch.length > 0;
  const writeReason = !canWrite ? reasonFor('change_server_settings') : doc !== null && doc.editable.length === 0 ? 'You may not change any column.' : null;
  const canPreset = canWrite && doc !== null && doc.editable.includes('member');

  if (doc === null) {
    return (
      <section ref={root} className="roles-section" aria-label="Roles and permissions">
        {phone ? null : <h4>Roles and permissions</h4>}
        {readLine === null ? (
          <p className="hint" role="status">
            Reading the permissions…
          </p>
        ) : (
          <p role="alert" className="notice">
            {readLine}
          </p>
        )}
      </section>
    );
  }

  return (
    <section ref={root} className="roles-section" aria-label="Roles and permissions">
      {phone ? null : <h4>Roles and permissions</h4>}
      {readLine === null ? null : (
        <p role="alert" className="notice">
          {readLine}
        </p>
      )}
      {writeReason === null ? null : <p className="hint">{writeReason}</p>}

      <div className="preset-cards">
        {doc.presets.map((p) => {
          const changes = presetChanges(doc, p.preset);
          const here = doc.preset === p.preset;
          return (
            <section key={p.preset} className={`preset-card${here ? ' current' : ''}`} aria-label={`${presetLabel(presetShort, p.preset)} preset`}>
              <h5>
                {presetLabel(presetNames, p.preset)}
                {here ? <span className="current-mark"> (the members’ column matches this)</span> : null}
              </h5>
              {changes === null || (changes.on.length === 0 && changes.off.length === 0) ? (
                <p className="hint">Changes nothing in the members’ column now.</p>
              ) : (
                <>
                  {changes.on.length === 0 ? null : <p>Would turn on: {labels(changes.on)}.</p>}
                  {changes.off.length === 0 ? null : <p>Would turn off: {labels(changes.off)}.</p>}
                </>
              )}
              <button
                type="button"
                disabled={!canPreset || busy || dirty || here || changes === null || (changes.on.length === 0 && changes.off.length === 0)}
                title={dirty ? 'Save or discard your changes first.' : here || changes === null || (changes.on.length === 0 && changes.off.length === 0) ? 'Already applied.' : undefined}
                onClick={() => {
                  applyPreset(p.preset);
                }}
              >
                Use {presetLabel(presetShort, p.preset)} preset
              </button>
            </section>
          );
        })}
      </div>
      {dirty ? <p className="hint">Save or discard your changes below before using a preset.</p> : null}

      {doc.warnings.length === 0 ? null : (
        <ul className="join-warnings" aria-label="Warnings about these permissions">
          {doc.warnings.map((w) => (
            <li key={w}>{warningText(w)}</li>
          ))}
        </ul>
      )}

      {moved === null ? null : (
        <div role="status" className="notice">
          <p>{stale ? 'The permissions changed on the server while you were editing. Your unsaved changes are kept and marked; check them, then save or discard.' : 'The permissions changed on the server.'}</p>
          {moved.length === 0 ? null : <p>Since you read them: {moved.join('; ')}.</p>}
        </div>
      )}

      {phone ? (
        <div role="group" aria-label="Column" className="segmented">
          {roles.map((role) => (
            <button
              key={role}
              type="button"
              aria-pressed={column === role}
              onClick={() => {
                setColumn(role);
              }}
            >
              {roleLabels[role]}
            </button>
          ))}
        </div>
      ) : null}

      <PermissionMatrix doc={doc} who={who} phone={phone} column={column} edits={edits} stale={stale} flagged={flagged} busy={busy} onToggle={toggle} />

      <p className="managers-block">{managersBlock}</p>

      <div className="actions">
        <button type="button" disabled={!dirty || !canWrite || busy} aria-busy={busy} onClick={save}>
          {busy ? 'Saving…' : 'Save changes'}
        </button>
        <button
          type="button"
          disabled={!dirty || busy}
          onClick={() => {
            setEdits({});
            setStale(false);
            setMoved(null);
            setConfirming(false);
            setLine(null);
            setFlagged(null);
          }}
        >
          Discard changes
        </button>
        {dirty ? <span className="hint">{String(patch.length)} unsaved {patch.length === 1 ? 'change' : 'changes'}</span> : null}
      </div>

      {confirming && canWrite && operatorEditable && turnsOnActAs ? (
        <div role="group" aria-label="Confirm Act as members" className="signout-confirm">
          <p>Operators {actAsMembersSentence}. Turn it on for operators?</p>
          <div className="actions">
            <button type="button" disabled={busy || !canWrite} onClick={save}>
              Turn it on for operators
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => {
                setConfirming(false);
              }}
            >
              Cancel
            </button>
          </div>
        </div>
      ) : null}

      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>

      {issued === null ? null : (
        <IssuedList
          who={who}
          items={issued}
          {...(names === undefined ? {} : { names })}
          onDone={(gone) => {
            setIssued((now) => (now ?? []).filter((i) => !gone(i)));
          }}
          onDismiss={() => {
            setIssued(null);
          }}
          onUnauthorized={onUnauthorized}
        />
      )}
    </section>
  );
}

const when = (stamp: string) => (Number.isNaN(Date.parse(stamp)) ? stamp : new Date(stamp).toLocaleString());

// IssuedList is the answer to turning act_as_members off for operators (design
// §7.5, O13): the live credentials and hooks operators issued to other people,
// which taking the permission away does not end. Each can be revoked here; a
// person's credentials can be signed out all at once.
function IssuedList({
  who,
  items,
  names,
  onDone,
  onDismiss,
  onUnauthorized,
}: {
  readonly who: WhoAmIResponseV1;
  readonly items: readonly IssuedByOperatorV1[];
  readonly names?: ReadonlyMap<number, string>;
  readonly onDone: (gone: (item: IssuedByOperatorV1) => boolean) => void;
  readonly onDismiss: () => void;
  readonly onUnauthorized: () => void;
}) {
  const canRevoke = canDo(who, 'manage_members');
  const [busy, setBusy] = useState<string | null>(null);
  const [line, setLine] = useState<Line>(null);
  // The question open: signing a person out everywhere, or revoking one of the
  // viewer's own. Nothing is sent before it is answered yes.
  const [asking, setAsking] = useState<{ readonly text: string; readonly yes: string; readonly go: () => void } | null>(null);
  const person = (id: number) => (id === who.id ? `${names?.get(id) ?? `person #${String(id)}`} (this is you)` : (names?.get(id) ?? `person #${String(id)}`));
  const plain = (id: number) => names?.get(id) ?? `person #${String(id)}`;

  // Losing manage_members closes the question: it never sends.
  useEffect(() => {
    if (!canRevoke) setAsking(null);
  }, [canRevoke]);

  function run(key: string, call: () => Promise<Awaited<ReturnType<typeof api.revokeIssuedHook>>>, gone: (item: IssuedByOperatorV1) => boolean, done: string) {
    if (busy !== null || !canRevoke) return;
    setBusy(key);
    setLine(null);
    void call().then((result) => {
      setBusy(null);
      if (result.ok) {
        onDone(gone);
        setLine({ text: done, refused: false });
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setLine({ text: refusalLine(result, 'Nothing was revoked.'), refused: true });
    });
  }

  // The first credential of each person carries their one Sign out everywhere.
  const firstOf = new Set<string>();
  const leaders = new Set<string>();
  for (const item of items) {
    if (item.kind !== 'credential' || firstOf.has(String(item.principal_id))) continue;
    firstOf.add(String(item.principal_id));
    leaders.add(`${item.kind}-${String(item.id)}`);
  }

  return (
    <section className="issued-list" aria-label="Issued to others by operators">
      <h5>Still working after this change</h5>
      <p>
        Operators issued these to other people. Taking “Act as members” away does not end them. {canRevoke ? '' : reasonFor('manage_members')}
      </p>
      {items.length === 0 ? <p className="hint">Nothing issued by operators is left on this list.</p> : null}
      <ul>
        {items.map((item) => {
          const key = `${item.kind}-${String(item.id)}`;
          // Revoked by its kind's route; a kind this page does not know it
          // cannot revoke here, and never sends to another kind's route.
          const revoke = item.kind === 'credential' ? () => api.revokeIssuedCredential(item.id) : item.kind === 'hook' ? () => api.revokeIssuedHook(item.id) : null;
          return (
            <li key={key}>
              <span>
                {issuedLabel(item.kind)} #{item.id} for {person(item.principal_id)}, issued by {person(item.issued_by)} on {when(item.issued_at)}
              </span>
              <span className="actions">
                {leaders.has(key) ? (
                  <button
                    type="button"
                    disabled={!canRevoke || busy !== null}
                    aria-label={`Sign out everywhere, ${plain(item.principal_id)}`}
                    onClick={() => {
                      setAsking({
                        text: `Sign ${person(item.principal_id)} out everywhere? Every credential of theirs ends, on every device${item.principal_id === who.id ? ', and you are signed out here too' : ''}.`,
                        yes: 'Yes, sign out everywhere',
                        go: () => {
                          run(`all-${String(item.principal_id)}`, () => api.signOutPrincipalEverywhere(item.principal_id), (i) => i.kind === 'credential' && i.principal_id === item.principal_id, `Signed ${plain(item.principal_id)} out everywhere.`);
                        },
                      });
                    }}
                  >
                    Sign out everywhere
                  </button>
                ) : null}
                <button
                  type="button"
                  disabled={!canRevoke || busy !== null || revoke === null}
                  title={revoke === null ? 'This page has no words for this kind, so it cannot revoke it here.' : undefined}
                  aria-label={`Revoke ${item.kind} #${String(item.id)}`}
                  onClick={() => {
                    if (revoke === null) return;
                    const go = () => {
                      run(key, revoke, (i) => i.kind === item.kind && i.id === item.id, `Revoked ${item.kind} #${String(item.id)}.`);
                    };
                    if (item.principal_id === who.id) {
                      setAsking({ text: `Revoke your own ${item.kind} #${String(item.id)}? It stops working at once.`, yes: 'Yes, revoke', go });
                    } else go();
                  }}
                >
                  Revoke
                </button>
              </span>
            </li>
          );
        })}
      </ul>
      {asking === null || !canRevoke ? null : (
        <div role="group" aria-label="Confirm" className="signout-confirm">
          <p>{asking.text}</p>
          <div className="actions">
            <button
              type="button"
              className="danger"
              disabled={busy !== null || !canRevoke}
              onClick={() => {
                const go = asking.go;
                setAsking(null);
                go();
              }}
            >
              {asking.yes}
            </button>
            <button
              type="button"
              onClick={() => {
                setAsking(null);
              }}
            >
              Cancel
            </button>
          </div>
        </div>
      )}
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
      <button type="button" onClick={onDismiss}>
        Dismiss this list
      </button>
    </section>
  );
}
