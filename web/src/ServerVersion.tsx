import { useEffect, useState } from 'react';
import * as api from './api';

// The version of the conchd serving this page, which testers quote when they
// report a problem. It says nothing until the server has answered, and nothing
// if it never does: the page works without it.
export function ServerVersion() {
  const [version, setVersion] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    void api.serverVersion().then((v) => {
      if (live) setVersion(v);
    });
    return () => {
      live = false;
    };
  }, []);
  if (version === null || version === '') return null;
  return (
    <span className="version">
      Version <code>{version}</code>
    </span>
  );
}
