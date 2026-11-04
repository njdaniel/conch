// Authors who are not in the channel's member list (design §13.5, O14d): a
// message's author stays named for everyone who can read it, so the page asks
// the channel's authors route for the ones it does not hold, in batches of at
// most api.authorBatch, and falls back to #<id> only for an id the route did
// not name. The member list always wins over the route, and what the route
// said is dropped whenever the list it was asked against is replaced (the
// directory changed), so no name outlives the answer that gave it.
import { useEffect, useMemo, useState } from 'react';
import * as api from './api';
import type { Person } from './useLiveChannel';

interface Held {
  readonly channel: string;
  readonly against: ReadonlyMap<number, Person>;
  readonly names: ReadonlyMap<number, Person>;
  // Ids the route was asked about and answered, named or not: not asked again
  // until the list is replaced.
  readonly asked: ReadonlySet<number>;
}

// useAuthorNames is people plus the names of the given author ids that the
// route answered for. A 401 is passed to onUnauthorized.
export function useAuthorNames(
  channel: string,
  authorIds: readonly number[],
  people: ReadonlyMap<number, Person>,
  onUnauthorized?: () => void,
): ReadonlyMap<number, Person> {
  const [held, setHeld] = useState<Held | null>(null);
  const fresh = held !== null && held.channel === channel && held.against === people ? held : null;
  const current = fresh === null ? null : fresh.names;
  const missing = authorIds.filter((id) => !people.has(id) && fresh?.asked.has(id) !== true);
  const key = [...new Set(missing)].sort((a, b) => a - b).join(',');
  useEffect(() => {
    if (key === '') return;
    const ids = key.split(',').map(Number);
    let live = true;
    void api.channelAuthors(channel, ids).then((result) => {
      if (!live) return;
      if (!result.ok) {
        if (result.failure === 'unauthorized') onUnauthorized?.();
        return;
      }
      setHeld((before) => {
        const same = before !== null && before.channel === channel && before.against === people;
        const names = new Map(same ? before.names : []);
        const asked = new Set(same ? before.asked : []);
        for (const id of ids) asked.add(id);
        for (const author of result.value) names.set(author.principal_id, { name: author.name, kind: author.kind });
        return { channel, against: people, names, asked };
      });
    });
    return () => {
      live = false;
    };
    // onUnauthorized is a callback of the page, not a reason to ask again.
  }, [channel, people, key]);
  return useMemo(() => {
    if (current === null || current.size === 0) return people;
    const all = new Map(current);
    for (const [id, person] of people) all.set(id, person);
    return all;
  }, [people, current]);
}
