// What a row of the Members section, and of the member panel, does with a
// person (ADR-014 design §10.2, §10.3, issue #1020, #972): the ⋯ menu of
// memberActions.ts, and the in-place question or form each item opens.
//
// The menu is the message log's (MessageMenu.tsx): opened by its button, Enter
// or Space on it, a right-click on the row (the caller's useOpeners), a long
// press, and on a phone a sheet from the bottom. Choosing a destructive item
// asks first, in the row, with focus on Cancel. Nothing is sent until the
// server's question has been answered yes, nothing is shown as done until the
// server has said so, and nothing is sent once the viewer's grant for it has
// gone: the items are worked out afresh from whoami at the moment of sending.
//
// The words of every refusal are this page's own, chosen by the server's code
// and the permission it names; the server's message is never shown. A link, a
// credential or a hook acts as the person: each is worded so and its secret is
// shown once (SecretOnce.tsx).
import { useEffect, useId, useRef, useState, type KeyboardEvent, type RefObject, type SyntheticEvent } from 'react';
import * as api from '../api';
import { memberActions, voiceOffLine, voiceRoleOff, type MemberAction, type MemberActionId } from '../memberActions';
import { ActionsMenu, type MenuItem } from '../MessageMenu';
import { deviceLink } from '../route';
import type { PermissionRowV1 } from '../schema/permission';
import type { ServerMemberV1, WhoAmIResponseV1 } from '../schema/principal';
import type { SignInLinkPurpose } from '../schema/signin';
import { canDo, refusalLine, type Refusal } from './permissions';
import { SecretOnce } from './SecretOnce';
import { timeOf } from '../time';

type Line = { readonly text: string; readonly refused: boolean } | null;
type Role = 'member' | 'operator' | 'owner';

type Panel =
  | { readonly kind: 'ask'; readonly id: 'signout' | 'disable' | 'remove' }
  | { readonly kind: 'role' }
  | { readonly kind: 'link' }
  | { readonly kind: 'credential' }
  | { readonly kind: 'hook' }
  | { readonly kind: 'channel' };

// The panel an item opens, by the item that opens it.
const panelFor: Partial<Record<MemberActionId, Panel>> = {
  signout: { kind: 'ask', id: 'signout' },
  disable: { kind: 'ask', id: 'disable' },
  remove: { kind: 'ask', id: 'remove' },
  role: { kind: 'role' },
  link: { kind: 'link' },
  credential: { kind: 'credential' },
  hook: { kind: 'hook' },
  channel: { kind: 'channel' },
};

const panelAction = (panel: Panel): MemberActionId => (panel.kind === 'ask' ? panel.id : panel.kind);

// personRefusal is the page's own words for a write to a person the server did
// not make: the codes of these routes first, then the shared lines by code and
// permission (permissions.ts). `nothing` says what was left as it was.
export function personRefusal(result: Refusal & { readonly retryAfter?: number }, nothing: string): string {
  if (result.failure !== 'network') {
    switch (result.code) {
      case 'principal_not_found':
        return `That person is gone, or you can no longer see them. ${nothing}`;
      case 'last_operator':
        return `The server keeps at least one operator. ${nothing}`;
      case 'principal_disabled':
        return `That person is disabled. Enable them first. ${nothing}`;
      case 'not_a_person':
        return `That does not apply to an agent. ${nothing}`;
      case 'guest_not_allowed':
        return `That does not apply to a guest. ${nothing}`;
      case 'channel_not_found':
        return `That channel is gone, or you can no longer see it. ${nothing}`;
      case 'rate_limited':
        return `Too many attempts; try again later. ${nothing}`;
      default:
        break;
    }
  }
  return refusalLine(result, nothing);
}

const roleWords: Readonly<Record<Role, string>> = { member: 'a member', operator: 'an operator', owner: 'an owner' };

const roleOf = (person: ServerMemberV1): Role => (person.owner === true ? 'owner' : person.role === 'operator' ? 'operator' : 'member');

const purposes: readonly { readonly value: SignInLinkPurpose; readonly label: string; readonly note: string }[] = [
  { value: 'sign_in', label: 'Sign in as them', note: 'Opening it once signs in as this person.' },
  { value: 'reset', label: 'Reset their password', note: 'Opening it once lets this person choose a new password.' },
  { value: 'enrollment', label: 'Set up their account', note: 'Opening it once lets this person set a password and sign in.' },
];

const clock = (at: string) => timeOf(at);

// The channel choices of the two forms that name a channel, read when one opens.
type Channels = readonly { readonly id: number; readonly name: string }[] | 'failed' | null;

export interface PersonActionsProps {
  readonly who: WhoAmIResponseV1;
  readonly person: ServerMemberV1;
  readonly origin: string;
  // The permissions document's rows, for the voice line; null if not read.
  readonly rows: readonly PermissionRowV1[] | null;
  // Asks for the document again; called when the menu opens with a voice item.
  readonly readRows: () => void;
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly button: RefObject<HTMLButtonElement | null>;
  // The community list is read again after a change, and told the line to
  // show (the row may be gone by then).
  readonly onChanged: (done?: string) => void;
  readonly onUnauthorized: () => void;
}

export function PersonActions({ who, person, origin, rows, readRows, open, onOpenChange, button, onChanged, onUnauthorized }: PersonActionsProps) {
  const actions = memberActions(who, person);
  // What the viewer may do now, for the moment a request is sent.
  const allowed = useRef<readonly MemberAction[]>(actions);
  allowed.current = actions;
  const name = person.name;
  const [panel, setPanel] = useState<Panel | null>(null);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line>(null);
  // The one secret just made, until Done.
  const [secret, setSecret] = useState<{ readonly id: MemberActionId; readonly label: string; readonly value: string; readonly note: string } | null>(null);
  const [channels, setChannels] = useState<Channels>(null);
  const sending = useRef(false);
  const live = useRef(true);
  const group = useRef<HTMLDivElement>(null);
  const channelReads = useRef(0);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);

  const panelId = panel === null ? null : panelAction(panel);
  const stillAllowed = panelId === null || actions.some((a) => a.id === panelId);
  // A grant that goes while a form or a question is open takes it away, and a
  // secret already made with it.
  useEffect(() => {
    if (!stillAllowed) {
      setPanel(null);
      setSecret(null);
      setLine({ text: 'You can no longer do that. Nothing was sent.', refused: true });
    }
  }, [stillAllowed]);
  const secretAllowed = secret === null || actions.some((a) => a.id === secret.id);
  useEffect(() => {
    if (!secretAllowed) setSecret(null);
  }, [secretAllowed]);

  // The panel takes focus: Cancel on a question, the first field of a form.
  useEffect(() => {
    if (panel === null) return;
    group.current?.querySelector<HTMLElement>(panel.kind === 'ask' ? 'button.cancel' : 'input, select')?.focus();
  }, [panel?.kind, panel?.kind === 'ask' ? panel.id : null]);

  // The channels the forms choose from are read when one opens.
  const wantsChannels = panel?.kind === 'channel' || panel?.kind === 'hook';
  useEffect(() => {
    if (!wantsChannels) return;
    const mine = ++channelReads.current;
    setChannels(null);
    // Every channel for a holder of manage_channels, who may list them all;
    // otherwise the viewer's own, the only ones they can bind a hook in.
    const read = canDo(who, 'manage_channels') ? api.channelDirectoryAll() : api.listChannels();
    void read.then((result) => {
      if (mine !== channelReads.current || !live.current) return;
      if (!result.ok && result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      setChannels(result.ok ? result.value : 'failed');
    });
  }, [wantsChannels]);

  const hasVoice = actions.some((a) => a.id === 'voice');
  const latestRead = useRef(readRows);
  latestRead.current = readRows;
  useEffect(() => {
    if (open && hasVoice) latestRead.current();
  }, [open, hasVoice]);

  const close = () => {
    setPanel(null);
    button.current?.focus();
  };

  // send makes one write, once, and says how it went. `done` is the line for
  // success; `then` gets the value the server answered with.
  function send<T>(id: MemberActionId, write: () => Promise<api.InviteResult<T>>, nothing: string, done: (value: T) => string, lost?: string, then?: (value: T) => void) {
    if (sending.current) return;
    if (!allowed.current.some((a) => a.id === id)) {
      setPanel(null);
      setLine({ text: 'You can no longer do that. Nothing was sent.', refused: true });
      return;
    }
    sending.current = true;
    setBusy(true);
    setLine(null);
    void write().then((result) => {
      sending.current = false;
      if (!result.ok && result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      // The row is gone (the grant went, or Settings closed): the answer
      // changes nothing here, and the list may follow.
      if (!live.current) {
        if (result.ok) onChanged(lost);
        return;
      }
      setBusy(false);
      if (result.ok) {
        setPanel(null);
        const text = done(result.value);
        setLine(null);
        then?.(result.value);
        // Focus goes back to the row's ⋯ button; if the row goes with the next
        // read, the list's status line takes it (Members.tsx).
        button.current?.focus();
        onChanged(text);
        return;
      }
      setLine({ text: personRefusal(result, nothing), refused: true });
      if (result.failure !== 'network' && (result.code === 'principal_not_found' || result.code === 'last_operator')) onChanged();
    });
  }

  const voiceOff = voiceRoleOff(person, rows);
  const items: MenuItem[] = actions.map((action) => {
    const item: MenuItem = { key: action.id, label: action.label, disabled: busy, onSelect: () => undefined };
    switch (action.id) {
      case 'voice': {
        if (voiceOff) return { ...item, disabled: true, reason: voiceOffLine(person) };
        const to = person.voice_allowed === false;
        return {
          ...item,
          onSelect: () => {
            send(
              'voice',
              () => api.setVoicePermission(person.id, to),
              'Voice was not changed.',
              (v) => `Voice is ${v.voice_allowed ? 'on' : 'off'} for ${name}.`,
            );
          },
        };
      }
      case 'invites': {
        const to = person.may_invite === false;
        return {
          ...item,
          onSelect: () => {
            send('invites', () => api.setInvitePermission(person.id, to), 'Member links were not changed.', () => `${name} ${to ? 'may make' : 'may no longer make'} member links.`);
          },
        };
      }
      case 'enable':
        return {
          ...item,
          onSelect: () => {
            send('enable', () => api.enablePrincipal(person.id), 'Nothing was changed.', () => `${name} may sign in.`);
          },
        };
      default: {
        const next = panelFor[action.id];
        return {
          ...item,
          onSelect: () => {
            setLine(null);
            setSecret(null);
            if (next !== undefined) setPanel(next);
          },
        };
      }
    }
  });

  function leave(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key !== 'Escape' || event.nativeEvent.isComposing) return;
    event.preventDefault();
    event.stopPropagation();
    close();
  }

  return (
    <>
      {items.length === 0 ? null : <ActionsMenu label={`Actions for ${name}`} items={items} open={open} onOpenChange={onOpenChange} button={button} />}
      {panel === null ? null : (
        <div ref={group} className="person-panel" onKeyDown={leave}>
          {panel.kind === 'ask' ? (
            <Ask
              id={panel.id}
              name={name}
              busy={busy}
              onYes={() => {
                if (panel.id === 'signout') send('signout', () => api.signOutPrincipalEverywhere(person.id), 'Nobody was signed out.', () => `${name} is signed out everywhere.`);
                else if (panel.id === 'disable') send('disable', () => api.disablePrincipal(person.id), 'Nothing was changed.', () => `${name} is disabled and signed out.`);
                else send('remove', () => api.hidePrincipal(person.id), 'Nobody was removed.', () => `${name} is removed from the server.`);
              }}
              onCancel={close}
            />
          ) : null}
          {panel.kind === 'role' ? (
            <RoleForm
              name={name}
              current={roleOf(person)}
              busy={busy}
              onCancel={close}
              onSubmit={(to) => {
                send(
                  'role',
                  () => api.setRole(person.id, to === 'member' ? { role: 'member' } : { role: 'operator', owner: to === 'owner' }),
                  'The role was not changed.',
                  (r) => {
                    const ended = r.credentials_revoked + r.hooks_revoked;
                    return `${name} is now ${roleWords[to]}.${ended > 0 ? ` ${String(r.credentials_revoked)} sign-ins and ${String(r.hooks_revoked)} hooks ended.` : ''}`;
                  },
                );
              }}
            />
          ) : null}
          {panel.kind === 'link' ? (
            <LinkForm
              name={name}
              busy={busy}
              onCancel={close}
              onSubmit={(purpose) => {
                send(
                  'link',
                  () => api.makeLinkFor(person.id, purpose),
                  'No link was made.',
                  (made) => `Link made. It works once, until ${clock(made.expiresAt)}.`,
                  `A link for ${name} was made but could not be shown. It works once and runs out on its own.`,
                  (made) => {
                    setSecret({
                      id: 'link',
                      label: `Link that signs in as ${name}`,
                      value: deviceLink(origin, made.secret),
                      note: purposes.find((p) => p.value === purpose)?.note ?? '',
                    });
                  },
                );
              }}
            />
          ) : null}
          {panel.kind === 'credential' ? (
            <LabelForm
              heading={`Make a credential that acts as ${name}`}
              note={`Anyone holding it acts as ${name} until it is revoked.`}
              submit="Make credential"
              required
              busy={busy}
              onCancel={close}
              onSubmit={(label) => {
                send(
                  'credential',
                  () => api.issueCredentialFor(person.id, label),
                  'No credential was made.',
                  () => 'Credential made.',
                  `A credential for ${name} was made but could not be shown; revoke it under Sign-ins.`,
                  (made) => {
                    setSecret({ id: 'credential', label: `Credential that acts as ${name}`, value: made.secret, note: '' });
                  },
                );
              }}
            />
          ) : null}
          {panel.kind === 'hook' ? (
            <LabelForm
              heading={`Make a hook that posts as ${name}`}
              note={`Anything sent to it appears in the channel as ${name}.`}
              submit="Make hook"
              channels={channels}
              busy={busy}
              onCancel={close}
              onSubmit={(label, channel) => {
                send(
                  'hook',
                  () => api.bindHookFor(channel, person.id, label),
                  'No hook was made.',
                  () => 'Hook made.',
                  `A hook for ${name} was made but could not be shown; revoke it in the permissions list.`,
                  (made) => {
                    setSecret({ id: 'hook', label: `Hook that posts as ${name}`, value: made.secret, note: '' });
                  },
                );
              }}
            />
          ) : null}
          {panel.kind === 'channel' ? (
            <ChannelForm
              name={name}
              channels={channels}
              busy={busy}
              onCancel={close}
              onSubmit={(channel) => {
                // The server's add-if-absent form: someone already in the channel is
                // left as they were, so their right to decide approvals is never
                // changed. A new member does not decide approvals.
                send(
                  'channel',
                  () => api.addChannelMemberById(channel, person.id, false),
                  'Nothing was sent.',
                  (result) => (result.added ? `${name} is in ${channel}.` : `${name} is already in that channel. Nothing changed.`),
                );
              }}
            />
          ) : null}
        </div>
      )}
      {secret === null ? null : (
        <div
          className="person-panel"
          onKeyDown={(event) => {
            // Escape must not close Settings and lose the secret: only Done ends it.
            if (event.key === 'Escape') {
              event.preventDefault();
              event.stopPropagation();
            }
          }}
        >
          {secret.note === '' ? null : <p className="hint">{secret.note}</p>}
          <SecretOnce
            label={secret.label}
            value={secret.value}
            onDone={() => {
              setSecret(null);
              button.current?.focus();
            }}
          />
        </div>
      )}
      {items.length === 0 && line === null ? null : (
        <p role="status" className={`member-line${line?.refused === true ? ' notice' : ''}`}>
          {line?.text ?? ''}
        </p>
      )}
    </>
  );
}

const questions: Readonly<Record<'signout' | 'disable' | 'remove', (name: string) => { readonly text: string; readonly yes: string }>> = {
  signout: (name) => ({ text: `Sign ${name} out everywhere? Every browser and app they use is signed out.`, yes: 'Yes, sign out' }),
  disable: (name) => ({ text: `Disable ${name}? They are signed out and cannot sign in until you enable them.`, yes: 'Yes, disable' }),
  remove: (name) => ({ text: `Remove ${name} from the server? They are signed out, leave every list and cannot use the server.`, yes: 'Yes, remove' }),
};

function Ask({ id, name, busy, onYes, onCancel }: { readonly id: 'signout' | 'disable' | 'remove'; readonly name: string; readonly busy: boolean; readonly onYes: () => void; readonly onCancel: () => void }) {
  const question = useId();
  const words = questions[id](name);
  return (
    <div role="group" aria-labelledby={question} className="member-confirm">
      <p id={question}>{words.text}</p>
      <div className="actions">
        <button type="button" disabled={busy} onClick={onYes}>
          {words.yes}
        </button>
        <button type="button" className="cancel" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}

function RoleForm({ name, current, busy, onCancel, onSubmit }: { readonly name: string; readonly current: Role; readonly busy: boolean; readonly onCancel: () => void; readonly onSubmit: (to: Role) => void }) {
  const id = useId();
  const [to, setTo] = useState<Role>(current);
  return (
    <form
      aria-label={`Change ${name}'s role`}
      className="person-form"
      onSubmit={(event: SyntheticEvent) => {
        event.preventDefault();
        if (to !== current) onSubmit(to);
      }}
    >
      <fieldset>
        <legend>{`${name}'s role`}</legend>
        {(['member', 'operator', 'owner'] as const).map((role) => (
          <label key={role} className="choice">
            <input
              type="radio"
              name={id}
              value={role}
              checked={to === role}
              onChange={() => {
                setTo(role);
              }}
            />
            {role === 'member' ? 'Member' : role === 'operator' ? 'Operator' : 'Owner'}
          </label>
        ))}
      </fieldset>
      {to === 'owner' ? <p className="notice">An owner holds every permission, is never braked, and only another owner can change or remove them.</p> : null}
      <p className="hint">A new role ends their sign-ins and hooks, and closes the links they made. They sign in again.</p>
      <div className="actions">
        <button type="submit" disabled={busy || to === current}>
          {to === 'owner' ? 'Make an owner, who holds every permission' : `Make ${roleWords[to]}`}
        </button>
        <button type="button" className="cancel" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function LinkForm({ name, busy, onCancel, onSubmit }: { readonly name: string; readonly busy: boolean; readonly onCancel: () => void; readonly onSubmit: (purpose: SignInLinkPurpose) => void }) {
  const id = useId();
  const [purpose, setPurpose] = useState<SignInLinkPurpose>('reset');
  return (
    <form
      aria-label={`Make a link that signs in as ${name}`}
      className="person-form"
      onSubmit={(event: SyntheticEvent) => {
        event.preventDefault();
        onSubmit(purpose);
      }}
    >
      <p>{`Make a link that signs in as ${name}. It works once, and it is recorded in the audit log.`}</p>
      <label htmlFor={`${id}-purpose`}>What the link does</label>
      <select
        id={`${id}-purpose`}
        value={purpose}
        onChange={(event) => {
          setPurpose(event.target.value as SignInLinkPurpose);
        }}
      >
        {purposes.map((p) => (
          <option key={p.value} value={p.value}>
            {p.label}
          </option>
        ))}
      </select>
      <div className="actions">
        <button type="submit" disabled={busy}>
          Make link
        </button>
        <button type="button" className="cancel" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

// LabelForm is the form of a credential (a label) and of a hook (a label and a
// channel, which it has when `channels` is given).
function LabelForm({
  heading,
  note,
  submit,
  required = false,
  channels,
  busy,
  onCancel,
  onSubmit,
}: {
  readonly heading: string;
  readonly note: string;
  readonly submit: string;
  readonly required?: boolean;
  readonly channels?: Channels;
  readonly busy: boolean;
  readonly onCancel: () => void;
  readonly onSubmit: (label: string, channel: string) => void;
}) {
  const id = useId();
  const [label, setLabel] = useState('');
  const [channel, setChannel] = useState('');
  const picking = channels !== undefined;
  const ready = (!required || label.trim() !== '') && (!picking || channel !== '');
  return (
    <form
      aria-label={heading}
      className="person-form"
      onSubmit={(event: SyntheticEvent) => {
        event.preventDefault();
        if (ready) onSubmit(label.trim(), channel);
      }}
    >
      <p>{heading}</p>
      <p className="hint">{note}</p>
      {picking ? <ChannelSelect id={`${id}-channel`} label="Channel" channels={channels} value={channel} onChange={setChannel} /> : null}
      <label htmlFor={`${id}-label`}>{required ? 'Name for it' : 'Name for it (optional)'}</label>
      <input
        id={`${id}-label`}
        type="text"
        value={label}
        maxLength={100}
        autoComplete="off"
        onChange={(event) => {
          setLabel(event.target.value);
        }}
      />
      <div className="actions">
        <button type="submit" disabled={busy || !ready}>
          {submit}
        </button>
        <button type="button" className="cancel" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function ChannelForm({ name, channels, busy, onCancel, onSubmit }: { readonly name: string; readonly channels: Channels; readonly busy: boolean; readonly onCancel: () => void; readonly onSubmit: (channel: string) => void }) {
  const id = useId();
  const [channel, setChannel] = useState('');
  return (
    <form
      aria-label={`Add ${name} to a channel`}
      className="person-form"
      onSubmit={(event: SyntheticEvent) => {
        event.preventDefault();
        if (channel !== '') onSubmit(channel);
      }}
    >
      <ChannelSelect id={`${id}-channel`} label={`Add ${name} to`} channels={channels} value={channel} onChange={setChannel} />
      <div className="actions">
        <button type="submit" disabled={busy || channel === ''}>
          Add to channel
        </button>
        <button type="button" className="cancel" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function ChannelSelect({ id, label, channels, value, onChange }: { readonly id: string; readonly label: string; readonly channels: Channels; readonly value: string; readonly onChange: (name: string) => void }) {
  return (
    <>
      <label htmlFor={id}>{label}</label>
      <select
        id={id}
        value={value}
        disabled={channels === null || channels === 'failed'}
        onChange={(event) => {
          onChange(event.target.value);
        }}
      >
        <option value="">{channels === null ? 'Reading the channels…' : 'Choose a channel'}</option>
        {channels === null || channels === 'failed'
          ? null
          : channels.map((c) => (
              <option key={c.id} value={c.name}>
                {c.name}
              </option>
            ))}
      </select>
      {channels === 'failed' ? <p className="notice">The channels could not be read.</p> : null}
    </>
  );
}
