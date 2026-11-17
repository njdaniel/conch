// Following a channel rename (issues #1880, #1881; docs/design/channel-rename.md
// §3.4). Input: a client, a channel id that conchd's own answers showed (never
// the page's) and a time bound. Output: the channel's name now, as conchd's
// channel list gives it. Owns: nothing; it keeps no state, so the share's
// sound and the screen companion each keep the id they hold.
//
// conchd resolves /v1/channels/{channel}/... by the channel's current name. A
// rename makes the old name answer 404 channel_not_found (or, once another
// channel takes the name, answer for that other channel).
import { paramOk } from '../api/target.ts';
import type { Exchange, Send } from '../api/client.ts';
import { listChannelsResponse, type ListChannelsResponse } from '../../../web/src/schema/generated.ts';
import { fromServer } from '../../../web/src/schema/validate.ts';

// conchd's word that it has no channel of that name for the caller (the voice
// routes' 404, channel_not_found): any other 404 is not a rename.
export const isNotFound = (e: Exchange): boolean =>
  e.kind === 'answer' && e.status === 404 && typeof e.body === 'object' && e.body !== null && (e.body as { code?: unknown }).code === 'channel_not_found';

// The channel's name now, or why there is none. 'gone': the list was read and
// the id is not in it, or its name is one the client would refuse to put in a
// path (a separator, a percent: see paramOk), so it cannot be followed.
// 'unknown': the list could not be read in time, or was not a valid answer.
export type CurrentName = { readonly name: string } | 'gone' | 'unknown';

// currentName reads conchd's channel list with `client` and finds `id` in it.
// The list is not paginated (handleListChannels returns all of the caller's
// channels). `timeoutMs` bounds the read; a bound already spent reads nothing.
export async function currentName(client: { send(request: Send): Promise<Exchange> }, id: number, timeoutMs: number): Promise<CurrentName> {
  if (timeoutMs <= 0) return 'unknown';
  let answer: Exchange;
  try {
    answer = await client.send({ op: 'listChannels', timeoutMs });
  } catch {
    return 'unknown';
  }
  if (answer.kind !== 'answer' || !answer.ok || fromServer(listChannelsResponse)(answer.body).length !== 0) return 'unknown';
  const found = (answer.body as ListChannelsResponse).channels.find((c) => c.id === id);
  if (found === undefined || !paramOk(found.name)) return 'gone';
  return { name: found.name };
}
