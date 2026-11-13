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
  // The sign-in ends with the tab (an operator's or a guest's): closing the
  // tab would leave a registration with nobody to end it, so none is offered.
  | { readonly kind: 'session-only' }
  // The server has not turned Web Push on, or could not be asked.
  | { readonly kind: 'not-configured' }
  | { readonly kind: 'denied' }
  | { readonly kind: 'off' }
  | { readonly kind: 'working' }
  | { readonly kind: 'on' };

// The step that failed, so the person is told which one.
export type PushFailure = 'permission' | 'subscribe' | 'register' | 'preferences' | 'end' | 'mirror';

// A copy that failed to reach the server is sent again: after the server's
// Retry-After when it gave one, else after a wait that doubles to this bound.
const retryBase = 1000;
const retryMax = 60_000;

// shape is what the server's copy would hold for the local preferences: the
// policy, and the mutes it can hold (the newest, past its bound), in order.
function shape(policy: NotificationPolicy, muted: ReadonlySet<number> | readonly number[]): { readonly policy: NotificationPolicy; readonly muted: number[]; readonly key: string } {
  const kept = api.serverMutes([...muted]);
  return { policy, muted: kept, key: JSON.stringify({ policy, muted: [...kept].sort((a, b) => a - b) }) };
}

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
  // Bumped when the record this origin keeps changes in another tab, and when
  // the credential is replaced: the state is read again.
  const [tick, setTick] = useState(0);
  const vapid = useRef('');
  const live = useRef({ signedIn, eligible, account });
  live.current = { signedIn, eligible, account };
  // What changed locally; what is SENT is read from storage at send time (see
  // stored), so a tab holding an older choice in memory never writes it over a
  // newer one made in another tab.
  const wanted = shape(policy, muted);
  const gone = useRef(false);
  const isGone = () => gone.current;

  // The mirror. sent is what the server's copy last took; known says whether
  // it has been read this load; the timer and notBefore hold a resend.
  const sent = useRef('');
  const known = useRef(false);
  const running = useRef(false);
  const attempts = useRef(0);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const notBefore = useRef(0);
  const checked = useRef('');

  useEffect(() => api.onPushRegistrationChange(() => {
    setTick((previous) => previous + 1);
  }), []);

  useEffect(() => api.onCredentialReplacing((event) => {
    // A password change ends the old credential's registration with it.
    if (event.phase === 'end' && event.rotated) setTick((previous) => previous + 1);
  }), []);

  useEffect(() => {
    const key = `${String(signedIn)}:${String(eligible)}:${String(account)}`;
    // A read again for the same person keeps what the settings show meanwhile.
    const again = checked.current === key;
    checked.current = key;
    if (!again) {
      sent.current = '';
      known.current = false;
      attempts.current = 0;
      vapid.current = '';
      setFailure(null);
    }
    if (!signedIn || !eligible || account === 0) {
      setState({ kind: 'hidden' });
      return;
    }
    // Only a lasting credential is offered it: a tab's ends with the tab,
    // with no sign-out to end the registration.
    if (api.heldPlace() !== 'lasting') {
      setState({ kind: 'session-only' });
      return;
    }
    // Under a lasting credential, the origin's one, another account's record
    // is stale: its subscription would wake this browser for someone who is
    // gone. Dropped here, with no request (that credential is not ours).
    const held = api.readPushRegistration();
    if (held !== null && held.account !== account) void api.forgetPush(held.account);
    const push = platform.services.push;
    if (push === undefined || !push.supported()) {
      setState({ kind: 'unsupported' });
      return;
    }
    const run = { cancelled: false };
    const stale = () => run.cancelled;
    if (!again) setState({ kind: 'checking' });
    void (async () => {
      const config = await api.pushConfig();
      if (stale()) return;
      if (!config.ok || !config.value.transports.includes('webpush') || config.value.vapid_public_key === undefined || config.value.vapid_public_key === '') {
        setState({ kind: 'not-configured' });
        return;
      }
      vapid.current = config.value.vapid_public_key;
      // On means all of: this account's record here, a subscription in the
      // browser and a registration the server still holds. Anything less is
      // cleaned up and reads as off. Another account's record is left alone.
      const record = api.readPushRegistration();
      if (record !== null && record.account === account) {
        const [mine, subscribed] = await Promise.all([api.ownPushRegistrations(), push.subscribed().catch(() => false)]);
        if (stale()) return;
        if (mine.ok && subscribed && mine.value.some((registration) => registration.id === record.id)) {
          setState({ kind: 'on' });
          return;
        }
        // Only a clear answer clears the record: a failed read says nothing.
        if (mine.ok) {
          await api.endPush(account, true);
          if (stale()) return;
        }
      }
      setState({ kind: platform.services.notifications.permission() === 'denied' ? 'denied' : 'off' });
    })();
    return () => {
      run.cancelled = true;
    };
  }, [signedIn, eligible, account, tick]);

  // The server's copy follows the local policy and mutes for as long as this
  // origin holds this account's registration, whichever tab made it, until the
  // server has taken the latest. One send at a time; a refusal (the write
  // budget it shares with registrations, a network fault) is sent again.
  // stored is the newest choice: useNotifications writes it before this hook's
  // effects run, and another tab's is there too.
  const stored = (who: number) => {
    const preferences = api.readNotificationPreferences(platform.services.origin(), who);
    return shape(preferences.policy, preferences.muted);
  };

  const pump = useCallback(async () => {
    if (running.current) return;
    running.current = true;
    clearTimeout(timer.current);
    try {
      for (;;) {
        const now = live.current;
        const record = api.readPushRegistration();
        if (gone.current || !now.signedIn || !now.eligible || api.heldPlace() !== 'lasting' || record === null || record.account !== now.account) return;
        const want = stored(now.account);
        if (sent.current === want.key) {
          attempts.current = 0;
          setFailure((previous) => (previous === 'mirror' ? null : previous));
          return;
        }
        // The first send of a load: a read is on another budget than a write,
        // so when the server's copy already agrees nothing is written.
        if (!known.current) {
          known.current = true;
          const server = await api.serverNotificationPreferences();
          if (server.ok && shape(server.value.policy, new Set(server.value.muted)).key === want.key) {
            sent.current = want.key;
            continue;
          }
        }
        const result = await api.mirrorNotificationPreferences(want.policy, want.muted);
        if (result.ok) {
          sent.current = want.key;
          continue;
        }
        if (result.failure === 'unauthorized' || isGone()) return;
        // Not hidden: until it takes, the server wakes this browser by the
        // earlier copy.
        setFailure('mirror');
        const wait = result.retryAfter === undefined ? Math.min(retryMax, retryBase * 2 ** attempts.current) : Math.min(result.retryAfter * 1000, 5 * retryMax);
        attempts.current++;
        notBefore.current = result.retryAfter === undefined ? 0 : Date.now() + wait;
        timer.current = setTimeout(() => {
          void pump();
        }, wait);
        return;
      }
    } finally {
      running.current = false;
    }
  }, []);

  useEffect(() => {
    void pump();
  }, [pump, wanted.key, state.kind, account, signedIn, eligible, tick]);

  // A copy that could not be sent is tried again when the browser comes back
  // online or the tab is shown, but never before the server's Retry-After.
  useEffect(() => {
    gone.current = false;
    const retry = () => {
      if (document.visibilityState === 'hidden' || Date.now() < notBefore.current) return;
      void pump();
    };
    window.addEventListener('online', retry);
    document.addEventListener('visibilitychange', retry);
    return () => {
      window.removeEventListener('online', retry);
      document.removeEventListener('visibilitychange', retry);
      gone.current = true;
      clearTimeout(timer.current);
    };
  }, [pump]);

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
    // A subscription this account's record does not own may be an earlier
    // sign-in's, with a registration still live: it is replaced, never shared.
    const mine = live.current.account;
    if (api.readPushRegistration()?.account !== mine && (await push.subscribed().catch(() => false))) {
      await push.unsubscribe().catch(() => undefined);
    }
    let target;
    try {
      target = await push.subscribe(vapid.current);
    } catch {
      await fail('subscribe', () => push.unsubscribe());
      return;
    }
    // The policy goes first: preferences with no registration wake nothing,
    // while a registration under the server's earlier copy could.
    const want = stored(mine);
    const mirrored = await api.mirrorNotificationPreferences(want.policy, want.muted);
    if (!mirrored.ok) {
      await fail('preferences', () => push.unsubscribe());
      return;
    }
    const registered = await api.createPushRegistration(target);
    if (!registered.ok) {
      await fail('register', () => push.unsubscribe());
      return;
    }
    api.keepPushRegistration({ id: registered.value.id, account: mine });
    sent.current = want.key;
    known.current = true;
    setState({ kind: 'on' });
  }, []);

  const disable = useCallback(async () => {
    setFailure(null);
    setState({ kind: 'working' });
    const ended = await api.endPush(live.current.account);
    if (!ended) {
      setFailure('end');
      setState({ kind: 'on' });
      return;
    }
    setState({ kind: 'off' });
  }, []);

  return { state, failure, mutesCut: muted.size > api.maxMutedChannels, enable, disable };
}
