// The desktop shell's gate in front of the client: the "Server address" step.
// The desktop window has no server of its own to be served from, so before the
// existing sign-in views can run the person says which server to talk to. Main
// checks the address (https, or http for this machine only) and keeps it; the
// client below then runs unchanged against it.
//
// Once an address is set, if main already holds a sign-in for it (the page was
// reloaded) the stand-in credential is restored before the client starts, so
// that the client's own restore finds it.
import { useEffect, useId, useRef, useState, type ReactNode, type SyntheticEvent } from 'react';
import { signedIn } from './credentials';
import { bridge } from './access';
import type { ServerAddressRefusal } from './bridge';
import { setServerOrigin } from './state';

// Written in two parts: the page's source holds no absolute URL.
const example = ['https:', 'chat.example.org'].join('//');

const refusalLines: Readonly<Record<ServerAddressRefusal, string>> = {
  malformed: `That is not a web address. Write it like ${example}`,
  scheme: 'Use a secure (https) address. A plain http address is accepted only for this computer (localhost, 127.0.0.1 or [::1]).',
  userinfo: 'Leave the user name and password out of the address.',
  path: 'Give the server’s address alone: no path, query or #fragment after the host.',
  unreachable: 'That server could not be reached.',
  unsupported: 'That address is not supported.',
};

const certificateLine =
  'This computer does not trust that server’s certificate, so nothing was sent. If the server uses a private certificate authority, install it in the system’s certificate store (see desktop/README.md), then try again.';

type Phase = 'loading' | 'address' | 'ready' | 'missing';

export function DesktopShell({ children }: { readonly children: ReactNode }) {
  const [phase, setPhase] = useState<Phase>('loading');
  const [line, setLine] = useState('');
  const [busy, setBusy] = useState(false);
  const field = useRef<HTMLInputElement>(null);
  const id = useId();

  // enter makes the client's stand-in credential if main holds a sign-in.
  async function enter(origin: string): Promise<void> {
    setServerOrigin(origin);
    const b = bridge();
    const restored = b === undefined ? null : await b.restore().catch(() => null);
    if (restored?.ok === true && restored.who !== null) signedIn(restored.who.role === 'member' && restored.who.guest === undefined);
    setPhase('ready');
  }

  useEffect(() => {
    let live = true;
    const b = bridge();
    if (b === undefined) {
      setPhase('missing');
      return;
    }
    void b
      .serverAddress()
      .then(async (address) => {
        if (!live) return;
        if (address.origin === null) setPhase('address');
        else await enter(address.origin);
      })
      .catch(() => {
        if (live) setPhase('missing');
      });
    return () => {
      live = false;
    };
  }, []);

  useEffect(() => {
    if (phase === 'address') field.current?.focus();
  }, [phase, line]);

  async function submit(event: SyntheticEvent): Promise<void> {
    event.preventDefault();
    const b = bridge();
    const typed = field.current?.value.trim() ?? '';
    if (b === undefined || busy) return;
    if (typed === '') {
      field.current?.focus();
      return;
    }
    setBusy(true);
    setLine('');
    try {
      const set = await b.setServerAddress(typed);
      if (!set.ok) {
        setLine(refusalLines[set.refusal]);
        return;
      }
      // Ask the server who it is before the client starts, so that a
      // certificate that is not trusted, or an address that is no Conch
      // server, is said here and not as a sign-in that never answers.
      const reply = await b.request({ operation: 'health' });
      if (reply.kind === 'none') {
        if (reply.reason === 'certificate') setLine(certificateLine);
        else if (reply.reason === 'invalid' || reply.reason === 'oversize' || reply.reason === 'redirect') setLine(`${set.origin} answered, but not as a Conch server.`);
        else setLine(`Could not reach ${set.origin}. Check the address and the connection.`);
        return;
      }
      if (reply.kind === 'reply' && reply.status !== 200) {
        setLine(`${set.origin} answered, but not as a Conch server.`);
        return;
      }
      await enter(set.origin);
    } catch {
      setLine('The desktop app could not check that address.');
    } finally {
      setBusy(false);
    }
  }

  if (phase === 'ready') return <>{children}</>;
  if (phase === 'loading') {
    return (
      <main className="signin">
        <h1>Conch</h1>
        <p role="status">Starting…</p>
      </main>
    );
  }
  if (phase === 'missing') {
    return (
      <main className="signin">
        <h1>Conch</h1>
        <p role="alert" className="notice">
          The desktop app’s connection to its main process is missing, so there is nothing to sign in to. Restart the app.
        </p>
      </main>
    );
  }
  return (
    <main className="signin">
      <h1>Conch</h1>
      <h2>Server address</h2>
      <p className="hint">The address of the Conch server you use. Your sign-in is kept by the app and is gone when it closes.</p>
      <p role="alert" className="notice">
        {line}
      </p>
      <form
        onSubmit={(event) => {
          void submit(event);
        }}
      >
        <label htmlFor={id}>Server address</label>
        <input id={id} ref={field} type="text" inputMode="url" autoComplete="off" spellCheck={false} placeholder={example} />
        <button type="submit" disabled={busy}>
          Continue
        </button>
      </form>
    </main>
  );
}
