import { platform } from './platform';
import type { ChannelV0 } from './schema/channel';
import type { BrowserPermission, NotificationPolicy } from './useNotifications';

// What the chosen policy does, in words (F20).
const policyLine: Record<NotificationPolicy, string> = {
  off: 'Notifications are off.',
  mentions: 'You’re alerted when someone writes your name.',
  all: 'You’re alerted for every new message.',
};

export function NotificationSettings({ policy, native, permission, supported, channels = [], muted = new Set<number>(), onPolicy, onEnable, onToggleMute }: {
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
    </div>
  );
}
