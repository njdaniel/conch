import { useCallback, useEffect, useRef, useState } from 'react';
import * as api from './api';
import type { NotificationPolicy } from './api';
import { platform } from './platform';

// Background push for this browser (issue #1826): "Notify me when Conch is
// closed". The state is what the settings say; the work is in enable, disable
// and the mirror of the local policy to the server's copy, which the sender
// reads to decide whether to wake this browser. The service worker itself
// (public/sw.js) holds no credential and shows one fixed notification.
export type PushState =
  // A guest or an agent: the server refuses them, so nothing is offered.
  | { readonly kind: 'hidden' }
  | { readonly kind: 'checking' }
  // This browser has no service workers or push.
  | { readonly kind: 'unsupported' }
  // The server has not turned Web Push on, or could not be asked.
  | { readonly kind: 'not-configured' }
  | { readonly kind: 'denied' }
  | { readonly kind: 'off' }
  | { readonly kind: 'working' }
  | { readonly kind: 'on' };

// The step that failed, so the person is told which one.
export type PushFailure = 'permission' | 'subscribe' | 'register' | 'preferences' | 'end' | 'mirror';

export function usePush({
  signedIn,
  eligible,
  account,
  policy,
  muted,
}: {
  readonly signedIn: boolean;
  // Whether the server offers push to this person: a human with a full
  // credential.
  readonly eligible: boolean;
  readonly account: number;
  readonly policy: NotificationPolicy;
  readonly muted: ReadonlySet<number>;
}) {
  const [state, setState] = useState<PushState>({ kind: 'hidden' });
  const [failure, setFailure] = useState<PushFailure | null>(null);
  const vapid = useRef('');
  // What the server's copy last took, so a change is sent once.
  const mirrored = useRef('');
  const chain = useRef<Promise<unknown>>(Promise.resolve());
  const latest = useRef({ policy, muted });
  latest.current = { policy, muted };

  const mutedKey = [...muted].sort((a, b) => a - b);
  const wanted = JSON.stringify({ policy, muted: mutedKey });

  useEffect(() => {
    mirrored.current = '';
    vapid.current = '';
    setFailure(null);
    if (!signedIn || !eligible || account === 0) {
      setState({ kind: 'hidden' });
      return;
    }
    const push = platform.services.push;
    if (push === undefined || !push.supported()) {
      setState({ kind: 'unsupported' });
      return;
    }
    const run = { cancelled: false };
    const stale = () => run.cancelled;
    setState({ kind: 'checking' });
    void (async () => {
      const config = await api.pushConfig();
      if (stale()) return;
      if (!config.ok || !config.value.transports.includes('webpush') || config.value.vapid_public_key === undefined || config.value.vapid_public_key === '') {
        setState({ kind: 'not-configured' });
        return;
      }
      vapid.current = config.value.vapid_public_key;
      // On means all of: a record here, a subscription in the browser and a
      // registration the server still holds. Anything less is cleaned up and
      // reads as off.
      const id = api.readPushRegistration();
      if (id !== null) {
        const [mine, subscribed] = await Promise.all([api.ownPushRegistrations(), push.subscribed().catch(() => false)]);
        if (stale()) return;
        if (mine.ok && subscribed && mine.value.some((registration) => registration.id === id)) {
          setState({ kind: 'on' });
          return;
        }
        // Only a clear answer clears the record: a failed read says nothing.
        if (mine.ok) {
          await api.endPush(true);
          if (stale()) return;
        }
      }
      setState({ kind: platform.services.notifications.permission() === 'denied' ? 'denied' : 'off' });
    })();
    return () => {
      run.cancelled = true;
    };
  }, [signedIn, eligible, account]);

  // While push is on, the server's copy follows the local policy and mutes.
  // The first run after load resyncs it too, which is harmless.
  useEffect(() => {
    if (state.kind !== 'on' || mirrored.current === wanted) return;
    mirrored.current = wanted;
    chain.current = chain.current.then(async () => {
      const { policy: p, muted: m } = latest.current;
      const result = await api.mirrorNotificationPreferences(p, [...m]);
      if (result.ok) {
        setFailure((previous) => (previous === 'mirror' ? null : previous));
      } else {
        // Sent again on the next change, and not hidden: until it takes, the
        // server wakes this browser by the old policy.
        mirrored.current = '';
        setFailure('mirror');
      }
    });
  }, [state.kind, wanted]);

  const enable = useCallback(async () => {
    const push = platform.services.push;
    if (push === undefined || vapid.current === '') return;
    setFailure(null);
    setState({ kind: 'working' });
    const fail = async (step: PushFailure, undo: () => Promise<unknown>) => {
      await undo().catch(() => undefined);
      setFailure(step);
      setState({ kind: platform.services.notifications.permission() === 'denied' ? 'denied' : 'off' });
    };
    // Permission, on this click. A refusal leaves nothing registered.
    if (platform.services.notifications.permission() !== 'granted') {
      const answer = await platform.services.notifications.request();
      if (answer !== 'granted') {
        await fail('permission', () => Promise.resolve());
        return;
      }
    }
    let target;
    try {
      target = await push.subscribe(vapid.current);
    } catch {
      await fail('subscribe', () => push.unsubscribe());
      return;
    }
    const registered = await api.createPushRegistration(target);
    if (!registered.ok) {
      await fail('register', () => push.unsubscribe());
      return;
    }
    // From here a registration exists: a failure undoes it as well.
    const { policy: p, muted: m } = latest.current;
    const mirror = await api.mirrorNotificationPreferences(p, [...m]);
    if (!mirror.ok) {
      await fail('preferences', async () => {
        await api.endPushRegistration(registered.value.id);
        await push.unsubscribe();
      });
      return;
    }
    api.keepPushRegistration(registered.value.id);
    mirrored.current = JSON.stringify({ policy: p, muted: [...m].sort((a, b) => a - b) });
    setState({ kind: 'on' });
  }, []);

  const disable = useCallback(async () => {
    setFailure(null);
    setState({ kind: 'working' });
    const ended = await api.endPush();
    if (!ended) {
      setFailure('end');
      setState({ kind: 'on' });
      return;
    }
    setState({ kind: 'off' });
  }, []);

  return { state, failure, enable, disable };
}
