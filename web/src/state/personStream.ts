// The per-person events stream's frames (issue #2040, ADR-017): what arrives
// on GET /v1/events/person/ws, which is scoped to the signed-in person and
// carries no channel. This reads one text frame into a typed frame; it owns no
// socket, timer or state (api.ts openPersonSocket opens the socket, and the DM
// state that applies these frames is a later issue's).
//
// Every frame is checked by its generated validator, leniently as every
// server document is (a field a later server adds is ignored). A frame whose
// schema this client does not know, one that fails its validator, and anything
// that is not JSON, is null: the stream is followed, not trusted, and a schema
// added later (the call frames) must not break a client that predates it.
import {
  dmChangedV1,
  dmChangedV1Schema,
  dmMessageEditedV1,
  dmMessageEditedV1Schema,
  dmMessageRemovedV1,
  dmMessageRemovedV1Schema,
  dmMessageV1,
  dmMessageV1Schema,
  dmSnapshotV1,
  dmSnapshotV1Schema,
  personStreamHelloV1,
  personStreamHelloV1Schema,
  type DMChangedV1,
  type DMMessageEditedV1,
  type DMMessageRemovedV1,
  type DMMessageV1,
  type DMSnapshotV1,
  type PersonStreamHelloV1,
} from '../schema/generated';
import { fromServer, type Validator } from '../schema/validate';

export type PersonFrame =
  // The first frame of a connection: this socket's own instance id. Ringing
  // dismissal and the selected endpoint name a tab by it; it is new on every
  // connection and is not a credential.
  | { readonly kind: 'hello'; readonly hello: PersonStreamHelloV1 }
  // The second: the reconnect snapshot. It replaces what the client holds;
  // a frame for a conversation at or below that conversation's revision in it is a duplicate to ignore.
  | { readonly kind: 'snapshot'; readonly snapshot: DMSnapshotV1 }
  | { readonly kind: 'dm_message'; readonly message: DMMessageV1 }
  | { readonly kind: 'dm_message_edited'; readonly edited: DMMessageEditedV1 }
  | { readonly kind: 'dm_message_removed'; readonly removed: DMMessageRemovedV1 }
  | { readonly kind: 'dm_changed'; readonly changed: DMChangedV1 };

// checked is body as T once the generated validator accepts it, else null.
function checked<T>(check: Validator<T>, body: object): T | null {
  return fromServer(check)(body).length === 0 ? (body as T) : null;
}

// parsePersonFrame is the one reader of the stream's frames.
export function parsePersonFrame(data: unknown): PersonFrame | null {
  if (typeof data !== 'string') return null;
  let body: unknown;
  try {
    body = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof body !== 'object' || body === null) return null;
  const schema: unknown = (body as { schema?: unknown }).schema;
  switch (schema) {
    case personStreamHelloV1Schema: {
      const hello = checked(personStreamHelloV1, body);
      return hello === null ? null : { kind: 'hello', hello };
    }
    case dmSnapshotV1Schema: {
      const snapshot = checked(dmSnapshotV1, body);
      return snapshot === null ? null : { kind: 'snapshot', snapshot };
    }
    case dmMessageV1Schema: {
      const message = checked(dmMessageV1, body);
      return message === null ? null : { kind: 'dm_message', message };
    }
    case dmMessageEditedV1Schema: {
      const edited = checked(dmMessageEditedV1, body);
      return edited === null ? null : { kind: 'dm_message_edited', edited };
    }
    case dmMessageRemovedV1Schema: {
      const removed = checked(dmMessageRemovedV1, body);
      return removed === null ? null : { kind: 'dm_message_removed', removed };
    }
    case dmChangedV1Schema: {
      const changed = checked(dmChangedV1, body);
      return changed === null ? null : { kind: 'dm_changed', changed };
    }
    default: return null;
  }
}
