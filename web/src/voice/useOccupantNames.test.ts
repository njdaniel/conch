// Names for the people the channel list shows in voice (issue #1103): read
// from each channel's members, once per channel, and again only for someone
// new there; the page's own people named without a read. The member read is
// a fake the test answers. No network, no SDK.
import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ApiResult } from "../api";
import type { ChannelMemberV1, ChannelV0 } from "../schema/channel";
import type { Person } from "../useLiveChannel";
import type { InVoice } from "./useOccupancy";

// One member read the hook asked for, and how to answer it.
interface Read {
  readonly channel: string;
  readonly answer: (result: ApiResult<ChannelMemberV1[]>) => void;
}

const fake = vi.hoisted(() => ({ reads: [] as Read[] }));

vi.mock("../api", () => ({
  listMembers: (channel: string) =>
    new Promise<ApiResult<ChannelMemberV1[]>>((resolve) => {
      fake.reads.push({ channel, answer: resolve });
    }),
}));

import { useOccupantNames } from "./useOccupantNames";

const at = "2026-10-18T00:00:00Z";
const channels: ChannelV0[] = [
  { id: 1, name: "general", created_at: at },
  { id: 2, name: "game-night", created_at: at },
  { id: 3, name: "book-club", created_at: at },
];

const member = (principal_id: number, name?: string): ChannelMemberV1 => ({
  principal_id,
  created_at: at,
  ...(name === undefined ? {} : { name, kind: "human" as const }),
});

// Who is in voice, by channel id; null for a server that did not say.
const voices = (people: Record<number, number[] | null>): InVoice =>
  new Map(
    Object.entries(people).map(([id, ids]) => [
      Number(id),
      { count: ids?.length ?? 1, people: ids },
    ]),
  );

// The people of the channel on screen: the viewer (1) and the operator (9).
const viewedPeople: ReadonlyMap<number, Person> = new Map([
  [1, { name: "alice", kind: "human" }],
  [9, { name: "operator", kind: "human" }],
]);

interface Props {
  readonly occupancy: InVoice;
  readonly people?: ReadonlyMap<number, Person>;
}

// The viewer is 1.
function hook(initial: Props) {
  return renderHook(
    (p: Props) =>
      useOccupantNames({
        occupancy: p.occupancy,
        channels,
        people: p.people ?? viewedPeople,
        viewer: 1,
      }),
    { initialProps: initial },
  );
}

async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

async function answer(
  i: number,
  result: ApiResult<ChannelMemberV1[]>,
): Promise<void> {
  const read = fake.reads[i];
  if (read === undefined) throw new Error(`no read ${String(i)}`);
  read.answer(result);
  await flush();
}

beforeEach(() => {
  fake.reads.length = 0;
});

describe("useOccupantNames", () => {
  it("reads nothing while nobody is in voice, and names the page's people", async () => {
    const { result } = hook({ occupancy: voices({}) });
    await flush();
    expect(fake.reads).toEqual([]);
    expect(result.current.get(9)).toBe("operator");
  });

  it("reads nothing for people the page already names, nor for a server that did not say who", async () => {
    const { result } = hook({ occupancy: voices({ 2: [1, 9], 3: null }) });
    await flush();
    expect(fake.reads).toEqual([]);
    expect(result.current.get(1)).toBe("alice");
  });

  it("never reads for the viewer alone, whom the list calls You", async () => {
    hook({ occupancy: voices({ 2: [1] }), people: new Map() });
    await flush();
    expect(fake.reads).toEqual([]);
  });

  it("reads a channel's members once for someone it does not name, and names them", async () => {
    const { result, rerender } = hook({ occupancy: voices({ 2: [1, 2] }) });
    await flush();
    expect(fake.reads.map((r) => r.channel)).toEqual(["game-night"]);
    expect(result.current.has(2)).toBe(false);
    await answer(0, {
      ok: true,
      value: [member(1, "alice"), member(2, "bob")],
    });
    expect(result.current.get(2)).toBe("bob");
    // The same people again, in another order, or fewer: no new read.
    rerender({ occupancy: voices({ 2: [2, 1] }) });
    rerender({ occupancy: voices({ 2: [2] }) });
    await flush();
    expect(fake.reads).toHaveLength(1);
    expect(result.current.get(2)).toBe("bob");
  });

  it("reads each channel with someone new, and a channel again only for someone new there", async () => {
    const { result, rerender } = hook({
      occupancy: voices({ 2: [2], 3: [3] }),
    });
    await flush();
    expect(fake.reads.map((r) => r.channel)).toEqual([
      "game-night",
      "book-club",
    ]);
    await answer(0, { ok: true, value: [member(2, "bob")] });
    await answer(1, { ok: true, value: [member(3, "carol")] });
    expect(result.current.get(2)).toBe("bob");
    expect(result.current.get(3)).toBe("carol");
    // bob known from game-night joins book-club too: book-club is read again
    // for him; game-night is not.
    rerender({ occupancy: voices({ 2: [2], 3: [2, 3] }) });
    await flush();
    expect(fake.reads.map((r) => r.channel)).toEqual([
      "game-night",
      "book-club",
      "book-club",
    ]);
    await answer(2, {
      ok: true,
      value: [member(2, "bob"), member(3, "carol")],
    });
    expect(result.current.get(2)).toBe("bob");
  });

  it("does not read a channel a second time while one is in flight, and looks again once it is back", async () => {
    const { result, rerender } = hook({ occupancy: voices({ 2: [2] }) });
    await flush();
    rerender({ occupancy: voices({ 2: [2, 3] }) });
    await flush();
    expect(fake.reads).toHaveLength(1);
    // The read in flight knew bob only; carol, who came during it, is read
    // for once it is back.
    await answer(0, { ok: true, value: [member(2, "bob")] });
    expect(fake.reads).toHaveLength(2);
    await answer(1, {
      ok: true,
      value: [member(2, "bob"), member(3, "carol")],
    });
    expect(result.current.get(3)).toBe("carol");
  });

  it("asks at most once for someone the list does not name, and keeps the fallback after a failed read", async () => {
    const { result, rerender } = hook({ occupancy: voices({ 2: [2, 5] }) });
    await flush();
    // A member without a name is known, and not read for again.
    await answer(0, { ok: true, value: [member(2, "bob"), member(5)] });
    expect(result.current.get(2)).toBe("bob");
    expect(result.current.has(5)).toBe(false);
    rerender({ occupancy: voices({ 2: [5, 2] }) });
    await flush();
    expect(fake.reads).toHaveLength(1);
    // A failed read for someone new names nobody and is not repeated for them.
    rerender({ occupancy: voices({ 2: [2, 5, 6] }) });
    await flush();
    expect(fake.reads).toHaveLength(2);
    await answer(1, { ok: false, failure: "network" });
    expect(result.current.has(6)).toBe(false);
    rerender({ occupancy: voices({ 2: [6] }) });
    await flush();
    expect(fake.reads).toHaveLength(2);
  });

  it("names from the page's people first", async () => {
    const { result } = hook({
      occupancy: voices({ 2: [9] }),
      people: new Map([[9, { name: "the operator", kind: "human" }]]),
    });
    await flush();
    expect(fake.reads).toEqual([]);
    expect(result.current.get(9)).toBe("the operator");
  });
});
