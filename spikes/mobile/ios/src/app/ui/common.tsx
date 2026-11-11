import type { ReactNode } from 'react';
import { Pressable, StyleSheet, Text, View } from 'react-native';
import { useAnnounceText } from './useAnnounce';

export const colours = {
  ink: '#101418',
  muted: '#59636e',
  line: '#c9d1d9',
  paper: '#ffffff',
  accent: '#0b57d0',
  warn: '#9a3412',
  warnPaper: '#fff4e5',
  hot: '#b3261e',
};

// Every control is at least 44 dp (M-A2).
export function Button(props: { label: string; onPress: () => void; disabled?: boolean; kind?: 'primary' | 'plain' | 'danger'; hint?: string }) {
  const kind = props.kind ?? 'plain';
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={props.label}
      accessibilityHint={props.hint}
      accessibilityState={{ disabled: props.disabled === true }}
      disabled={props.disabled}
      onPress={props.onPress}
      style={[styles.button, kind === 'primary' && styles.primary, kind === 'danger' && styles.danger, props.disabled === true && styles.disabled]}
    >
      <Text style={[styles.buttonText, kind !== 'plain' && styles.buttonTextOn]}>{props.label}</Text>
    </Pressable>
  );
}

export function Screen(props: { title: string; children: ReactNode; right?: ReactNode }) {
  return (
    <View style={styles.screen}>
      <View style={styles.header}>
        <Text accessibilityRole="header" style={styles.title}>
          {props.title}
        </Text>
        {props.right}
      </View>
      {props.children}
    </View>
  );
}

export function Note(props: { text: string | null; kind?: 'warn' | 'plain' }) {
  useAnnounceText(props.text);
  if (props.text === null || props.text === '') return null;
  return (
    <View style={[styles.note, props.kind === 'warn' && styles.noteWarn]} accessibilityLiveRegion="polite">
      <Text style={styles.noteText}>{props.text}</Text>
    </View>
  );
}

export const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: colours.paper, padding: 16, paddingTop: 40 },
  header: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 },
  title: { fontSize: 22, fontWeight: '600', color: colours.ink },
  label: { fontSize: 14, color: colours.muted, marginTop: 12, marginBottom: 4 },
  input: { borderWidth: 1, borderColor: colours.line, borderRadius: 6, paddingHorizontal: 12, minHeight: 48, fontSize: 16, color: colours.ink },
  button: { minHeight: 48, minWidth: 48, paddingHorizontal: 16, borderRadius: 6, borderWidth: 1, borderColor: colours.line, alignItems: 'center', justifyContent: 'center', marginTop: 8 },
  primary: { backgroundColor: colours.accent, borderColor: colours.accent },
  danger: { backgroundColor: colours.hot, borderColor: colours.hot },
  disabled: { opacity: 0.45 },
  buttonText: { fontSize: 16, color: colours.ink },
  buttonTextOn: { color: '#ffffff' },
  note: { borderRadius: 6, borderWidth: 1, borderColor: colours.line, padding: 12, marginTop: 12 },
  noteWarn: { backgroundColor: colours.warnPaper, borderColor: colours.warn },
  noteText: { fontSize: 15, color: colours.ink },
  row: { flexDirection: 'row', gap: 8 },
  grow: { flex: 1 },
  small: { fontSize: 13, color: colours.muted },
  mono: { fontFamily: 'monospace', fontSize: 12, color: colours.ink },
});
