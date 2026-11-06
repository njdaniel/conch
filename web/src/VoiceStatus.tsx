// Where voice appears: join and leave, mute and deafen, push-to-talk or an open
// microphone, and who is in the room and who is talking. The SDK and the
// microphone are reached only from src/voice/.
import { useState } from 'react';
import { readTalkKeyLabel, writeTalkKeyLabel } from './api';
import { isArchived } from './archived';
import { platform } from './platform';
import type { ChannelV0 } from './schema/channel';
import type { InVoice } from './voice/useOccupancy';
import { NativeVoicePanel } from './voice/native/NativeVoicePanel';
import { VoicePanel } from './voice/VoicePanel';
import { useVoice } from './voice/useVoice';
import { useVoiceNames } from './voice/useVoiceNames';
import type { Person } from './useLiveChannel';

type Props = Parameters<typeof BrowserVoiceStatus>[0];

// Where the platform has a native voice (the desktop), the strip is the native
// one: the browser engine, and with it a second microphone publisher, is never
// built there (ADR-007, linux-desktop.md section 6).
export function VoiceStatus(props: Props) {
  const native = platform.nativeVoice;
  if (native !== undefined) return <NativeStatus native={native} {...props} />;
  return <BrowserVoiceStatus {...props} />;
}

function NativeStatus({ native, ...props }: Props & { readonly native: NonNullable<typeof platform.nativeVoice> }) {
  const [label, setLabel] = useState(readTalkKeyLabel);
  return (
      <NativeVoicePanel
        source={native}
        binding={label}
        onBound={(next) => {
          writeTalkKeyLabel(next);
          setLabel(next);
        }}
        signedIn={props.signedIn}
        channel={props.channel}
        viewer={props.viewer}
        people={props.people}
        voiceAllowed={props.voiceAllowed}
        channels={props.channels}
        onUnauthorized={props.onUnauthorized}
        onOpen={props.onOpen}
      />
  );
}

function BrowserVoiceStatus({
  signedIn,
  channel,
  viewer,
  people,
  voiceAllowed,
  cameraAllowed = false,
  reachable,
  channels,
  occupancy,
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
  // Whether whoami says the viewer may turn a camera on (use_camera).
  readonly cameraAllowed?: boolean;
  // Whether conchd is reached: the status line's Connected.
  readonly reachable: boolean;
  // The viewer's channels and how many are in each one's voice (issue #596),
  // for the line that says where else people are talking; and by id, how voice
  // knows a channel renamed under it (issue #728).
  readonly channels: readonly ChannelV0[];
  readonly occupancy: InVoice;
  readonly onUnauthorized: () => void;
  // Opens a channel: the strip's way back to the channel voice is in.
  readonly onOpen: (name: string) => void;
}) {
  const voice = useVoice({ signedIn, channel, channels, voiceAllowed, reachable, onUnauthorized });
  const { state, snapshot } = voice;
  const joined = state.phase !== 'idle' && state.phase !== 'stopped' ? state.channel : null;
  const named = useVoiceNames({
    joined,
    viewed: channel,
    presence: snapshot.kind === 'presence' ? snapshot.presence : null,
    people,
  });
  return <VoicePanel voice={voice} viewer={viewer} people={named} viewed={channel} archived={isArchived(channels.find((c) => c.name === channel))} channels={channels} occupancy={occupancy} cameraAllowed={cameraAllowed} onOpen={onOpen} />;
}
