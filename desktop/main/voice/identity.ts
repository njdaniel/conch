// Builds the production voice identity: configuration fences without reading a
// bearer; Join/Retry refresh the fence when they read the credential.
import type { Activation } from '../profiles/switch.ts';
import type { VoiceIdentity } from './service.ts';

export function createVoiceIdentity(
  activation: Pick<Activation, 'voiceGeneration' | 'voiceCredential' | 'voiceCurrent'>,
  active: () => boolean,
  fields: Pick<VoiceIdentity, 'profileId' | 'configDir' | 'principalId'>,
): VoiceIdentity {
  let read = activation.voiceGeneration();
  return {
    ...fields,
    credential: () => {
      const credential = activation.voiceCredential();
      if (credential === null) return null;
      read = credential.generation;
      return { origin: credential.origin, token: credential.token };
    },
    current: () => read !== null && active() && activation.voiceCurrent(read),
  };
}
