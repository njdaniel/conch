// The iOS spike's app (#1110, M5-C): sign in, the channel list, the voice
// screen and the diagnostics. A state switch, no navigation library.
import { useEffect, useState } from 'react';
import { Channels } from './src/app/ui/Channels';
import { Diagnostics } from './src/app/ui/Diagnostics';
import { SignIn } from './src/app/ui/SignIn';
import { Voice } from './src/app/ui/Voice';
import { bridgeLifecycle, signOut, whenSignedOut } from './src/app/services';

type Screen = { kind: 'signin' } | { kind: 'channels' } | { kind: 'voice'; channel: string } | { kind: 'diagnostics' };

function App() {
  const [screen, setScreen] = useState<Screen>({ kind: 'signin' });

  useEffect(() => {
    bridgeLifecycle();
    // The credential refused: the controller has torn media down (services.ts).
    whenSignedOut(() => {
      setScreen({ kind: 'signin' });
    });
  }, []);

  switch (screen.kind) {
    case 'signin':
      return <SignIn onDone={() => setScreen({ kind: 'channels' })} />;
    case 'channels':
      return (
        <Channels
          onOpen={(channel) => setScreen({ kind: 'voice', channel })}
          onDiagnostics={() => setScreen({ kind: 'diagnostics' })}
          onSignOut={() => {
            void signOut().then(() => {
              setScreen({ kind: 'signin' });
            });
          }}
        />
      );
    case 'voice':
      return <Voice channel={screen.channel} onBack={() => setScreen({ kind: 'channels' })} />;
    case 'diagnostics':
      return <Diagnostics onBack={() => setScreen({ kind: 'channels' })} />;
  }
}

export default App;
