import { useCallback, useEffect, useState } from 'react';
import { FlatList, Pressable, Text, View } from 'react-native';
import type { ChannelV0 } from '../../logic/schema/generated';
import { client, principal } from '../services';
import { Button, Note, Screen, colours, styles } from './common';

export function Channels(props: { onOpen: (channel: string) => void; onDiagnostics: () => void; onSignOut: () => void }) {
  const [channels, setChannels] = useState<readonly ChannelV0[]>([]);
  const [problem, setProblem] = useState<string | null>(null);

  const load = useCallback(async () => {
    const result = await client.channels();
    if (result.ok) {
      setChannels(result.value.channels.filter((c) => c.archived_at === undefined));
      setProblem(null);
    } else if (result.failure !== 'unauthorized') {
      setProblem('Could not load the channels.');
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <Screen title="Channels" right={<Text style={styles.small}>{principal().name}</Text>}>
      <FlatList
        data={channels}
        keyExtractor={(c) => String(c.id)}
        renderItem={({ item }) => (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel={`Open ${item.name}`}
            onPress={() => {
              props.onOpen(item.name);
            }}
            style={{ minHeight: 48, justifyContent: 'center', borderBottomWidth: 1, borderBottomColor: colours.line }}
          >
            <Text style={{ fontSize: 17, color: colours.ink }}>{'# ' + item.name}</Text>
          </Pressable>
        )}
        ListEmptyComponent={<Text style={styles.small}>No channels.</Text>}
      />
      <Note text={problem} kind="warn" />
      <View style={styles.row}>
        <View style={styles.grow}>
          <Button label="Refresh" onPress={() => void load()} />
        </View>
        <View style={styles.grow}>
          <Button label="Diagnostics" onPress={props.onDiagnostics} />
        </View>
      </View>
      <Button label="Sign out" onPress={props.onSignOut} />
    </Screen>
  );
}
