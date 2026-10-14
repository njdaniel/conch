// Copies of pkg/schema/net.go.
import { array, int, object, str, timestamp, vocab } from './validate';

export type NetRole = 'member' | 'monitor';

export interface NetMember {
  principal_id: number;
  role: NetRole;
}

export const netMember = object<NetMember>({
  principal_id: int,
  role: vocab<NetRole>({ member: true, monitor: true }),
});

// NetV1 is one net of a channel. The chat only needs its name, to label a
// message scoped to it.
export interface NetV1 {
  id: number;
  channel_id: number;
  name: string;
  members: NetMember[];
  created_at: string;
}

export const netV1 = object<NetV1>({
  id: int,
  channel_id: int,
  name: str,
  members: array(netMember),
  created_at: timestamp,
});

// ListNetsResponseV1 is the body of GET /v1/channels/{channel}/nets.
export interface ListNetsResponseV1 {
  nets: NetV1[];
}

export const listNetsResponseV1 = object<ListNetsResponseV1>({
  nets: array(netV1),
});
