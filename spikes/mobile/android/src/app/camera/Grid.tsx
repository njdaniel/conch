// The receive grid and the camera button. The grid is rendered by the Voice
// view only, so a camera is never shown (or, with policy.ts, received) outside
// it (webcam-video.md §8; #1109 M-C8).
import { useSyncExternalStore } from 'react';
import { Text, View } from 'react-native';
import { VideoView } from '@livekit/react-native';
import type { RemoteVideoTrack } from 'livekit-client';
import { cameraNote } from '../../logic/camera';
import { Button, Note, colours, styles } from '../ui/common';
import { controller } from '../services';
import { cameraStore } from './store';
import { useVoiceState } from '../ui/useVoiceState';

function Tile(props: { name: string; track: RemoteVideoTrack }) {
  return (
    <View accessible accessibilityLabel={props.name + ' camera'} style={{ width: '48%', aspectRatio: 16 / 9, marginTop: 8, backgroundColor: colours.ink, borderRadius: 6, overflow: 'hidden' }}>
      <VideoView videoTrack={props.track} objectFit="cover" style={{ flex: 1 }} />
      <Text style={{ position: 'absolute', left: 6, bottom: 4, color: '#ffffff', fontSize: 12 }}>{props.name}</Text>
    </View>
  );
}

export function CameraGrid(props: { me: number }) {
  const tracks = useSyncExternalStore(cameraStore.subscribe, cameraStore.get);
  const ids = [...tracks.keys()].filter((id) => id !== props.me).sort((a, b) => a - b);
  if (ids.length === 0) return null;
  return (
    <View>
      <Text style={styles.label}>Cameras</Text>
      <View style={{ flexDirection: 'row', flexWrap: 'wrap', justifyContent: 'space-between' }}>
        {ids.map((id) => {
          const track = tracks.get(id);
          return track === undefined ? null : <Tile key={id} name={'Person ' + String(id)} track={track} />;
        })}
      </View>
    </View>
  );
}

// The button, its words and the state's. Shown while connected; a person in
// Listen only is told why it does nothing.
export function CameraControl() {
  const v = useVoiceState(controller);
  const on = v.cameraOn;
  const note = v.cameraExtra ?? cameraNote(v.camera);
  return (
    <View>
      <Button
        label={on ? 'Turn camera off' : 'Turn camera on'}
        kind={on ? 'danger' : 'plain'}
        disabled={v.phase !== 'connected' || v.camera.kind === 'stopping' || v.blocked.length > 0}
        hint="Sends your front camera to this voice room. It stops by itself if the app leaves the screen."
        onPress={() => void controller.cameraTap()}
      />
      <Text style={styles.small}>{v.camera.kind === 'sending' ? 'Your camera is being sent.' : 'Your camera is off.'}</Text>
      <Note text={note} kind={v.camera.kind === 'failed' ? 'warn' : 'plain'} />
    </View>
  );
}
