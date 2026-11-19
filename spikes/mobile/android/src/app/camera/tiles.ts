// Tracks can arrive before the member list or while it is unavailable.
// Use a generic label until the channel's member list can supply the name.
export function cameraTiles<Track>(tracks: ReadonlyMap<number, Track>, me: number, names: ReadonlyMap<number, string>) {
  return [...tracks.entries()]
    .filter(([id]) => id !== me)
    .sort(([a], [b]) => a - b)
    .map(([id, track]) => ({ id, track, name: names.get(id) ?? 'Unknown person' }));
}
