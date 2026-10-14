// Copies of pkg/schema/message_v2.go and pkg/schema/payload.go.
import { array, int, json, object, optional, str, tagged, timestamp, vocab } from './validate';

// Payload is a message's optional typed machine payload. data is kept opaque:
// a payload schema the client does not know is still a valid message.
export interface Payload {
  schema: string;
  data: unknown;
}

export const payload = object<Payload>({
  schema: str,
  data: json,
});

// Audience scopes a message to a net or to a list of principals (ADR-005).
// The kinds are a closed vocabulary: an unknown kind is refused, never read as
// a channel-wide message. A message with no audience is channel-wide.
export type Audience = NetAudience | PrincipalsAudience;

export interface NetAudience {
  kind: 'net';
  net_id: number;
}

// PrincipalsAudience is a whisper. Read back, it lists the author too.
export interface PrincipalsAudience {
  kind: 'principals';
  principal_ids: number[];
}

export const audience = tagged<Audience, 'kind'>('kind', {
  net: object<NetAudience>({
    kind: vocab<'net'>({ net: true }),
    net_id: int,
  }),
  principals: object<PrincipalsAudience>({
    kind: vocab<'principals'>({ principals: true }),
    principal_ids: array(int),
  }),
});

// MessageV2 is the v2 message envelope.
export interface MessageV2 {
  schema: 'conch.message.v2';
  id: number;
  channel_id: number;
  author_id: number;
  created_at: string;
  body: string;
  payload?: Payload;
  audience?: Audience;
  // thread_root_id is the message this one replies to; absent if it is not a
  // reply.
  thread_root_id?: number;
}

export const messageV2 = object<MessageV2>({
  schema: vocab<'conch.message.v2'>({ 'conch.message.v2': true }),
  id: int,
  channel_id: int,
  author_id: int,
  created_at: timestamp,
  body: str,
  payload: optional(payload),
  audience: optional(audience),
  thread_root_id: optional(int),
});
