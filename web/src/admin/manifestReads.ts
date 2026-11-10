// The session's one gate for agent manifest reads (issue #1562): the agents
// the newest applied community-directory answer lists, those that answered
// 404, and the session-wide order of every directory answer.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ServerMemberV1 } from '../schema/principal';
import type { DirectoryRequest, ManifestReadControl } from './AIAdminCard';

// useManifestReadControl builds the control App provides to every agent card.
// changes counts the server's directory-changed words; signedIn false clears
// the control, so another person's session starts clean.
export function useManifestReadControl(signedIn: boolean, changes: number): ManifestReadControl {
  // The agents the newest applied answer lists; null before any.
  const [listed, setListed] = useState<ReadonlySet<number> | null>(null);
  const [missingManifests, setMissingManifests] = useState<ReadonlySet<number>>(new Set());
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
    const present = new Set(next.filter((member) => member.kind === 'agent').map((member) => member.id));
    setListed(present);
    if (request.generation <= recovered.current) return true;
    recovered.current = request.generation;
    setMissingManifests((current) => {
      const changed = new Set(current);
      for (const id of present) changed.delete(id);
      return changed.size === current.size ? current : changed;
    });
    return true;
  }, []);
  const generationNow = useCallback(() => generation.current, []);
  const stopManifestRead = useCallback((id: number, sent: number) => {
    // A recovery newer than this read has already cleared the stops.
    if (sent < recovered.current) return;
    setMissingManifests((current) => {
      if (current.has(id)) return current;
      const changed = new Set(current);
      changed.add(id);
      return changed;
    });
  }, []);
  useEffect(() => {
    if (signedIn) return;
    acceptedDirectory.current = requestedDirectory.current;
    setListed(null);
    setMissingManifests(new Set());
  }, [signedIn]);
  // One gate applies to every directory view. A routine reopen does not retry
  // a 404; a post-change directory answer listing the agent does.
  return useMemo<ManifestReadControl>(
    () => ({
      mayRead: (id) => (listed === null || listed.has(id)) && !missingManifests.has(id),
      generation: generationNow,
      stop: stopManifestRead,
      beginDirectory,
      acceptDirectory,
    }),
    [listed, missingManifests, generationNow, stopManifestRead, beginDirectory, acceptDirectory],
  );
}
