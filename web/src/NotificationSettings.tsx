import { platform } from './platform';
import type { ChannelV0 } from './schema/channel';
import type { BrowserPermission, NotificationPolicy } from './useNotifications';
import type { PushFailure, PushState } from './usePush';

// What the chosen policy does, in words (F20).
const policyLine: Record<NotificationPolicy, string> = {
  off: 'Notifications are off.',
  mentions: 'You’re alerted when someone writes your name.',
  all: 'You’re alerted for every new message.',
};

// What each failed step of turning background push on says (issue #1826).
const pushFailureLine: Record<PushFailure, string> = {
  permission: 'Notifications were not allowed, so nothing was turned on.',
  subscribe: 'Your browser could not subscribe to push notifications, so nothing was turned on.',
  register: 'The server did not accept this browser, so nothing was turned on.',
  preferences: 'Your notification settings could not be saved to the server, so nothing was turned on.',
  end: 'It could not be turned off just now. Try again.',
  mirror: 'Your latest notification settings could not be saved to the server yet; it still uses the earlier ones.',
};

// "Notify me when Conch is closed": the section the push state allows, or
// nothing at all when the server offers it to this person never.
function BackgroundPush({ push, policy }: { readonly push: PushSettings; readonly policy: NotificationPolicy }) {
  const { state, failure } = push;
  if (state.kind === 'hidden' || state.kind === 'checking') return null;
  return (
    <div className="background-push">
      <h4>When Conch is closed</h4>
      {state.kind === 'unsupported' ? <p className="hint">Notifications when Conch is closed are not available in this browser.</p> : null}
      {state.kind === 'not-configured' ? <p className="hint">Notifications when Conch is closed are not turned on at this server.</p> : null}
      {state.kind === 'denied' ? <p className="hint">Notifications are blocked for this site. Allow them in your browser settings to be told when Conch is closed.</p> : null}
      {state.kind === 'off' ? (
        <>
          <button type="button" onClick={push.onEnable}>Notify me when Conch is closed</button>
          <p className="hint">The notice only says there is new activity in Conch. It never shows a message, a channel or a name.</p>
        </>
      ) : null}
      {state.kind === 'working' ? <p className="hint" role="status">Working…</p> : null}
      {state.kind === 'on' ? (
        <>
          <p className="hint">You’ll get a notice that there is new activity when Conch is closed, for the messages the setting above allows.{policy === 'off' ? ' It is off above, so nothing will arrive until you choose Mentions only or All messages.' : ''} While a Conch tab is open you may see it as well as the alerts here.</p>
          <button type="button" onClick={push.onDisable}>Stop notifying me when Conch is closed</button>
        </>
      ) : null}
      {failure === null ? null : <p className="hint" role="alert">{pushFailureLine[failure]}</p>}
    </div>
  );
}

export interface PushSettings {
  readonly state: PushState;
  readonly failure: PushFailure | null;
  readonly onEnable: () => void;
  readonly onDisable: () => void;
}

export function NotificationSettings({ policy, native, permission, supported, channels = [], muted = new Set<number>(), push, onPolicy, onEnable, onToggleMute }: {
  readonly push?: PushSettings;
  readonly policy: NotificationPolicy;
  readonly native: boolean;
  readonly permission: BrowserPermission;
  readonly supported: boolean;
  readonly channels?: readonly ChannelV0[];
  readonly muted?: ReadonlySet<number>;
  readonly onPolicy: (policy: NotificationPolicy) => void;
  readonly onEnable: () => void;
  readonly onToggleMute?: (channel: number) => void;
}) {
  const notificationKind = platform.name === 'desktop' ? 'Desktop' : 'Browser';
  return (
    <div className="notification-settings">
      <h3>Notifications</h3>
      <label>
        Message notifications
        <select value={policy} onChange={(event) => {
          onPolicy(event.target.value as NotificationPolicy);
        }}>
          <option value="off">Off</option>
          <option value="mentions">Mentions only</option>
          <option value="all">All messages</option>
        </select>
      </label>
      <p className="hint">{policyLine[policy]} Channel mutes hide alerts, not unread badges.</p>
      {channels.length === 0 || onToggleMute === undefined ? null : (
        <details>
          <summary>Channel mutes</summary>
          <fieldset>
            <legend className="hint">Muted channels</legend>
            {channels.map((channel) => (
              <label key={channel.id}>
                <input
                  type="checkbox"
                  checked={muted.has(channel.id)}
                  onChange={() => {
                    onToggleMute(channel.id);
                  }}
                />
                Mute notifications for #{channel.name}
              </label>
            ))}
          </fieldset>
        </details>
      )}
      {!supported || permission === 'unsupported' ? <p className="hint">{notificationKind} notifications are not supported. Alerts stay here.</p> : null}
      {permission === 'denied' ? <p className="hint">{notificationKind} permission is denied. Alerts stay here; you can allow it in your browser settings.</p> : null}
      {!native && supported && permission !== 'unsupported' ? <button type="button" onClick={onEnable}>Enable {notificationKind.toLowerCase()} notifications</button> : null}
      {native && permission === 'granted' ? <p className="hint">{notificationKind} notifications are on when this tab is in the background. Alerts always remain available here.</p> : null}
      {push === undefined ? null : <BackgroundPush push={push} policy={policy} />}
    </div>
  );
}
