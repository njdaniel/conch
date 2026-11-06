import { useId, useState } from 'react';
import { Glyph } from './Icons';
import type { NotificationAlert } from './useNotifications';

export function NotificationCenter({ alerts, onOpen, onClear }: {
  readonly alerts: readonly NotificationAlert[];
  readonly onOpen: (id: string) => void;
  readonly onClear: () => void;
}) {
  const id = useId();
  const [open, setOpen] = useState(false);
  return (
    <div className="notification-center">
      <button type="button" className="icon-button" aria-expanded={open} aria-controls={`${id}-panel`} onClick={() => {
        setOpen(!open);
      }}>
        <Glyph name="bell" />
        <span className="label">Notifications{alerts.length === 0 ? '' : ` (${String(alerts.length)})`}</span>
      </button>
      <section id={`${id}-panel`} aria-label="Notifications" hidden={!open} className="notification-panel">
        <div className="drawer-head">
          <h2>Notifications</h2>
          {alerts.length === 0 ? null : <button type="button" onClick={onClear}>Clear all</button>}
        </div>
        <p role="status" className="hint">{alerts.length === 0 ? 'No new notifications.' : `${String(alerts.length)} unread`}</p>
        <ul aria-label="Recent notifications">
          {alerts.map((alert) => (
            <li key={alert.id}>
              <button type="button" aria-label={`${alert.mention ? 'Mention' : 'New message'} in #${alert.channel} ${alert.message.body}`} onClick={() => {
                onOpen(alert.id);
              }}>
                <strong>{alert.mention ? 'Mention' : 'New message'} in #{alert.channel}</strong>
                <span>{alert.message.body}</span>
              </button>
            </li>
          ))}
        </ul>
      </section>
    </div>
  );
}
