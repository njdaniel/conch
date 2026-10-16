// Where voice appears: join and leave, mute and deafen, push-to-talk or an open
// microphone, and who is in the room and who is talking. The SDK and the
// microphone are reached only from src/voice/.
import { VoicePanel } from './voice/VoicePanel';
import { useVoice } from './voice/useVoice';
import type { Person } from './useLiveChannel';

export function VoiceStatus({
  signedIn,
  channel,
  viewer,
  people,
  onUnauthorized,
}: {
  readonly signedIn: boolean;
  readonly channel: string | null;
  readonly viewer: number;
  readonly people: ReadonlyMap<number, Person>;
  readonly onUnauthorized: () => void;
}) {
  const voice = useVoice({ signedIn, channel, onUnauthorized });
  return <VoicePanel voice={voice} viewer={viewer} people={people} viewed={channel} />;
}
