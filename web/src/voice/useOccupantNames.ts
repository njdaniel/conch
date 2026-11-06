// Names for the people the channel list shows in voice (issue #1103). The
// occupancy document carries principal ids alone; a name comes from the
// channel's own member list, which the viewer, a member of every channel in
// the document, may read, as the voice bar's roster names the room it is in
// (useVoiceNames.ts). The page's people, the members of the channel on
// screen, name whom they can first.
//
// Each channel with someone in voice is read once, and again only when
// occupancy lists someone there it did not know: at most one read for each
// new principal in each channel, and none for a channel while one is in
// flight. A read that fails leaves the list's fallback (Member N), an
// identifier the page has, not an invented name. Nothing here joins voice or
// reaches the SDK.
import { useEffect, useMemo, useRef, useState } from "react";
import * as api from "../api";
import { memberName } from "../guestName";
import type { ChannelV0 } from "../schema/channel";
import type { Person } from "../useLiveChannel";
import type { InVoice } from "./useOccupancy";

// What one read of a channel's members gave: the names it held, and every
// principal it listed, named or not (a member without a name is known, and
// is not read for again).
interface Read {
  readonly names: ReadonlyMap<number, string>;
  readonly known: ReadonlySet<number>;
}

export function useOccupantNames({
  occupancy,
  channels,
  people,
  viewer,
}: {
  // Who is in each channel's voice, by channel id.
  readonly occupancy: InVoice;
  // The viewer's channels, for the name a read is made by.
  readonly channels: readonly ChannelV0[];
  // The people of the channel on screen.
  readonly people: ReadonlyMap<number, Person>;
  // The viewer's own principal id: shown as "You", never read for.
  readonly viewer: number;
}): ReadonlyMap<number, string> {
  // What has been read, by channel id.
  const [reads, setReads] = useState<ReadonlyMap<number, Read>>(new Map());
  // The principals a read has been asked for, by channel id, and the
  // channels with a read in flight. Refs, so a read is never asked twice for
  // one render's occupancy.
  const asked = useRef(new Map<number, Set<number>>());
  const inFlight = useRef(new Set<number>());
  const live = useRef(true);

  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);

  useEffect(() => {
    for (const [channelId, voices] of occupancy) {
      if (voices.people === null || inFlight.current.has(channelId)) continue;
      const channel = channels.find((c) => c.id === channelId);
      if (channel === undefined) continue;
      const known = reads.get(channelId)?.known;
      let held = asked.current.get(channelId);
      if (held === undefined) {
        held = new Set();
        asked.current.set(channelId, held);
      }
      // Someone other than the viewer whom neither the page's people name,
      // nor a read of this channel listed, nor one already asked for.
      const fresh = voices.people.filter(
        (id) =>
          id !== viewer &&
          !people.has(id) &&
          known?.has(id) !== true &&
          !held.has(id),
      );
      if (fresh.length === 0) continue;
      for (const id of fresh) held.add(id);
      inFlight.current.add(channelId);
      void api.listMembers(channel.name).then((result) => {
        if (!live.current) return;
        inFlight.current.delete(channelId);
        if (!result.ok) {
          // Nothing named: the fallback stays. The state still moves, so a
          // principal who arrived during the read is looked at again.
          setReads((r) =>
            new Map(r).set(
              channelId,
              r.get(channelId) ?? { names: new Map(), known: new Set() },
            ),
          );
          return;
        }
        const names = new Map<number, string>();
        const listed = new Set<number>();
        for (const m of result.value) {
          listed.add(m.principal_id);
          const name = memberName(m);
          if (name !== undefined) names.set(m.principal_id, name);
        }
        setReads((r) => new Map(r).set(channelId, { names, known: listed }));
      });
    }
  }, [occupancy, channels, people, viewer, reads]);

  return useMemo(() => {
    const out = new Map<number, string>();
    for (const read of reads.values())
      for (const [id, name] of read.names) out.set(id, name);
    for (const [id, person] of people) out.set(id, person.name);
    return out;
  }, [reads, people]);
}
