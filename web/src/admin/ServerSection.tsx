// The console's Server section (ADR-014 design §10.2, issue #1018): the
// server's name (read-only: it is set on the host), the setup card while the
// permissions document is at its first revision, two summary lines drawn from
// the document, the document's warnings, and the Joining panel with its open
// sign-up, pause, default link expiry, invites and brakes.
//
// Everything here keys on whoami (permissions.ts); the server decides each
// request again, and a refusal is shown by its code and the permission it
// names. The setup card's Personal and Group first show what the preset
// changes in the members' column, then apply it with PUT /v1/permissions;
// Decide later dismisses the card for this browser. Writes that need a recent
// proof open the Reauth overlay (api.ts asks for it) and are sent again.
import { useCallback, useContext, useEffect, useRef, useState } from 'react';
import * as api from '../api';
import { SettingsOpen } from '../joiningSettings';
import type { ChannelV0 } from '../schema/channel';
import type { KnownPermissionPreset, Permission, PermissionPreset, PermissionsResponseV1, PermissionsWarning } from '../schema/permission';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { JoiningPanel } from './JoiningPanel';
import { canDo, permissionName, reasonFor, refusalLine } from './permissions';

type Line = { readonly text: string; readonly refused: boolean };

// The card offers the presets this page knows; the server's own list is the
// Roles and permissions section's.
const presetLabels: Readonly<Record<KnownPermissionPreset, string>> = { personal: 'Personal server', group: 'Group server' };

// memberCell reads a permission's member column in the document; undefined
// when the document has no row for it.
function memberCell(doc: PermissionsResponseV1, permission: Permission): boolean | undefined {
  return doc.rows.find((row) => row.permission === permission)?.member;
}

// presetChanges is what applying a preset would change in the members'
// column: the permissions it turns on and the ones it turns off.
export function presetChanges(doc: PermissionsResponseV1, preset: PermissionPreset): { readonly on: readonly string[]; readonly off: readonly string[] } | null {
  const chosen = doc.presets.find((p) => p.preset === preset);
  if (chosen === undefined) return null;
  const now = new Set<string>(doc.rows.filter((row) => row.member).map((row) => row.permission));
  const then = new Set<string>(chosen.member);
  return {
    on: [...then].filter((p) => !now.has(p)),
    off: [...now].filter((p) => !then.has(p)),
  };
}

// warningLine is a document warning in words; one this page has no words for
// is shown by its name.
export function permissionsWarningLine(warning: PermissionsWarning): string {
  switch (warning) {
    case 'open_signup_on':
      return 'Members cannot make member links, but the server’s address still lets anyone make an account.';
    case 'default_channel_exposes_members':
      return 'Members cannot see the member directory, yet a default channel shows them each other.';
    default:
      return String(warning);
  }
}

function names(permissions: readonly string[]): string {
  return permissions.map(permissionName).join(', ');
}

function SetupCard({
  doc,
  who,
  origin,
  onApplied,
  onStale,
  onUnauthorized,
}: {
  readonly doc: PermissionsResponseV1;
  readonly who: WhoAmIResponseV1;
  readonly origin: string;
  readonly onApplied: (doc: PermissionsResponseV1) => void;
  // The document moved under the page: it is read again, and the line is
  // said by the section, since the card may go with the new revision.
  readonly onStale: (line: string) => void;
  readonly onUnauthorized: () => void;
}) {
  const [dismissed, setDismissed] = useState(() => api.setupCardDismissed(origin, who.id));
  const [asked, setAsked] = useState<KnownPermissionPreset | null>(null);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  const canChange = canDo(who, 'change_server_settings');
  // Losing the permission closes a question that was open.
  useEffect(() => {
    if (!canChange) setAsked(null);
  }, [canChange]);
  if (dismissed) return null;
  const changes = asked === null ? null : presetChanges(doc, asked);

  function apply(preset: KnownPermissionPreset) {
    if (busy || !canChange) return;
    setBusy(true);
    setLine(null);
    void api.putPermissions({ revision: doc.revision, preset }).then((result) => {
      setBusy(false);
      if (result.ok) {
        setAsked(null);
        // A preset the members already match moves no revision, and the card
        // would come back: the choice is made, so it is put away for good.
        api.dismissSetupCard(origin, who.id);
        setDismissed(true);
        onApplied(result.value);
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      setAsked(null);
      const said = refusalLine(result, 'Nothing was changed.');
      if (result.code === 'permissions_stale') onStale(said);
      else setLine({ text: said, refused: true });
    });
  }

  return (
    <section className="setup-card" aria-label="Set up this server">
      <h4>How will this server be used?</h4>
      {canChange ? null : <p className="hint">{reasonFor('change_server_settings')}</p>}
      <div className="actions">
        {(['personal', 'group'] as const).map((preset) => (
          <button
            key={preset}
            type="button"
            disabled={!canChange || busy}
            aria-pressed={asked === preset}
            onClick={() => {
              setLine(null);
              setAsked(preset);
            }}
          >
            {presetLabels[preset]}
          </button>
        ))}
        <button
          type="button"
          onClick={() => {
            api.dismissSetupCard(origin, who.id);
            setDismissed(true);
          }}
        >
          Decide later
        </button>
      </div>
      {asked === null || changes === null || !canChange ? null : (
        <div role="group" aria-label={`Apply ${presetLabels[asked]}`} className="signout-confirm">
          <p>
            {changes.on.length === 0 && changes.off.length === 0
              ? `${presetLabels[asked]} already matches what members may do, so nothing changes.`
              : `${presetLabels[asked]} changes what members may do.`}
          </p>
          {changes.on.length === 0 ? null : <p>Members gain: {names(changes.on)}.</p>}
          {changes.off.length === 0 ? null : <p>Members lose: {names(changes.off)}.</p>}
          <div className="actions">
            <button
              type="button"
              disabled={busy || !canChange}
              aria-busy={busy}
              onClick={() => {
                apply(asked);
              }}
            >
              {busy ? 'Applying…' : `Apply ${presetLabels[asked]}`}
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => {
                setAsked(null);
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
    </section>
  );
}

export function ServerSection({
  who,
  origin,
  channels,
  refresh,
  onOpenRoles,
  onUnauthorized,
}: {
  readonly who: WhoAmIResponseV1;
  readonly origin: string;
  readonly channels: readonly ChannelV0[];
  // Counts the times the server said the permissions changed: whatever this
  // section shows from them is read again.
  readonly refresh: number;
  // Where a summary line links to its cell, once the Roles and permissions
  // section exists; plain text before that.
  readonly onOpenRoles?: (permission: Permission) => void;
  readonly onUnauthorized: () => void;
}) {
  const shown = useContext(SettingsOpen);
  const [doc, setDoc] = useState<PermissionsResponseV1 | null>(null);
  const [serverName, setServerName] = useState<string | null>(null);
  const [docLine, setDocLine] = useState<string | null>(null);
  // Said by the section when the card that asked has gone with a new revision.
  const [note, setNote] = useState<string | null>(null);
  // Counts the reads started, so an answer to an earlier one is ignored.
  const reads = useRef(0);

  const read = useCallback(() => {
    const mine = ++reads.current;
    void api.permissions().then((result) => {
      if (mine !== reads.current) return;
      if (result.ok) {
        setDoc(result.value);
        setDocLine(null);
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setDocLine(result.failure === 'network' ? 'Cannot reach the server.' : 'Could not read the permissions.');
    });
    void api.joinSettings().then((result) => {
      if (mine === reads.current && result.ok) setServerName(result.value.server_name);
    });
  }, [onUnauthorized]);

  // Read afresh each time Settings opens, and when the server says the
  // permissions changed.
  useEffect(() => {
    if (shown) {
      setNote(null);
      read();
    }
  }, [shown, refresh, read]);

  const sees = doc === null ? undefined : memberCell(doc, 'see_member_directory');
  const voice = doc === null ? undefined : memberCell(doc, 'use_voice');
  const cell = (permission: Permission, text: string) =>
    onOpenRoles === undefined ? (
      text
    ) : (
      <button
        type="button"
        className="link"
        onClick={() => {
          onOpenRoles(permission);
        }}
      >
        {text}
      </button>
    );

  return (
    <div className="server-section" role="group" aria-label="Server settings">
      <p>
        Server name: <strong>{serverName ?? '…'}</strong>
      </p>
      <p className="hint">Set on the server’s host (<code>--server-name</code>).</p>
      {docLine === null ? null : (
        <p role="alert" className="notice">
          {docLine}
        </p>
      )}
      {doc === null || doc.revision !== 1 ? null : (
        <SetupCard
          doc={doc}
          who={who}
          origin={origin}
          onApplied={setDoc}
          onStale={(said) => {
            setNote(said);
            read();
          }}
          onUnauthorized={onUnauthorized}
        />
      )}
      {note === null ? null : (
        <p role="status" className="notice">
          {note}
        </p>
      )}
      {sees === undefined && voice === undefined ? null : (
        <ul className="summary-lines" aria-label="What members have">
          {sees === undefined ? null : (
            <li>
              Members see: {cell('see_member_directory', sees ? 'everyone' : 'only people they share a channel with')}
            </li>
          )}
          {voice === undefined ? null : <li>Voice: {cell('use_voice', voice ? 'on for members' : 'off for members')}</li>}
        </ul>
      )}
      {doc === null || doc.warnings.length === 0 ? null : (
        <ul className="join-warnings" aria-label="Permission warnings">
          {doc.warnings.map((w) => (
            <li key={w}>{permissionsWarningLine(w)}</li>
          ))}
        </ul>
      )}
      <JoiningPanel who={who} channels={channels} onUnauthorized={onUnauthorized} />
    </div>
  );
}
