// Sign-in: a password, or a one-use sign-in link. The bearer the server answers
// with is held in the client's memory (client.ts) and nowhere else; the server
// address is typed here and is not stored either.
import { useState } from 'react';
import { ScrollView, Text, TextInput, View } from 'react-native';
import { client, signedIn } from '../services';
import { Button, Note, Screen, styles } from './common';

const refusals: Readonly<Record<string, string>> = {
  auth_failed: 'The name or password is not right.',
  sign_in_link_invalid: 'That link no longer works. Ask for a new one.',
  rate_limited: 'Too many tries. Wait a minute and try again.',
  link_other_server: 'That link is for a different server than the one typed above. Check the address, or ask for a new link.',
  link_malformed: 'That link is not written correctly. Paste it again, or paste just its code.',
};

export function SignIn(props: { onDone: () => void }) {
  const [server, setServer] = useState('');
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [link, setLink] = useState('');
  const [problem, setProblem] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const finish = async (): Promise<void> => {
    const me = await client.whoami();
    if (!me.ok) {
      setProblem('Signed in, but the server did not say who you are.');
      return;
    }
    signedIn(me.value.id, me.value.name);
    setPassword('');
    setLink('');
    props.onDone();
  };

  const words = (r: { failure: string; code?: string; status?: number }): string => {
    if (r.failure === 'network') return 'Could not reach the server. Check the address and that this phone can reach it.';
    if (r.code !== undefined && refusals[r.code] !== undefined) return refusals[r.code] ?? '';
    return `The server refused (${r.status ?? 'no status'}).`;
  };

  const withPassword = async (): Promise<void> => {
    setBusy(true);
    setProblem(null);
    client.setServer(server);
    const result = await client.login(name.trim(), password);
    if (!result.ok) {
      setProblem(words(result));
      setPassword('');
      setLink('');
    } else if (result.value.next === 'totp') {
      setProblem('This account uses a second step. The spike does not support it; use a sign-in link.');
    } else {
      await finish();
    }
    setBusy(false);
  };

  const withLink = async (): Promise<void> => {
    setBusy(true);
    setProblem(null);
    client.setServer(server);
    const result = await client.redeemSignInLink(link);
    if (!result.ok) {
      setProblem(words(result));
      setPassword('');
      setLink('');
    } else await finish();
    setBusy(false);
  };

  return (
    <Screen title="Sign in">
      <ScrollView keyboardShouldPersistTaps="handled">
        <Text style={styles.label}>Server</Text>
        <TextInput
          style={styles.input}
          value={server}
          onChangeText={setServer}
          autoCapitalize="none"
          autoCorrect={false}
          keyboardType="url"
          accessibilityLabel="Server address"
          placeholder="https://… or http://… (development)"
        />
        <Text style={styles.label}>Name</Text>
        <TextInput style={styles.input} value={name} onChangeText={setName} autoCapitalize="none" autoCorrect={false} accessibilityLabel="Name" />
        <Text style={styles.label}>Password</Text>
        <TextInput style={styles.input} value={password} onChangeText={setPassword} secureTextEntry autoCapitalize="none" autoCorrect={false} accessibilityLabel="Password" />
        <Button label="Sign in" kind="primary" disabled={busy || server.trim() === '' || name.trim() === '' || password === ''} onPress={() => void withPassword()} />
        <View style={{ height: 16 }} />
        <Text style={styles.label}>Or a one-use sign-in link</Text>
        <TextInput
          style={styles.input}
          value={link}
          onChangeText={setLink}
          autoCapitalize="none"
          autoCorrect={false}
          accessibilityLabel="Sign-in link"
          placeholder="Paste the link or its code"
        />
        <Button label="Use the link" disabled={busy || server.trim() === '' || link.trim() === ''} onPress={() => void withLink()} />
        <Note text={problem} kind="warn" />
        <Note text="Your sign-in is kept in memory only. Closing the app signs you out." />
      </ScrollView>
    </Screen>
  );
}
