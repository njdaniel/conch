// The preview store of Preview as member (issue #1517,
// docs/design/permission-preview.md §7.2): what the server answered as the
// named member, held in React memory and nowhere else. It is read whole and
// replaced whole, never merged, and nothing from the administrator's own
// stores is read to build it or to show it.
import type * as apiModule from '../api';
import type { ChannelListingV1, ChannelMemberV1, ChannelV0 } from '../schema/generated';
import type { PreviewResponseV1 } from '../schema/preview';
import type { ServerMembersResponseV1, WhoAmIResponseV1 } from '../schema/principal';

export interface PreviewStore {
  readonly target: PreviewResponseV1;
  readonly channels: readonly ChannelV0[];
  // The channel directory as the target would browse it.
  readonly channelDirectory: readonly ChannelListingV1[];
  // The community's member list as the target sees it, with its scope.
  readonly directory: ServerMembersResponseV1;
  // The member list of each channel read so far, by name.
  readonly members: ReadonlyMap<string, readonly ChannelMemberV1[]>;
  readonly loadedAt: number;
}

// Why a read of the preview did not give a store. unauthorized is a 401 (the
// page signs out); network is no answer (a preview already shown stays, with
// the problem said); refused is any other answer, the server's refusal to
// preview this person any more (403 preview_read_only, forbidden or
// permission_denied, 404 principal_not_found): the preview ends.
export type PreviewLoad =
  | { readonly ok: true; readonly store: PreviewStore }
  | { readonly ok: false; readonly failure: 'unauthorized' | 'network' | 'refused'; readonly code?: string; readonly permission?: string };

type Reads = ReturnType<typeof apiModule.previewAs>;

// loadPreview reads the target's view afresh: the summary, the channel list,
// the channel directory and the member directory together, then the member
// list of the channel to show. The first failure is the answer.
export async function loadPreview(reads: Reads, open: string | null, now: number, again = true): Promise<PreviewLoad> {
  const [summary, channels, channelDirectory, directory] = await Promise.all([reads.summary(), reads.channels(), reads.channelDirectory(), reads.directoryMembers()]);
  for (const result of [summary, channels, channelDirectory, directory]) {
    if (!result.ok) return failed(result);
  }
  if (!summary.ok || !channels.ok || !channelDirectory.ok || !directory.ok) return { ok: false, failure: 'refused' };
  const members = new Map<string, readonly ChannelMemberV1[]>();
  const shown = open !== null && channels.value.some((c) => c.name === open) ? open : (channels.value[0]?.name ?? null);
  if (shown !== null) {
    const list = await reads.listMembers(shown);
    if (!list.ok) {
      // The channel went between the list and its people (channel_not_found):
      // not a refusal to preview. The view is read again once, and a channel
      // still named but gone then has no people to show.
      if (list.failure === 'invalid' && list.code === 'channel_not_found') {
        if (again) return loadPreview(reads, open, now, false);
        members.set(shown, []);
      } else return failed(list);
    } else members.set(shown, list.value);
  }
  return {
    ok: true,
    store: { target: summary.value, channels: channels.value, channelDirectory: channelDirectory.value, directory: directory.value, members, loadedAt: now },
  };
}

function failed(result: { readonly failure: 'unauthorized' | 'network' | 'invalid'; readonly code?: string; readonly permission?: string }): PreviewLoad {
  if (result.failure === 'invalid') return { ok: false, failure: 'refused', ...(result.code === undefined ? {} : { code: result.code }), ...(result.permission === undefined ? {} : { permission: result.permission }) };
  return { ok: false, failure: result.failure };
}

// previewWho is the target as the page's own permission helpers read a
// person: the shape of whoami, so canDo, memberActions and the channel
// helpers are fed the target and need no table of their own for it.
export function previewWho(target: PreviewResponseV1): WhoAmIResponseV1 {
  return {
    id: target.id,
    kind: target.kind,
    name: target.name,
    role: target.role,
    owner: target.owner,
    permissions: [...target.permissions],
    permissions_revision: target.permissions_revision,
    may_invite: target.may_invite,
    voice_allowed: target.voice_allowed,
  };
}
