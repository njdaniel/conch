// What the events socket sends: one pkg/schema document per text frame, told
// apart by its schema field. Each is checked with its validator before the
// page looks at it; a frame of a schema this client does not know is skipped
// (a newer server may send more), and so is one that does not validate.
import { activitySchema, agentActivityV1, type AgentActivityV1 } from '../schema/activity';
import { channelDocSchema, channelDocV1, channelMembersSchema, channelMembersV1, type ChannelDocV1, type ChannelMembersV1 } from '../schema/channel';
import { interventionSchema, interventionV1, type InterventionV1 } from '../schema/intervention';
import { messageEditedV1, messageEditedV1Schema, messageV2, type MessageEditedV1, type MessageV2 } from '../schema/message';
import { channelsChangedV1, channelsChangedV1Schema, messageV2Schema, readStateV1, readStateV1Schema, type ReadStateV1 } from '../schema/generated';
import { runSchema, runV1, type RunV1 } from '../schema/run';
import { sessionRosterSchema, sessionRosterV1, type SessionRosterV1 } from '../schema/session';

export type EventFrame =
  | { readonly kind: 'roster'; readonly roster: SessionRosterV1 }
  | { readonly kind: 'members'; readonly members: ChannelMembersV1 }
  | { readonly kind: 'run'; readonly run: RunV1 }
  | { readonly kind: 'activity'; readonly entry: AgentActivityV1 }
  | { readonly kind: 'intervention'; readonly intervention: InterventionV1 }
  // The channel as it now is, sent when it is renamed (issue #728).
  | { readonly kind: 'channel'; readonly channel: ChannelDocV1 }
  | { readonly kind: 'readState'; readonly state: ReadStateV1 }
  | { readonly kind: 'message'; readonly message: MessageV2 }
  // A message edited (issue #845): never a new one, so it never notifies.
  | { readonly kind: 'messageEdited'; readonly message: MessageV2 }
  // The person's own channels may have changed (issue #1027): no content, only
  // a hint to read the list again.
  | { readonly kind: 'channelsChanged' };

export function parseEventFrame(data: unknown): EventFrame | null {
  if (typeof data !== 'string') return null;
  let doc: unknown;
  try {
    doc = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof doc !== 'object' || doc === null) return null;
  switch ((doc as { schema?: unknown }).schema) {
    case sessionRosterSchema:
      return sessionRosterV1(doc).length === 0 ? { kind: 'roster', roster: doc as SessionRosterV1 } : null;
    case channelMembersSchema:
      return channelMembersV1(doc).length === 0 ? { kind: 'members', members: doc as ChannelMembersV1 } : null;
    case runSchema:
      return runV1(doc).length === 0 ? { kind: 'run', run: doc as RunV1 } : null;
    case activitySchema:
      return agentActivityV1(doc).length === 0 ? { kind: 'activity', entry: doc as AgentActivityV1 } : null;
    case interventionSchema:
      return interventionV1(doc).length === 0 ? { kind: 'intervention', intervention: doc as InterventionV1 } : null;
    case channelDocSchema:
      return channelDocV1(doc).length === 0 ? { kind: 'channel', channel: doc as ChannelDocV1 } : null;
    case readStateV1Schema:
      return readStateV1(doc).length === 0 ? { kind: 'readState', state: doc as ReadStateV1 } : null;
    case messageV2Schema:
      return messageV2(doc).length === 0 ? { kind: 'message', message: doc as MessageV2 } : null;
    case messageEditedV1Schema:
      return messageEditedV1(doc).length === 0 ? { kind: 'messageEdited', message: (doc as MessageEditedV1).message } : null;
    case channelsChangedV1Schema:
      // The frame has no content, and a field a newer server adds is ignored:
      // only the document's own schema is checked.
      return channelsChangedV1({ schema: channelsChangedV1Schema }).length === 0 ? { kind: 'channelsChanged' } : null;
    default:
      return null;
  }
}
