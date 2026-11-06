// The diagnostics screen and its redacted export: a bounded ring of what the app
// and the SDK said, scrubbed of the bearer, the address and device ids
// (logic/diagnostics.ts). Sharing hands the text to the iOS share sheet.
import { useState } from 'react';
import { ScrollView, Share, Text } from 'react-native';
import { controller, diagnostics } from '../services';
import { useVoiceState } from './useVoiceState';
import { Button, Screen, styles } from './common';

export function Diagnostics(props: { onBack: () => void }) {
  const [text, setText] = useState(() => diagnostics.export());
  const voice = useVoiceState(controller);
  const status = [
    `phase ${voice.phase}`,
    `mode ${voice.mode}`,
    `muted ${voice.muted}`,
    `deafened ${voice.deafened}`,
    `gate ${voice.gateOpen ? 'open' : 'shut'}`,
    `blocked ${voice.blocked.length === 0 ? 'no' : voice.blocked.join('+')}`,
    `route ${voice.route === '' ? '(none yet)' : voice.route}`,
    // What iOS actually delivered (the receipt records it).
    `delivered ${controller.audio.delivered.length === 0 ? '(nothing)' : '\n  ' + controller.audio.delivered.join('\n  ')}`,
  ].join('\n');
  return (
    <Screen title="Diagnostics">
      <Text style={styles.small}>Redacted: no sign-in, address or device id.</Text>
      <ScrollView style={{ flex: 1, marginTop: 8 }}>
        <Text selectable style={styles.mono}>
          {status + '\n\n' + (text === '' ? '(nothing yet)' : text)}
        </Text>
      </ScrollView>
      <Button label="Refresh" onPress={() => setText(diagnostics.export())} />
      <Button
        label="Share export"
        onPress={() => {
          void Share.share({ message: diagnostics.export() });
        }}
      />
      <Button label="Back" onPress={props.onBack} />
    </Screen>
  );
}
