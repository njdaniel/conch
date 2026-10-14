// Copies of pkg/schema/channel.go and pkg/schema/channel_member.go.
import { type PrincipalKind, principalKind } from './principal';
import { array, int, object, optional, str, timestamp } from './validate';

// ChannelV0 is one channel.
export interface ChannelV0 {
  id: number;
  name: string;
  created_at: string;
}

export const channelV0 = object<ChannelV0>({
  id: int,
  name: str,
  created_at: timestamp,
});

// ListChannelsResponse is the body of the channel list, ordered by id.
export interface ListChannelsResponse {
  channels: ChannelV0[];
}

export const listChannelsResponse = object<ListChannelsResponse>({
  channels: array(channelV0),
});

// ChannelMemberV1 is one entry of a channel's member list. added_by is absent
// when unknown; name and kind are absent only from older servers.
export interface ChannelMemberV1 {
  principal_id: number;
  added_by?: number;
  created_at: string;
  name?: string;
  kind?: PrincipalKind;
}

export const channelMemberV1 = object<ChannelMemberV1>({
  principal_id: int,
  added_by: optional(int),
  created_at: timestamp,
  name: optional(str),
  kind: optional(principalKind),
});

// ListChannelMembersResponseV1 is the body of
// GET /v1/channels/{channel}/members, ordered by principal_id.
export interface ListChannelMembersResponseV1 {
  members: ChannelMemberV1[];
}

export const listChannelMembersResponseV1 = object<ListChannelMembersResponseV1>({
  members: array(channelMemberV1),
});
