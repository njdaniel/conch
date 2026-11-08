// The session's one stop for agent manifest reads (issue #1562): which agents
// the community directory no longer lists, which answered 404, and the
// session-wide generation that orders every directory answer.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ServerMemberV1 } from '../schema/principal';
import type { DirectoryRequest, ManifestReadControl } from './AIAdminCard';

// useManifestReadControl builds the control App provides to every agent card.
// changes counts the server's directory-changed words; signedIn false clears
// the control, so another person's session starts clean.
export function useManifestReadControl(signedIn: boolean, changes: number): ManifestReadControl {
  const [unlistedAgents, setUnlistedAgents] = useState<ReadonlySet<number>>(new Set());
  const [missingManifests, setMissingManifests] = useState<ReadonlySet<number>>(new Set());
  const listedAgents = useRef<ReadonlySet<number>>(new Set());
  // Tickets number every directory request from every view, closed ones
  // included; an answer older than the newest one applied is stale.
  const requestedDirectory = useRef(0);
  const acceptedDirectory = useRef(0);
  // The directory changes seen so far, and the newest generation whose
  // answer has lifted the 404 stops.
  const generation = useRef(changes);
  generation.current = changes;
  const recovered = useRef(changes);
  const beginDirectory = useCallback((): DirectoryRequest => ({ ticket: ++requestedDirectory.current, generation: generation.current }), []);
  // Only an accepted community-directory answer decides membership here; the
  // open channel's members are a different list.
  const acceptDirectory = useCallback((request: DirectoryRequest, next: readonly ServerMemberV1[]) => {
    if (request.ticket < acceptedDirectory.current) return false;
    acceptedDirectory.current = request.ticket;
    const before = listedAgents.current;
    const present = new Set(next.filter((member) => member.kind === 'agent').map((member) => member.id));
    listedAgents.current = present;
    setUnlistedAgents((current) => {
      const changed = new Set(current);
      for (const id of before) if (!present.has(id)) changed.add(id);
      for (const id of present) changed.delete(id);
      return changed.size === current.size && [...changed].every((id) => current.has(id)) ? current : changed;
    });
    if (request.generation <= recovered.current) return true;
    recovered.current = request.generation;
    setMissingManifests((current) => {
      const changed = new Set(current);
      for (const id of present) changed.delete(id);
      return changed.size === current.size ? current : changed;
    });
    return true;
  }, []);
  const stopManifestRead = useCallback((id: number) => {
    setMissingManifests((current) => {
      if (current.has(id)) return current;
      const changed = new Set(current);
      changed.add(id);
      return changed;
    });
  }, []);
  useEffect(() => {
    if (signedIn) return;
    listedAgents.current = new Set();
    acceptedDirectory.current = requestedDirectory.current;
    setUnlistedAgents(new Set());
    setMissingManifests(new Set());
  }, [signedIn]);
  // One stop applies to both directory views. A routine reopen does not retry
  // a 404; a post-change directory answer listing the agent does.
  return useMemo<ManifestReadControl>(
    () => ({ stopped: new Set([...unlistedAgents, ...missingManifests]), stop: stopManifestRead, beginDirectory, acceptDirectory }),
    [unlistedAgents, missingManifests, stopManifestRead, beginDirectory, acceptDirectory],
  );
}
