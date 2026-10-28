import { useEffect, useState } from 'react';
import * as api from './api';

// What a temporary server says of itself, fixed and with no dismiss
// (ADR-011 item 2.3).
export const temporaryLine = 'Temporary server: no backups, and it may be removed. Don’t keep anything here you need.';

// The version of the conchd serving this page, which testers quote when they
// report a problem, and on a temporary server the line above. It says nothing
// until the server has answered, and nothing if it never does: the page works
// without it. With className it renders as its own paragraph, and renders no
// empty paragraph when there is nothing. temporary is what whoami said (the
// lobby); /healthz says it too (the sign-in page).
export function ServerVersion({ className, temporary = false }: { className?: string; temporary?: boolean } = {}) {
  const [version, setVersion] = useState<string | null>(null);
  const [mode, setMode] = useState(false);
  useEffect(() => {
    let live = true;
    void api.serverHealth().then((h) => {
      if (!live || h === null) return;
      setVersion(h.version);
      setMode(h.temporary);
    });
    return () => {
      live = false;
    };
  }, []);
  const banner = temporary || mode ? <p className="notice temporary-banner">{temporaryLine}</p> : null;
  const text =
    version === null || version === '' ? null : (
      <span className="version">
        Version <code>{version}</code>
      </span>
    );
  if (className === undefined) {
    return (
      <>
        {text}
        {banner}
      </>
    );
  }
  return (
    <>
      {text === null ? null : <p className={className}>{text}</p>}
      {banner}
    </>
  );
}
