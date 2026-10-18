// Where voice appears: join and leave, mute and deafen, push-to-talk or an open
// microphone, and who is in the room and who is talking. The SDK and the
// microphone are reached only from src/voice/.
import { VoicePanel } from './voice/VoicePanel';
import { useVoice } from './voice/useVoice';
import { useVoiceNames } from './voice/useVoiceNames';
import type { Person } from './useLiveChannel';

export function VoiceStatus({
  signedIn,
  channel,
  viewer,
  people,
  voiceAllowed,
  reachable,
  onUnauthorized,
  onOpen,
}: {
  readonly signedIn: boolean;
  readonly channel: string | null;
  readonly viewer: number;
  // The people of the channel on screen. While voice is in another channel,
  // the roster's names come from that channel's members (useVoiceNames).
  readonly people: ReadonlyMap<number, Person>;
  // Whether the viewer may use voice: a human whose access includes it.
  // Without it the page asks for no voice presence at all.
  readonly voiceAllowed: boolean;
  // Whether conchd is reached: the status line's Connected.
  readonly reachable: boolean;
  readonly onUnauthorized: () => void;
  // Opens a channel: the strip's way back to the channel voice is in.
  readonly onOpen: (name: string) => void;
}) {
  const voice = useVoice({ signedIn, channel, voiceAllowed, reachable, onUnauthorized });
  const { state, snapshot } = voice;
  const joined = state.phase !== 'idle' && state.phase !== 'stopped' ? state.channel : null;
  const named = useVoiceNames({
    joined,
    viewed: channel,
    presence: snapshot.kind === 'presence' ? snapshot.presence : null,
    people,
  });
  return <VoicePanel voice={voice} viewer={viewer} people={named} viewed={channel} onOpen={onOpen} />;
}
