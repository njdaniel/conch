// Add an AI administrator (issue #1334, docs/design/ai-administrators.md §4.1
// and §4.2): the owner's action in the Members section. It composes the same
// seven steps as `conch admin ai-admin create`, through the public routes
// alone, as the signed-in owner:
//
//   1 make the agent        2 find or make the private approval channel
//   3 the owner decides there   4 the agent only watches there
//   5 the manifest (the grant, owner-only on the server)
//   6 a renewable credential    7 show the MCP entry once (SecretOnce.tsx)
//
// Whatever fails after step 1 disables the new principal, so no half-made
// agent is left holding anything. The button is the owner's alone; the server
// refuses the grant to anyone else, too. The credential is in memory from the
// moment the server answers until Done, and nowhere else.
import { useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import * as api from '../api';
import { isOwner } from '../memberActions';
import type { PutAgentManifestRequestV1 } from '../schema/admin';
import type { Permission } from '../schema/permission';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { permissionName } from './permissions';
import { personRefusal } from './PersonActions';
import { SecretOnce } from './SecretOnce';

const defaultChannel = 'admin';
const defaultDays = 90;
// The renewable credential's idle lifetime the server accepts, in days
// (schema MinCredentialRenewLifetimeSeconds and its maximum, a year).
const minDays = 2;
const maxDays = 365;
const secondsPerDay = 24 * 60 * 60;
// The admin.act limit the manifest carries (design §4.1): 60 an hour.
const rateMax = 60;
const rateWindow = 3600;

type Grantable = 'manage_members' | 'manage_channels' | 'make_member_links' | 'change_server_settings' | 'read_audit_log';
const usual: readonly Grantable[] = ['manage_members', 'manage_channels', 'make_member_links'];
// Off unless chosen, and under a warning.
const weighty: readonly Grantable[] = ['change_server_settings', 'read_audit_log'];

type Client = 'json' | 'claude-code';

// entryName is the MCP client entry's name (design §4.4): "conch-" and the
// server's name lowercased, anything but [a-z0-9-] replaced by "-".
export function entryName(serverName: string): string {
  const base = serverName === '' ? 'conch' : serverName;
  return `conch-${base.toLowerCase().replace(/[^a-z0-9-]/g, '-')}`;
}

// shellWord quotes s for a POSIX shell only when it has to be.
function shellWord(s: string): string {
  if (/^[A-Za-z0-9\-_./:@%+=]+$/.test(s)) return s;
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

// entryText is the snippet shown once: the JSON Claude Code reads from
// .mcp.json, or the one-line `claude mcp add` form.
export function entryText(client: Client, name: string, url: string, token: string): string {
  const header = `Bearer ${token}`;
  if (client === 'claude-code') return `claude mcp add --transport http --scope user ${shellWord(name)} ${shellWord(url)} --header ${shellWord(`Authorization: ${header}`)}`;
  return JSON.stringify({ mcpServers: { [name]: { type: 'http', url, headers: { Authorization: header } } } }, null, 2);
}

// manifestFor is the manifest the grant writes (design §4.1 step 4).
export function manifestFor(name: string, channel: number, grant: readonly Permission[]): PutAgentManifestRequestV1 {
  return {
    display_name: name,
    tier: 'A',
    capabilities: ['messages.read', 'messages.post', 'approvals.await', 'approvals.check', 'admin.act'],
    channels: [{ channel_id: channel, permissions: ['read', 'post'] }],
    rate_limits: [{ capability: 'admin.act', max: rateMax, window_seconds: rateWindow }],
    permissions: [...grant],
    admin: { approval_channel_id: channel },
  };
}

export interface AddAIAdminProps {
  readonly who: WhoAmIResponseV1;
  readonly origin: string;
  // The community list is read again after an agent was made.
  readonly onChanged: () => void;
  readonly onUnauthorized: () => void;
}

type Stage = 'closed' | 'form' | 'working' | 'shown';

export function AddAIAdmin({ who, origin, onChanged, onUnauthorized }: AddAIAdminProps) {
  const ids = useId();
  const [stage, setStage] = useState<Stage>('closed');
  const [name, setName] = useState('');
  const [channel, setChannel] = useState(defaultChannel);
  const [grant, setGrant] = useState<ReadonlySet<Grantable>>(new Set(usual));
  const [days, setDays] = useState(String(defaultDays));
  const [client, setClient] = useState<Client>('json');
  const [line, setLine] = useState<string | null>(null);
  const [step, setStep] = useState('');
  // The approval channel was found, not made (security review of #1344):
  // it keeps the deciders it had, which the entry panel says.
  const [found, setFound] = useState<'public' | 'private' | null>(null);
  // The entry, until Done.
  const [entry, setEntry] = useState<{ readonly label: string; readonly value: string } | null>(null);
  const live = useRef(true);
  const opener = useRef<HTMLButtonElement>(null);
  const first = useRef<HTMLInputElement>(null);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  useEffect(() => {
    if (stage === 'form') first.current?.focus();
  }, [stage]);

  // Only the owner is offered this; the server decides again.
  if (!isOwner(who)) return null;

  const dayCount = Number(days);
  const daysOk = Number.isInteger(dayCount) && dayCount >= minDays && dayCount <= maxDays;
  const ready = name.trim() !== '' && channel.trim() !== '' && grant.size > 0 && daysOk;

  function reset() {
    setStage('closed');
    setName('');
    setChannel(defaultChannel);
    setGrant(new Set(usual));
    setDays(String(defaultDays));
    setLine(null);
    setStep('');
    setEntry(null);
    setFound(null);
    opener.current?.focus();
  }

  function toggle(permission: Grantable) {
    const next = new Set(grant);
    if (next.has(permission)) next.delete(permission);
    else next.add(permission);
    setGrant(next);
  }

  async function create(event: SyntheticEvent) {
    event.preventDefault();
    if (stage !== 'form' || !ready) return;
    const agentName = name.trim();
    const channelName = channel.trim();
    const granted = [...usual, ...weighty].filter((p) => grant.has(p));
    setStage('working');
    setLine(null);

    // Step 1: the agent.
    setStep('Making the agent…');
    const made = await api.createAgent(agentName);
    if (!made.ok) {
      if (made.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      if (!live.current) return;
      setLine(personRefusal(made, 'Nothing was made.'));
      setStage('form');
      return;
    }
    const agent = made.value;
    onChanged();

    // Whatever fails from here on disables the agent, so nothing half-made
    // holds access.
    const fail = async (what: string, cause: api.InviteRefusal): Promise<void> => {
      const undone = await api.disablePrincipal(agent.id);
      if (cause.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      onChanged();
      if (!live.current) return;
      const why = personRefusal(cause, '');
      setLine(
        undone.ok
          ? `${what} failed: ${why.trim()} The new agent ${agentName} was disabled, so nothing is left holding access.`
          : `${what} failed: ${why.trim()} Disabling the new agent ${agentName} (#${String(agent.id)}) failed too: disable it from its menu.`,
      );
      setStep('');
      setStage('form');
    };
    try {
      // Step 2: the approval channel, private if absent.
      setStep('Finding the approval channel…');
      const listed = await api.channelDirectoryAll();
      if (!listed.ok) {
        await fail('Reading the channels', listed);
        return;
      }
      const existing = listed.value.find((c) => c.name === channelName);
      let channelId = existing?.id;
      setFound(existing === undefined ? null : existing.visibility === 'public' ? 'public' : 'private');
      if (channelId === undefined) {
        const created = await api.createChannel(channelName, 'private');
        if (!created.ok) {
        await fail('Making the approval channel', created);
        return;
      }
        channelId = created.value.id;
      }
      // Step 3: the owner decides there, the agent only watches.
      setStep('Seating you and the agent…');
      const decider = await api.putChannelMember(channelName, who.id, { can_decide: true });
      if (!decider.ok) {
        await fail('Seating you as a decider', decider);
        return;
      }
      const watcher = await api.putChannelMember(channelName, agent.id, { can_decide: false });
      if (!watcher.ok) {
        await fail('Adding the agent to the approval channel', watcher);
        return;
      }
      // Step 4: the manifest; a non-owner is refused here, by the server.
      setStep('Writing the grant…');
      const written = await api.putAgentManifest(agent.id, manifestFor(agentName, channelId, granted));
      if (!written.ok) {
        await fail('Writing the grant', written);
        return;
      }
      // Step 5: a renewable credential, shown once.
      setStep('Making the credential…');
      const credential = await api.issueCredentialFor(agent.id, 'ai-admin', dayCount * secondsPerDay);
      if (!credential.ok) {
        await fail('Making the credential', credential);
        return;
      }
      // Step 6: the entry, from the server's name.
      const info = await api.joinSettings();
      const serverName = info.ok ? info.value.server_name : '';
      const url = new URL('/mcp', origin).toString();
      const text = entryText(client, entryName(serverName), url, credential.value.secret);
      onChanged();
      if (!live.current) {
        // The panel closed before the entry could be shown: nobody holds
        // the credential, so the agent is disabled rather than left live
        // (security review of #1344).
        await api.disablePrincipal(agent.id);
        onChanged();
        return;
      }
      setEntry({ label: `MCP entry for ${agentName}`, value: text });
      setStep('');
      setStage('shown');
    } catch {
      // Nothing here throws what the server said; a surprise is still undone.
      await fail('Adding the AI administrator', { ok: false, failure: 'network' });
    }
  }

  return (
    <section aria-label="Add an AI administrator" className="person-form add-ai-admin">
      <button
        ref={opener}
        type="button"
        aria-expanded={stage !== 'closed'}
        disabled={stage === 'working' || stage === 'shown'}
        onClick={() => {
          if (stage === 'closed') setStage('form');
          else reset();
        }}
      >
        Add an AI administrator
      </button>
      {stage === 'form' || stage === 'working' ? (
        <form
          aria-label="New AI administrator"
          className="person-panel"
          onSubmit={(event) => void create(event)}
          onKeyDown={(event) => {
            if (event.key === 'Escape' && stage === 'form') {
              event.preventDefault();
              event.stopPropagation();
              reset();
            }
          }}
        >
          <p className="hint">An agent that administers this server over MCP with the permissions you pick. What it does with the riskier ones is asked of you first.</p>
          <p>
            <label htmlFor={`${ids}-name`}>Name</label>
            <input ref={first} id={`${ids}-name`} type="text" autoComplete="off" value={name} readOnly={stage === 'working'} onChange={(event) => { setName(event.target.value); }} />
          </p>
          <p>
            <label htmlFor={`${ids}-channel`}>Approval channel</label>
            <input id={`${ids}-channel`} type="text" autoComplete="off" value={channel} readOnly={stage === 'working'} onChange={(event) => { setChannel(event.target.value); }} />
          </p>
          <p className="hint">Its requests wait here for you to decide. It is made private if it does not exist.</p>
          <fieldset disabled={stage === 'working'}>
            <legend>What it may do</legend>
            {usual.map((p) => (
              <label key={p} className="choice">
                <input type="checkbox" checked={grant.has(p)} onChange={() => { toggle(p); }} />
                {`May ${permissionName(p)}`}
              </label>
            ))}
            <p className="notice">These two are powerful; give them only if you mean to.</p>
            {weighty.map((p) => (
              <label key={p} className="choice">
                <input type="checkbox" checked={grant.has(p)} onChange={() => { toggle(p); }} />
                {`May ${permissionName(p)}`}
              </label>
            ))}
          </fieldset>
          <p>
            <label htmlFor={`${ids}-days`}>Its credential lasts, unused, for (days)</label>
            <input id={`${ids}-days`} type="number" min={minDays} max={maxDays} value={days} readOnly={stage === 'working'} onChange={(event) => { setDays(event.target.value); }} />
          </p>
          <p>
            <label htmlFor={`${ids}-client`}>Show the entry as</label>
            <select id={`${ids}-client`} value={client} disabled={stage === 'working'} onChange={(event) => { setClient(event.target.value === 'claude-code' ? 'claude-code' : 'json'); }}>
              <option value="json">JSON for .mcp.json</option>
              <option value="claude-code">A claude mcp add command</option>
            </select>
          </p>
          <div className="actions">
            <button type="submit" disabled={!ready || stage === 'working'}>
              Add AI administrator
            </button>
            <button type="button" disabled={stage === 'working'} onClick={reset}>
              Cancel
            </button>
          </div>
        </form>
      ) : null}
      {stage === 'shown' && entry !== null ? (
        <div
          className="person-panel"
          onKeyDown={(event) => {
            // Escape must not close Settings and lose the entry: only Done ends it.
            if (event.key === 'Escape') {
              event.preventDefault();
              event.stopPropagation();
            }
          }}
        >
          <p className="hint">The AI administrator is ready. Put this entry in its MCP client now: it is shown once and holds its credential.</p>
          {found === null ? null : (
            <p className="notice">
              {`#${channel.trim()} already existed: you and everyone who already decides there decide this agent's requests${found === 'public' ? ', and it is public, so everyone can read them' : ''}. Check who decides in its member list.`}
            </p>
          )}
          <SecretOnce label={entry.label} value={entry.value} onDone={reset} />
        </div>
      ) : null}
      <p role="status" className={line === null ? 'hint' : 'notice'}>
        {line ?? step}
      </p>
    </section>
  );
}
