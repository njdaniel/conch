// Names for the voice roster when the channel voice is in is not the one on
// screen (issue #588, F05). The page's people are the members of the channel on
// screen, and someone in voice need not be one of them; the viewer is a member
// of the channel joined, so its member list names everyone in its room.
//
// While joined elsewhere, that list is read once, and again only when presence
// lists someone it did not have: at most one read for each new principal, and
// none while one is in flight. Leaving drops it. A read that fails leaves the
// roster's fallback (Member N), which is an identifier the page has, not an
// invented name. Not joined, or joined in the channel on screen, nothing is
// read: the page's people already hold everyone.
import { useEffect, useMemo, useRef, useState } from 'react';
import * as api from '../api';
import { memberName } from '../guestName';
import type { VoicePresenceV1 } from '../schema/voice';
import type { Person } from '../useLiveChannel';
import { occupants } from './presence';

// What one read of the joined channel's members gave: the names it held, and
// every principal it listed, named or not (a member without a name is known,
// and is not read for again).
interface Names {
  readonly channel: string;
  readonly people: ReadonlyMap<number, Person>;
  readonly known: ReadonlySet<number>;
}

export function useVoiceNames({
  joined,
  viewed,
  presence,
  people,
}: {
  // The channel voice is in, or null when not in voice.
  readonly joined: string | null;
  readonly viewed: string | null;
  // The joined channel's presence while in voice.
  readonly presence: VoicePresenceV1 | null;
  // The people of the channel on screen.
  readonly people: ReadonlyMap<number, Person>;
}): ReadonlyMap<number, Person> {
  const [names, setNames] = useState<Names | null>(null);
  // The principals a read has been asked for, in the joined channel, and
  // whether one is in flight. Refs, so a read is never asked twice for one
  // render's presence.
  const asked = useRef<{ channel: string; ids: Set<number> } | null>(null);
  const inFlight = useRef(false);
  // Moves on with the joined channel: a read for one left behind is dropped.
  const generation = useRef(0);

  const elsewhere = joined !== null && joined !== viewed ? joined : null;
  const ids = useMemo(() => occupants(presence).map((o) => o.principal), [presence]);

  // Leaving, or joining another channel, drops what was read.
  useEffect(() => {
    if ((asked.current?.channel ?? null) === joined) return;
    generation.current++;
    inFlight.current = false;
    asked.current = joined === null ? null : { channel: joined, ids: new Set() };
    setNames(null);
  }, [joined]);

  useEffect(() => {
    if (elsewhere === null || inFlight.current) return;
    const held = asked.current;
    if (held?.channel !== elsewhere) return;
    const known = names?.channel === elsewhere ? names.known : null;
    // The first read, or a principal neither known nor asked for already.
    const fresh = ids.filter((id) => known?.has(id) !== true && !held.ids.has(id));
    if (known !== null && fresh.length === 0) return;
    for (const id of fresh) held.ids.add(id);
    inFlight.current = true;
    const mine = generation.current;
    void api.listMembers(elsewhere).then((result) => {
      if (generation.current !== mine) return;
      inFlight.current = false;
      if (!result.ok) {
        // Nothing named: the fallback stays. The state still moves, so a
        // principal who arrived during the read is looked at again.
        setNames((n) => (n?.channel === elsewhere ? { ...n } : { channel: elsewhere, people: new Map(), known: new Set() }));
        return;
      }
      const named = new Map<number, Person>();
      for (const m of result.value) {
        const name = memberName(m);
        if (name !== undefined) named.set(m.principal_id, { name, kind: m.kind ?? null });
      }
      setNames({ channel: elsewhere, people: named, known: new Set(result.value.map((m) => m.principal_id)) });
    });
  }, [elsewhere, ids, names]);

  return useMemo(() => {
    if (elsewhere === null || names?.channel !== elsewhere || names.people.size === 0) return people;
    const merged = new Map(people);
    for (const [id, person] of names.people) merged.set(id, person);
    return merged;
  }, [elsewhere, names, people]);
}
