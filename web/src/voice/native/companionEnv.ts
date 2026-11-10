// The voice client's view of the desktop's screen companion (issue #1581,
// D4-04 of #772): the native voice's companion member shaped as the
// CompanionEnv the VoiceClient takes. The grant, the end and main's end event
// are the bridge's; the deafen is the helper's confirmed one, which the
// companion's playback of watched shares follows. Absent where there is no
// native voice or no companion bridge (the browser).
import type { NativeVoice } from '../../platform/types';
import type { CompanionEnv } from '../connection';

export function companionEnv(source: NativeVoice | undefined): CompanionEnv | undefined {
  const companion = source?.companion;
  if (source === undefined || companion === undefined) return undefined;
  return {
    grant: (channel) => companion.grant({ channel }),
    arm: () => companion.shareArm(),
    end: () => companion.end(),
    onEnd: (listener) =>
      companion.onEnd((event) => {
        listener(event.reason);
      }),
    deafened: () => source.snapshot().helper?.deafened === true,
    onDeafened: (listener) => source.subscribe(listener),
  };
}
