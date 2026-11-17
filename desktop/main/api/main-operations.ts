// The two routes only main calls (issue #1854; linux-share-audio.md §5.3, §8):
// the share's sound consent, PUT and DELETE …/voice/companion/audio. They sit
// beside the shared table of web/src/platform/operations.ts rather than in it:
// no page invokes either (the renderer has no bridge function that names an
// operation here, and isOperationId, which parses every renderer request,
// reads the shared table alone), so the browser's table and its tests are
// unchanged. Both are bearer operations sent by Client.send with the identity
// in force, like any other; the bodies are types of web/src/schema.
import { voiceCompanionAudioRequestV1, voiceCompanionAudioV1 } from '../../../web/src/schema/generated.ts';
import { fromServer, type Validator } from '../../../web/src/schema/validate.ts';
import { operations, type Operation, type OperationId } from '../../../web/src/platform/operations.ts';

const check = <T>(validator: Validator<T>): Validator<never> => validator as unknown as Validator<never>;

export const mainOperations = {
  // The consent: scope and publisher in, the consent as recorded out. A
  // request stays strict; an answer tolerates a key it lacks (as the table's
  // replies do).
  voiceShareAudio: {
    method: 'PUT',
    path: '/v1/channels/{channel}/voice/companion/audio',
    auth: 'bearer',
    request: check(voiceCompanionAudioRequestV1),
    response: check(fromServer(voiceCompanionAudioV1)),
    renderer: false,
  },
  // The withdrawal: 204 whether or not a consent was there.
  voiceShareAudioEnd: { method: 'DELETE', path: '/v1/channels/{channel}/voice/companion/audio', auth: 'bearer', renderer: false },
} as const satisfies Record<string, Operation>;

export type MainOperationId = keyof typeof mainOperations;

// Every operation Client.send may send: the shared table's and main's own.
export type SendableOperationId = OperationId | MainOperationId;

export function operationOf(id: SendableOperationId): Operation {
  return Object.hasOwn(mainOperations, id) ? mainOperations[id as MainOperationId] : operations[id as OperationId];
}
