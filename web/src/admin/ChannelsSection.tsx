// The console's Channels section (ADR-014 design §10.2, §10.3, §3.1, issue
// #1021): every channel on the server, and the channel administration a holder
// of manage_channels has. One row per channel: its name (a lock when private),
// whether it is a default channel, how many people are in it and who manages
// it. A row's ⋯ menu (the message menu's, also opened by right-click and a
// long press; on a phone a sheet from the bottom) offers:
//
//   - Add person, which on a personal server (the permissions document's
//     preset) is the first item: it is how the owner opens a new private
//     conversation;
//   - Rename channel;
//   - Make private or public, and Make or stop being a default channel
//     (PUT /v1/channels/{id}/settings, the confirmation of the channel list's
//     own menu, ChannelSettingsAsk);
//   - People and managers: remove a person (asked first), and give or take
//     away a person's manages.
//
// "New channel" makes a public or private channel with no daily cap for a
// holder of manage_channels (the server decides; the cap does not apply to
// them). The list is GET /v1/directory/channels?scope=all, which the server
// refuses to anyone else: the console omits the section for them. A channel
// manager without manage_channels manages their own channels from the channel
// itself, not from here.
//
// The words of every refusal are this page's own, chosen by the server's code
// and the permission it names; the server's message is never shown. Nothing
// here changes until the server has said yes, and the server decides every
// request again: hiding or disabling a control is not enforcement.
import { useCallback, useContext, useEffect, useId, useRef, useState, type RefObject, type SyntheticEvent } from 'react';
import * as api from '../api';
import { ChannelSettingsAsk, type SettingsChange } from '../ChannelSettingsAsk';
import { CreateChannel, nameRule, nameTaken } from '../CreateChannel';
import { PrivateMark } from '../Icons';
import { SettingsOpen } from '../joiningSettings';
import { usePhone } from '../phone';
import { ActionsMenu, useOpeners, type MenuItem } from '../MessageMenu';
import type { ChannelListingV1, ChannelMemberV1, ChannelV0 } from '../schema/channel';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { canDo, refusalLine, type Refusal } from './permissions';

type Line = { readonly text: string; readonly refused: boolean } | null;

// The people of a channel as the console last read them; null is a channel
// whose members could not be read.
type People = readonly ChannelMemberV1[] | null;

type Panel =
  | { readonly kind: 'add' }
  | { readonly kind: 'rename' }
  | { readonly kind: 'people' }
  | { readonly kind: 'settings'; readonly change: SettingsChange };

export const defaultMark = 'default channel';
export const phoneNote = 'Row actions open from the ⋯ button, or a long press.';

// channelRefusal is the page's own words for a channel write the server did
// not make: the codes of the member and rename routes first, then the shared
// lines by code and permission (permissions.ts). `nothing` says what was left
// as it was.
export function channelRefusal(result: Refusal, nothing: string): string {
  if (result.failure !== 'network') {
    switch (result.code) {
      case 'channel_not_found':
        return `That channel is gone, or you can no longer see it. ${nothing}`;
      case 'principal_not_found':
        return `Nobody on this server has that name, or they cannot be added to a channel. ${nothing}`;
      case 'channel_last_member':
        return `A private channel keeps at least one person. ${nothing}`;
      case 'channel_exists':
        return `${nameTaken}. ${nothing}`;
      case 'rate_limited':
        return `Too many refused attempts; try again later. ${nothing}`;
      case 'invalid_request':
        return `${nameRule} ${nothing}`;
      default:
        break;
    }
  }
  return refusalLine(result, nothing);
}

const peopleLine = (n: number) => (n === 1 ? '1 person' : `${String(n)} people`);

function managersOf(people: People | undefined): readonly string[] | null {
  if (people === undefined || people === null) return null;
  return people.filter((m) => m.manages === true).map((m) => m.name ?? `person ${String(m.principal_id)}`);
}

function Form({
  label,
  field,
  submit,
  initial,
  busy,
  onSubmit,
  onCancel,
  fieldRef,
}: {
  readonly label: string;
  readonly field: string;
  readonly submit: string;
  readonly initial: string;
  readonly busy: boolean;
  readonly onSubmit: (value: string) => void;
  readonly onCancel: () => void;
  readonly fieldRef: RefObject<HTMLInputElement | null>;
}) {
  const [value, setValue] = useState(initial);
  const id = useId();
  return (
    <form
      aria-label={label}
      onSubmit={(event: SyntheticEvent) => {
        event.preventDefault();
        if (value.trim() !== '') onSubmit(value);
      }}
    >
      <label htmlFor={id}>{field}</label>
      <input
        id={id}
        ref={fieldRef}
        type="text"
        autoComplete="off"
        value={value}
        readOnly={busy}
        onChange={(e) => {
          setValue(e.target.value);
        }}
      />
      <button type="submit" disabled={busy || value.trim() === ''}>
        {submit}
      </button>
      <button type="button" onClick={onCancel}>
        Cancel
      </button>
    </form>
  );
}

interface RowProps {
  readonly channel: ChannelListingV1;
  readonly people: People | undefined;
  readonly personal: boolean;
  readonly reload: (hint?: string) => void;
  readonly loadPeople: (channel: ChannelListingV1) => Promise<void>;
  readonly applied: (id: number, channel: ChannelV0) => void;
  readonly onUnauthorized: () => void;
}

function Row({ channel, people, personal, reload, loadPeople, applied, onUnauthorized }: RowProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  const [panel, setPanel] = useState<Panel | null>(null);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line>(null);
  // Removing a person is asked first, in place.
  const [removing, setRemoving] = useState<number | null>(null);
  const more = useRef<HTMLButtonElement>(null);
  const field = useRef<HTMLInputElement>(null);
  const sending = useRef(false);
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  useEffect(() => {
    if (panel?.kind === 'add' || panel?.kind === 'rename') field.current?.focus();
  }, [panel?.kind]);
  // The people a panel lists are read when it opens.
  useEffect(() => {
    if (panel?.kind === 'people') void loadPeople(channel);
    // Read once per opening; the row's own changes read them again.
  }, [panel?.kind]);

  const isPrivate = channel.visibility === 'private';
  const isDefault = channel.default === true;
  const close = () => {
    setPanel(null);
    setLine(null);
    setRemoving(null);
    more.current?.focus();
  };

  // run sends one write, once, and says how it went. `done` is the line for
  // success; `then` runs after the server has said yes.
  function run(write: () => Promise<api.InviteResult<unknown>>, nothing: string, done: string, then: () => void) {
    if (sending.current) return;
    sending.current = true;
    setBusy(true);
    setLine(null);
    void write().then((result) => {
      sending.current = false;
      if (!result.ok && result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      if (!live.current) {
        if (result.ok) then();
        return;
      }
      setBusy(false);
      if (result.ok) {
        setLine({ text: done, refused: false });
        then();
        return;
      }
      setLine({ text: channelRefusal(result, nothing), refused: true });
      // A channel that is gone, or no longer called what this page says, is
      // read again.
      if (result.code === 'channel_not_found' || result.code === 'channel_name_stale') reload();
    });
  }

  const items: MenuItem[] = [];
  const add: MenuItem = {
    key: 'add',
    label: 'Add person…',
    onSelect: () => {
      setLine(null);
      setPanel({ kind: 'add' });
    },
  };
  if (personal) items.push(add);
  items.push({
    key: 'rename',
    label: 'Rename channel…',
    onSelect: () => {
      setLine(null);
      setPanel({ kind: 'rename' });
    },
  });
  if (!personal) items.push(add);
  items.push({
    key: 'people',
    label: 'People and managers…',
    onSelect: () => {
      setLine(null);
      setRemoving(null);
      setPanel({ kind: 'people' });
    },
  });
  items.push({
    key: 'visibility',
    label: isPrivate ? 'Make public' : 'Make private',
    ...(!isPrivate && isDefault ? { disabled: true, reason: 'A default channel must be public. Stop it being a default channel first.' } : {}),
    onSelect: () => {
      setPanel({ kind: 'settings', change: { kind: 'visibility', to: isPrivate ? 'public' : 'private' } });
    },
  });
  items.push({
    key: 'default',
    label: isDefault ? 'Stop being a default channel' : 'Make default channel',
    ...(!isDefault && isPrivate ? { disabled: true, reason: 'A default channel must be public. Make it public first.' } : {}),
    onSelect: () => {
      setPanel({ kind: 'settings', change: { kind: 'default', to: !isDefault } });
    },
  });
  const openers = useOpeners(true, () => {
    setMenuOpen(true);
  });

  const managers = managersOf(people);
  const count = people === undefined || people === null ? channel.members : people.length;

  return (
    <li className={isPrivate ? 'private' : undefined} data-channel={channel.name} {...openers}>
      <div className="channel-row">
        <span className="channel-title">
          {isPrivate ? <PrivateMark /> : null}
          <strong>{channel.name}</strong>
          {isDefault ? <span className="ag-badge">{defaultMark}</span> : null}
        </span>
        <ActionsMenu label={`Actions for ${channel.name}`} items={items} open={menuOpen} onOpenChange={setMenuOpen} button={more} />
      </div>
      <p className="hint">
        {count === undefined ? 'People not counted' : peopleLine(count)}
        {' · '}
        {managers === null ? (people === null ? 'Managers could not be read' : 'Reading managers…') : managers.length === 0 ? 'No managers' : `Managed by ${managers.join(', ')}`}
      </p>
      {panel?.kind === 'add' ? (
        <div className="channel-panel" role="group" aria-label={`Add a person to ${channel.name}`}>
          <Form
            label="Add person"
            field="Their name"
            submit="Add person"
            initial=""
            busy={busy}
            fieldRef={field}
            onCancel={close}
            onSubmit={(name) => {
              run(
                () => api.addChannelMember(channel.name, name.trim()),
                'Nobody was added.',
                `Added ${name.trim()} to ${channel.name}.`,
                () => {
                  void loadPeople(channel);
                },
              );
            }}
          />
        </div>
      ) : null}
      {panel?.kind === 'rename' ? (
        <div className="channel-panel" role="group" aria-label={`Rename ${channel.name}`}>
          <Form
            label="Rename channel"
            field="New name"
            submit="Rename channel"
            initial={channel.name}
            busy={busy}
            fieldRef={field}
            onCancel={close}
            onSubmit={(name) => {
              if (name === channel.name) {
                setLine({ text: 'That is already the channel’s name.', refused: true });
                return;
              }
              run(
                () => api.renameChannel(channel.id, channel.name, name),
                'The name was not changed.',
                `Renamed to ${name}.`,
                () => {
                  reload();
                  setPanel(null);
                  more.current?.focus();
                },
              );
            }}
          />
        </div>
      ) : null}
      {panel?.kind === 'people' ? (
        <div className="channel-panel" role="group" aria-label={`People in ${channel.name}`}>
          {people === undefined ? <p className="hint">Reading the people…</p> : null}
          {people === null ? <p className="notice">The people could not be read.</p> : null}
          {people !== undefined && people !== null && people.length === 0 ? <p className="hint">Nobody is in this channel.</p> : null}
          <ul>
            {(people ?? []).map((m) => {
              const name = m.name ?? `person ${String(m.principal_id)}`;
              return (
                <li key={m.principal_id}>
                  <span>
                    {name}
                    {m.manages === true ? <span className="ag-badge manages">manages</span> : null}
                  </span>
                  <button
                    type="button"
                    disabled={busy}
                    aria-label={`${m.manages === true ? 'Stop' : 'Let'} ${name} ${m.manages === true ? 'managing' : 'manage'} ${channel.name}`}
                    onClick={() => {
                      run(
                        () => api.putChannelMember(channel.name, m.principal_id, { manages: m.manages !== true }),
                        'Nothing was changed.',
                        m.manages === true ? `${name} no longer manages ${channel.name}.` : `${name} now manages ${channel.name}.`,
                        () => {
                          void loadPeople(channel);
                        },
                      );
                    }}
                  >
                    {m.manages === true ? 'Stop managing' : 'Make manager'}
                  </button>
                  {removing === m.principal_id ? (
                    <span role="group" aria-label={`Remove ${name}`} className="signout-confirm">
                      <span>{`Remove ${name} from ${channel.name}?`}</span>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => {
                          run(
                            () => api.removeChannelMember(channel.name, m.principal_id),
                            'Nobody was removed.',
                            `Removed ${name} from ${channel.name}.`,
                            () => {
                              setRemoving(null);
                              void loadPeople(channel);
                            },
                          );
                        }}
                      >
                        {`Remove ${name}`}
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          setRemoving(null);
                        }}
                      >
                        Keep
                      </button>
                    </span>
                  ) : (
                    <button
                      type="button"
                      disabled={busy}
                      aria-label={`Remove ${name} from ${channel.name}`}
                      onClick={() => {
                        setLine(null);
                        setRemoving(m.principal_id);
                      }}
                    >
                      Remove…
                    </button>
                  )}
                </li>
              );
            })}
          </ul>
          <button type="button" onClick={close}>
            Close
          </button>
        </div>
      ) : null}
      {panel?.kind === 'settings' ? (
        <ChannelSettingsAsk
          key={`${panel.change.kind}:${String(panel.change.to)}`}
          channel={{ id: channel.id, name: channel.name, created_at: channel.created_at, visibility: channel.visibility, default: channel.default === true }}
          change={panel.change}
          onApplied={(next) => {
            applied(channel.id, next);
          }}
          onUnauthorized={onUnauthorized}
          onClose={close}
        />
      ) : null}
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </li>
  );
}

export interface ChannelsSectionProps {
  readonly who: WhoAmIResponseV1;
  // Counts the times the server said the permissions changed.
  readonly refresh: number;
  // The channel list the page keeps (the sidebar's) is read again after a
  // change made here.
  readonly onChannelsChanged: () => void;
  readonly onUnauthorized: () => void;
}

export function ChannelsSection({ who, refresh, onChannelsChanged, onUnauthorized }: ChannelsSectionProps) {
  const shown = useContext(SettingsOpen);
  const phone = usePhone();
  const [rows, setRows] = useState<readonly ChannelListingV1[] | null>(null);
  const [readLine, setReadLine] = useState<string | null>(null);
  const [personal, setPersonal] = useState(false);
  const [people, setPeople] = useState<ReadonlyMap<number, People>>(new Map());
  const reads = useRef(0);
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  const mayManage = canDo(who, 'manage_channels');

  const loadPeople = useCallback(
    async (channel: ChannelListingV1) => {
      const result = await api.listMembers(channel.name);
      if (!live.current) return;
      if (!result.ok && result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      setPeople((now) => new Map(now).set(channel.id, result.ok ? result.value : null));
    },
    [onUnauthorized],
  );

  // read asks again; `hint`, if given, is a line to show above the list.
  const read = useCallback(
    (hint?: string) => {
      const mine = ++reads.current;
      void (async () => {
        const result = await api.channelDirectoryAll();
        if (mine !== reads.current || !live.current) return;
        if (!result.ok) {
          if (result.failure === 'unauthorized') onUnauthorized();
          else if (result.code === 'permission_denied' || result.code === 'forbidden') setReadLine(refusalLine({ failure: result.failure, code: result.code }, 'No channel is shown.'));
          else setReadLine(result.failure === 'network' ? 'Cannot reach the server. The list may be out of date.' : 'The server could not list the channels.');
          return;
        }
        setReadLine(hint ?? null);
        setRows(result.value);
        setPeople(new Map());
        // The preset says whether Add person leads; a document that cannot be
        // read leaves the order as a group server's.
        void api.permissions().then((doc) => {
          if (mine === reads.current && live.current && doc.ok) setPersonal(doc.value.preset === 'personal');
        });
        // Who manages each channel: read a few at a time.
        const queue = [...result.value];
        const worker = async () => {
          for (let next = queue.shift(); next !== undefined; next = queue.shift()) {
            if (mine !== reads.current || !live.current) return;
            await loadPeople(next);
          }
        };
        await Promise.all([worker(), worker(), worker(), worker()]);
      })();
    },
    [loadPeople, onUnauthorized],
  );

  useEffect(() => {
    if (shown && mayManage) read();
  }, [shown, refresh, mayManage, read]);

  // The channels list of the page follows a change made here.
  const changed = useCallback(
    (hint?: string) => {
      onChannelsChanged();
      read(hint);
    },
    [onChannelsChanged, read],
  );

  if (!mayManage) return null;

  return (
    <section className="channels-section" aria-label="Channels">
      {phone ? null : <h4>Channels</h4>}
      <p className="hint">Every channel on this server, including private ones nobody here is in. {phoneNote}</p>
      {readLine === null ? null : (
        <p role="alert" className="notice">
          {readLine}
        </p>
      )}
      <CreateChannel
        onCreated={() => {
          changed();
        }}
        onUnauthorized={onUnauthorized}
      />
      {rows === null ? (
        <p className="hint" role="status">
          {readLine === null ? 'Reading the channels…' : ''}
        </p>
      ) : rows.length === 0 ? (
        <p className="hint">There are no channels yet.</p>
      ) : (
        <ul aria-label="Every channel" className="channels-admin-list">
          {rows.map((channel) => (
            <Row
              key={channel.id}
              channel={channel}
              people={people.get(channel.id)}
              personal={personal}
              reload={changed}
              loadPeople={loadPeople}
              applied={(id, next) => {
                setRows((now) => now?.map((r) => (r.id === id ? { ...r, visibility: next.visibility ?? r.visibility, default: next.default === true } : r)) ?? now);
                onChannelsChanged();
              }}
              onUnauthorized={onUnauthorized}
            />
          ))}
        </ul>
      )}
    </section>
  );
}
