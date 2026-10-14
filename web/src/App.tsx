// The browser client's top level: sign-in, the operator warning, then the
// lobby. The auth, connection and channels reducers hold the state; src/api.ts
// does every call and is the only code that sees the credential.
import { useCallback, useEffect, useReducer, useState } from 'react';
import * as api from './api';
import { Lobby } from './Lobby';
import { OperatorWarning } from './OperatorWarning';
import { channelFromHash, forgetChannel, hashForChannel, rememberChannel, rememberedChannel } from './route';
import { SignIn } from './SignIn';
import { authReducer, initialAuth } from './state/auth';
import { channelsReducer, initialChannels } from './state/channels';
import { connectionReducer } from './state/connection';

export function App({ origin }: { readonly origin: string }) {
  const [auth, dispatchAuth] = useReducer(authReducer, undefined, () => initialAuth(api.hasToken()));
  const [connection, dispatchConnection] = useReducer(connectionReducer, 'signedOut');
  const [channels, dispatchChannels] = useReducer(channelsReducer, initialChannels);
  const [attempt, setAttempt] = useState(0);

  // A credential left by an earlier load of this tab is checked once.
  const restoring = auth.phase === 'restoring';
  useEffect(() => {
    if (!restoring) return;
    let live = true;
    void api.restore().then((result) => {
      if (!live) return;
      if (result.ok) dispatchAuth({ type: 'verified', principal: result.value });
      else dispatchAuth({ type: 'failed', failure: result.failure });
    });
    return () => {
      live = false;
    };
  }, [restoring]);

  const signedIn = auth.phase === 'signedIn';

  // The lobby loads the caller's channels. A 401 here, or anywhere later,
  // sends the page back to sign-in.
  useEffect(() => {
    if (!signedIn) return;
    let live = true;
    if (attempt === 0) dispatchConnection({ type: 'started' });
    void api.listChannels().then((result) => {
      if (!live) return;
      if (result.ok) {
        dispatchConnection({ type: 'reached' });
        dispatchChannels({
          type: 'loaded',
          channels: result.value,
          preferred: channelFromHash(window.location.hash),
          remembered: rememberedChannel(),
        });
      } else if (result.failure === 'unauthorized') {
        dispatchAuth({ type: 'unauthorized' });
      } else {
        dispatchConnection({ type: 'lost' });
      }
    });
    return () => {
      live = false;
    };
  }, [signedIn, attempt]);

  useEffect(() => {
    if (auth.phase === 'signedOut') {
      dispatchConnection({ type: 'closed' });
      dispatchChannels({ type: 'reset' });
      setAttempt(0);
    }
  }, [auth.phase]);

  // The hash names the open channel, so back, forward and a pasted link work.
  useEffect(() => {
    if (!signedIn) return;
    function onHash() {
      const name = channelFromHash(window.location.hash);
      if (name !== null) dispatchChannels({ type: 'select', name });
    }
    window.addEventListener('hashchange', onHash);
    return () => {
      window.removeEventListener('hashchange', onHash);
    };
  }, [signedIn]);

  useEffect(() => {
    if (channels.selected === null) return;
    rememberChannel(channels.selected);
    const hash = hashForChannel(channels.selected);
    if (window.location.hash !== hash) window.location.hash = hash;
  }, [channels.selected]);

  const signOut = useCallback(() => {
    api.signOut();
    forgetChannel();
    dispatchAuth({ type: 'signOut' });
  }, []);

  const submit = useCallback((credential: string) => {
    dispatchAuth({ type: 'submit' });
    void api.signIn(credential).then((result) => {
      if (result.ok) dispatchAuth({ type: 'verified', principal: result.value });
      else dispatchAuth({ type: 'failed', failure: result.failure });
    });
  }, []);

  switch (auth.phase) {
    case 'restoring':
      return (
        <main className="signin">
          <h1>Conch</h1>
          <p role="status">Checking your sign-in…</p>
        </main>
      );
    case 'signedOut':
    case 'checking':
      return (
        <SignIn
          origin={origin}
          notice={auth.phase === 'signedOut' ? auth.notice : null}
          checking={auth.phase === 'checking'}
          onSubmit={submit}
        />
      );
    case 'operatorWarning':
      return (
        <OperatorWarning
          name={auth.principal.name}
          onContinue={() => {
            if (api.continueAsOperator()) dispatchAuth({ type: 'continue' });
            else dispatchAuth({ type: 'failed', failure: 'invalid' });
          }}
          onSignOut={signOut}
        />
      );
    case 'signedIn':
      return (
        <Lobby
          origin={origin}
          name={auth.principal.name}
          channels={channels}
          connection={connection}
          onOpen={(name) => {
            dispatchChannels({ type: 'select', name });
          }}
          onRetry={() => {
            setAttempt((n) => n + 1);
          }}
          onSignOut={signOut}
        />
      );
  }
}
