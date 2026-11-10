// The notice that the installed files were replaced under this running app
// (issue #1600, D6-06). Main decides (desktop/main/installed.ts) and sends the
// state; this shows it and, on the person's click, asks main to restart. The
// app never restarts by itself, and a call goes on until the click.
import { useEffect, useState } from 'react';

import { bridge } from '../access';
import type { DesktopUpdateBridge, InstalledView } from '../bridge';
import { restartLabel, updatedLine } from './text';

// The update half of the bridge, or undefined when the preload does not offer
// all of it.
function updateBridge(): DesktopUpdateBridge | undefined {
  const whole = bridge();
  if (whole === undefined) return undefined;
  const b: Partial<DesktopUpdateBridge> = whole as object;
  if (typeof b.updateState !== 'function' || typeof b.updateRestart !== 'function' || typeof b.onUpdateState !== 'function') return undefined;
  return b as DesktopUpdateBridge;
}

export function UpdateNotice() {
  const [view, setView] = useState<InstalledView>({ updated: false, version: null });

  useEffect(() => {
    const b = updateBridge();
    if (b === undefined) return;
    let live = true;
    const off = b.onUpdateState((next) => {
      if (live) setView(next);
    });
    b.updateState().then(
      (now) => {
        if (live) setView(now);
      },
      () => undefined,
    );
    return () => {
      live = false;
      off();
    };
  }, []);

  if (!view.updated) return null;
  return (
    <section role="status" aria-label="Update" className="notice desktop-update">
      <p>{updatedLine(view.version)}</p>
      <button
        type="button"
        onClick={() => {
          // Main quits through its normal teardown and relaunches.
          void updateBridge()
            ?.updateRestart()
            .catch(() => undefined);
        }}
      >
        {restartLabel}
      </button>
    </section>
  );
}
