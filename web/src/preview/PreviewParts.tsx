// What the shell draws for a preview (issue #1517,
// docs/design/permission-preview.md §5, §7.2). Each part reads the preview
// store and nothing else: not the administrator's channels, chat, people or
// mention candidates. Nothing here sends a request, opens a socket, joins
// voice or marks anything read, and every control is shown, not wired.
import { Avatar } from '../Icons';
import { Composer } from '../Composer';
import { headingId } from '../Conversation';
import { memberName } from '../guestName';
import { memberActions } from '../memberActions';
import type { Target } from '../state/drafts';
import type { Person } from '../useLiveChannel';
import type { ChannelMemberV1 } from '../schema/generated';
import type { ServerMemberV1 } from '../schema/principal';
import { previewWho, type PreviewStore } from './store';
import { controlLines, messagesPlaceholder, readOnlyLine, voicePreviewNote } from './words';

const noNets: ReadonlyMap<number, string> = new Map();
const noneMonitored: ReadonlySet<number> = new Set();
const nothing = (): void => undefined;
const wholeChannel: Target = { net: null, whisper: null, refusal: null, note: null };

const nameOf = (member: ChannelMemberV1): string => memberName(member) ?? `#${String(member.principal_id)}`;

// The conversation pane: a placeholder for the messages, and the composer
// disabled. Its people are the preview's member list of this channel, for the
// whisper and mention candidates a composer offers.
export function PreviewConversation({ store, channel }: { readonly store: PreviewStore; readonly channel: string | null }) {
  const members = channel === null ? undefined : store.members.get(channel);
  const people = new Map<number, Person>();
  for (const m of members ?? []) people.set(m.principal_id, { name: nameOf(m), kind: m.kind ?? null });
  return (
    <div className="preview-conversation">
      <h2 id={headingId} tabIndex={-1}>
        {channel === null ? 'No channel' : `#${channel}`}
      </h2>
      <p className="preview-placeholder">{messagesPlaceholder}</p>
      {channel === null ? null : (
        <Composer
          channel={channel}
          target={wholeChannel}
          nets={noNets}
          monitored={noneMonitored}
          people={people}
          viewer={store.target.id}
          draft=""
          enabled={false}
          paused={false}
          onDraft={nothing}
          onTarget={nothing}
          onSend={nothing}
          atBottom={nothing}
        />
      )}
      <p role="status" className="hint">
        {readOnlyLine}
      </p>
    </div>
  );
}

// The strip in place of voice: what voice would allow the previewed person
// here, and no way to connect.
export function PreviewVoice({ store, channel }: { readonly store: PreviewStore; readonly channel: string | null }) {
  const voice = controlLines(previewWho(store.target)).find((line) => line.label === 'Voice');
  return (
    <section aria-label="Voice preview" className="preview-voice">
      <h3>Voice</h3>
      <p>{voice === undefined ? '' : channel === null ? voice.reason : `${voice.reason.replace(/ here\.$/, '')} in #${channel}.`}</p>
      <button type="button" disabled>
        Join voice
      </button>
      <p className="hint">{voicePreviewNote}</p>
    </section>
  );
}

// The member panel and the community list as the previewed person sees them:
// one group, no online state.
export function PreviewPanel({ store, channel }: { readonly store: PreviewStore; readonly channel: string | null }) {
  const who = previewWho(store.target);
  const members = channel === null ? undefined : store.members.get(channel);
  const byId = new Map<number, ServerMemberV1>();
  for (const m of store.directory.members) byId.set(m.id, m);
  return (
    <>
      <section aria-label="Channel members" className="preview-people">
        <h3>{channel === null ? 'Members' : `Members of #${channel}`}</h3>
        {members === undefined ? (
          <p className="empty">Nobody to show</p>
        ) : (
          <ul aria-label="Members">
            {members.map((m) => (
              <li key={m.principal_id} className="community-member">
                <Avatar id={m.principal_id} name={nameOf(m)} />
                <span className="community-name">{nameOf(m)}</span>
                {m.kind === 'agent' ? <span className="ag-badge">Agent</span> : null}
                {m.manages === true ? <span className="ag-badge">Manager</span> : null}
                {m.role === undefined ? null : <span className="ag-badge">{m.role === 'operator' ? 'Operator' : 'Member'}</span>}
                {m.can_decide === true ? <span className="ag-badge">Decides</span> : null}
              </li>
            ))}
          </ul>
        )}
      </section>
      <section aria-label="Community members" className="preview-people community-members">
        <h3>Community members</h3>
        {store.directory.scope === 'shared_channels' ? <p className="hint">You see the people you share a channel with</p> : null}
        {store.directory.members.length === 0 ? (
          <p className="empty">Nobody to show</p>
        ) : (
          <ul aria-label="People">
            {store.directory.members.map((person) => {
              const offered = memberActions(who, person);
              return (
                <li key={person.id} className="community-member">
                  <Avatar id={person.id} name={person.name} />
                  <span className="community-name">{person.name}</span>
                  {person.kind === 'agent' ? <span className="ag-badge">Agent</span> : null}
                  {person.kind === 'human' && person.role !== undefined ? <span className="ag-badge">{person.role === 'operator' ? 'Operator' : 'Member'}</span> : null}
                  {person.owner === true ? <span className="ag-badge">Owner</span> : null}
                  {offered.length === 0 ? null : <span className="hint">{` · ${store.target.name} would be offered: ${offered.map((a) => a.label).join(', ')}`}</span>}
                </li>
              );
            })}
          </ul>
        )}
      </section>
    </>
  );
}

// The controls the previewed person holds, under the channel list: what they
// could do, and a line for each they could not. The channel directory is what
// they would browse.
export function PreviewControls({ store }: { readonly store: PreviewStore }) {
  const who = previewWho(store.target);
  const managed = store.channels.filter((c) => c.manages === true);
  return (
    <>
      <section aria-label="Controls" className="preview-controls">
        <h3>What {store.target.name} can do</h3>
        <ul>
          {controlLines(who).map((line) => (
            <li key={line.label}>
              <button type="button" disabled>
                {line.label}
              </button>{' '}
              <span className="hint">{line.available ? line.reason : `Not available: ${line.reason}`}</span>
            </li>
          ))}
          {managed.length === 0 ? null : (
            <li>
              <span className="hint">{`${store.target.name} manages ${managed.map((c) => `#${c.name}`).join(', ')}.`}</span>
            </li>
          )}
        </ul>
      </section>
      <section aria-label="Channel directory" className="preview-controls">
        <h3>Browse channels</h3>
        {store.channelDirectory.length === 0 ? (
          <p className="empty">Nothing to join</p>
        ) : (
          <ul>
            {store.channelDirectory.map((c) => (
              <li key={c.id}>
                {`#${c.name}`} <span className="hint">{`· ${c.visibility}${c.joined ? ' · joined' : ''}`}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </>
  );
}
