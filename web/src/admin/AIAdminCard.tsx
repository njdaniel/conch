// The agent card (issues #1334 and #1562, docs/design/ai-administrators.md §4.2
// and §4.5): under an agent in the Members section, what an AI administrator
// holds ("AI administrator: manage members, manage channels") and Remove
// access, which disables the principal: its credentials, sessions and sockets
// end. Read from the agent's manifest, which operators may read; an agent with
// no grant draws nothing. The server decides every request again.
import { createContext, useContext, useEffect, useRef, useState } from 'react';
import * as api from '../api';
import type { ServerMemberV1, WhoAmIResponseV1 } from '../schema/principal';
import { canDo, permissionName } from './permissions';
import { personRefusal } from './PersonActions';

// A community-directory request as the session numbers it: ticket orders it
// against every other request, generation is the count of directory changes
// the page had been told of when it was sent.
export interface DirectoryRequest {
  readonly ticket: number;
  readonly generation: number;
}

// ManifestReads is the one gate for agent manifest reads, shared by every
// card (#1562). The model: once any community-directory answer has been
// applied, an agent is read only if the newest applied answer lists it and no
// manifest read has answered 404 for it; before the first answer, reads are
// allowed. Only the community directory decides membership; channel members
// play no part. Every directory answer, from any view and even a closed one,
// is ordered by its ticket: one older than an answer already applied changes
// nothing here (each view still draws its own). An answer sent after a
// directory change (a newer generation than any applied) also lifts the 404
// stops of the agents it lists, and a 404 sent before that is ignored.
export interface ManifestReadControl {
  readonly mayRead: (id: number) => boolean;
  // generation is the count of directory changes seen now: tag a read with it.
  readonly generation: () => number;
  // stop records a 404 for a read sent at that generation, whatever became of the card.
  readonly stop: (id: number, generation: number) => void;
  readonly beginDirectory: () => DirectoryRequest;
  // acceptDirectory applies the answer to that request; false means it was stale.
  readonly acceptDirectory: (request: DirectoryRequest, members: readonly ServerMemberV1[]) => boolean;
}

export const ManifestReads = createContext<ManifestReadControl>({ mayRead: () => true, generation: () => 0, stop: () => undefined, beginDirectory: () => ({ ticket: 0, generation: 0 }), acceptDirectory: () => true });

export interface AIAdminCardProps {
  readonly who: WhoAmIResponseV1;
  readonly person: ServerMemberV1;
  readonly onChanged: (done?: string) => void;
  readonly onUnauthorized: () => void;
}

export function AIAdminCard({ who, person, onChanged, onUnauthorized }: AIAdminCardProps) {
  const manifest = useContext(ManifestReads);
  const reads = who.role === 'operator' && person.kind === 'agent' && manifest.mayRead(person.id);
  const [grant, setGrant] = useState<readonly string[]>([]);
  const [asking, setAsking] = useState(false);
  // Access was removed from this card. The server clears the grant when it
  // disables the agent (#1329), so a later read of the manifest finds none;
  // until then, and whatever order the list and the manifest are read in,
  // the card says the access was removed and offers no second removal.
  const [removed, setRemoved] = useState(false);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  const live = useRef(true);
  const unauthorized = useRef(onUnauthorized);
  unauthorized.current = onUnauthorized;
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  useEffect(() => {
    if (!reads) return;
    let current = true;
    const sent = manifest.generation();
    void api.agentManifest(person.id).then((result) => {
      // A 404 is the session's, kept whatever became of this card; the rest is local.
      if (!result.ok && result.failure !== 'unauthorized' && result.status === 404) manifest.stop(person.id, sent);
      if (!current) return;
      if (result.ok) setGrant(result.value.permissions ?? []);
      else if (result.failure === 'unauthorized') unauthorized.current();
      else setGrant([]);
    });
    return () => {
      current = false;
    };
  // Read again whenever the list is (person is a new object each time), so a
  // grant made or removed since shows, unless the directory or a 404 ended it.
  }, [reads, person, manifest.stop, manifest.generation]);

  if (!reads || (grant.length === 0 && !removed)) return null;
  const name = person.name;
  const may = canDo(who, 'manage_members');

  function remove() {
    if (busy) return;
    setBusy(true);
    setLine(null);
    void api.disablePrincipal(person.id).then((result) => {
      if (!result.ok && result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      if (!live.current) {
        if (result.ok) onChanged();
        return;
      }
      setBusy(false);
      if (result.ok) {
        setAsking(false);
        setRemoved(true);
        onChanged(`${name} has no access now: its credentials, sessions and connections ended.`);
        return;
      }
      setLine(personRefusal(result, 'Its access is unchanged.'));
    });
  }

  return (
    <div className="person-panel ai-admin-card" role="group" aria-label={`AI administrator ${name}`}>
      {removed ? (
        <p>{`${person.name} has no access now: its credentials, sessions and connections ended.`}</p>
      ) : (
        <p>{`AI administrator: ${grant.map(permissionName).join(', ')}`}</p>
      )}
      {may && !removed ? (
        asking ? (
          <div role="group" aria-label={`Remove access from ${name}`} className="member-confirm">
            <p>{`Remove ${name}'s access? Its credentials and connections end at once. You can enable it again later.`}</p>
            <div className="actions">
              <button type="button" disabled={busy} onClick={remove}>
                Yes, remove access
              </button>
              <button
                type="button"
                className="cancel"
                disabled={busy}
                onClick={() => {
                  setAsking(false);
                }}
              >
                Cancel
              </button>
            </div>
          </div>
        ) : (
          <button
            type="button"
            onClick={() => {
              setLine(null);
              setAsking(true);
            }}
          >
            Remove access
          </button>
        )
      ) : null}
      {line === null ? null : (
        <p role="status" className="notice">
          {line}
        </p>
      )}
    </div>
  );
}
