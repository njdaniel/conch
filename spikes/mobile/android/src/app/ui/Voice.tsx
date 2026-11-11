// The voice screen: presence, the hand-off warning, Join and Leave, hold to
// talk, mute, deafen, the open microphone and listen-only. The minimum the
// matrix needs (#1109: production UI is M2's).
import { useSyncExternalStore, useEffect } from 'react';
import { Text, View } from 'react-native';
import { useTrackVolume } from '@livekit/react-native';
import type { LocalAudioTrack } from 'livekit-client';
import { controller, micStore, principal } from '../services';
import { useVoiceState } from './useVoiceState';
import { Button, Note, Screen, colours, styles } from './common';
import { CameraControl, CameraGrid } from '../camera/Grid';

// Reads the microphone's level for the open microphone. Rendered only in the
// open mode while connected.
function LevelProbe(props: { track: LocalAudioTrack }) {
  const volume = useTrackVolume(props.track);
  useEffect(() => {
    controller.level(volume);
  }, [volume]);
  return null;
}

function HoldToTalk(props: { enabled: boolean; open: boolean }) {
  return (
    <View
      accessible
      accessibilityRole="button"
      accessibilityLabel="Hold to talk"
      accessibilityHint="Press and hold to talk. Release to stop."
      accessibilityState={{ disabled: !props.enabled, busy: props.open }}
      onTouchStart={() => {
        if (props.enabled) controller.press();
      }}
      onTouchEnd={() => {
        controller.release();
      }}
      // The system took the touch (a gesture, the shade, a call banner): the
      // gate shuts at once, with no tail (M-F5).
      onTouchCancel={() => {
        controller.cancel();
      }}
      style={{
        minHeight: 120,
        borderRadius: 12,
        marginTop: 16,
        alignItems: 'center',
        justifyContent: 'center',
        backgroundColor: !props.enabled ? colours.line : props.open ? colours.hot : colours.accent,
      }}
    >
      <Text style={{ color: '#ffffff', fontSize: 20, fontWeight: '600' }}>{props.open ? 'Talking' : 'Hold to talk'}</Text>
    </View>
  );
}

export function Voice(props: { channel: string; onBack: () => void }) {
  const v = useVoiceState(controller);
  const mic = useSyncExternalStore(micStore.subscribe, micStore.get);
  const inVoice = v.phase === 'connected' || v.phase === 'reconnecting';
  const mine = v.channel === props.channel;
  const shown = mine || v.phase === 'idle';
  const me = principal().id;

  useEffect(() => {
    controller.watch(props.channel);
    return () => {
      controller.watch(null);
    };
  }, [props.channel]);

  // Cameras are received only while this view is open (M-C8).
  useEffect(() => {
    controller.setViewing(true);
    return () => {
      controller.setViewing(false);
    };
  }, []);

  const busy = v.phase === 'checking' || v.phase === 'joining';
  const status = !mine
    ? 'Not in voice'
    : v.phase === 'connected'
      ? v.listenOnly
        ? 'In voice, listening only'
        : 'In voice'
      : v.phase === 'reconnecting'
        ? 'Reconnecting'
        : v.phase === 'confirm'
          ? 'Waiting for you to confirm'
          : busy
            ? 'Joining'
            : 'Not in voice';

  return (
    <Screen title={'# ' + props.channel} right={<Button label="Back" onPress={props.onBack} />}>
      <Text accessibilityLiveRegion="polite" style={{ fontSize: 17, color: colours.ink }}>
        {status}
      </Text>
      {mine && v.phase === 'confirm' ? (
        <View>
          <Note text={v.warning} kind="warn" />
          <Button label="Join here" kind="primary" onPress={() => void controller.confirmJoin()} />
          <Button label="Cancel" onPress={() => controller.cancelJoin()} />
        </View>
      ) : null}
      {mine ? <Note text={v.why} kind="warn" /> : null}
      {mine ? <Note text={v.note} kind="warn" /> : null}
      {mine && v.blocked.length > 0 ? <Note text={'The microphone is held shut: ' + v.blocked.join(', ') + '.'} /> : null}

      {!inVoice && v.phase !== 'confirm' ? <Button label="Join voice" kind="primary" disabled={busy} onPress={() => void controller.join(props.channel)} /> : null}
      {inVoice && mine ? (
        <View>
          <View style={styles.row}>
            <View style={styles.grow}>
              <Button label={v.muted ? 'Unmute' : 'Mute'} disabled={v.listenOnly} onPress={() => controller.setMuted(!v.muted)} />
            </View>
            <View style={styles.grow}>
              <Button label={v.deafened ? 'Undeafen' : 'Deafen'} onPress={() => controller.setDeafened(!v.deafened)} />
            </View>
          </View>
          <View style={styles.row}>
            <View style={styles.grow}>
              <Button label={v.mode === 'ptt' ? 'Use open microphone' : 'Use push to talk'} disabled={v.listenOnly} onPress={() => controller.setMode(v.mode === 'ptt' ? 'open' : 'ptt')} />
            </View>
          </View>
          {v.mode === 'ptt' ? (
            <HoldToTalk enabled={v.phase === 'connected' && !v.muted && !v.deafened && !v.listenOnly && v.blocked.length === 0} open={v.gateOpen} />
          ) : (
            <Note text={v.gateOpen ? 'Open microphone: sending' : 'Open microphone: waiting for you to speak'} />
          )}
          {v.mode === 'open' && mic !== null ? <LevelProbe track={mic} /> : null}
          <Text style={styles.small}>{v.transmitting ? 'Audio is being sent.' : 'Nothing is being sent.'}</Text>
          <CameraControl />
          <CameraGrid me={me} />
          <Button label="Leave voice" kind="danger" onPress={() => void controller.leave()} />
        </View>
      ) : null}

      {/* Presence follows the joined channel (controller.watch): while in voice
          elsewhere, this view does not show that room's people as this one's. */}
      {!mine && v.phase !== 'idle' ? <Text style={styles.small}>{'You are in voice in # ' + String(v.channel) + '.'}</Text> : null}
      <Text style={styles.label}>In this voice room</Text>
      {shown && v.participants.length === 0 ? <Text style={styles.small}>Nobody.</Text> : null}
      {(shown ? v.participants : []).map((p) => (
        <Text key={p.principal_id} style={{ fontSize: 16, color: colours.ink }}>
          {(p.principal_id === me ? 'You' : 'Person ' + String(p.principal_id)) + (p.transmitting ? ' (transmitting)' : '') + (p.can_publish ? '' : ' (listening)')}
        </Text>
      ))}
      {v.route !== '' ? <Text style={styles.small}>{'Audio devices: ' + v.route}</Text> : null}
    </Screen>
  );
}
