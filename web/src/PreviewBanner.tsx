// The persistent band of Preview as member (issue #1517,
// docs/design/permission-preview.md §7.2): who is being previewed, that it is
// read-only, and the two ways out of it. It stays on every phone view, under
// the top bar, and its buttons are 44px there.
import type { PreviewStore } from './preview/store';
import { loadedLine } from './preview/words';

export function PreviewBanner({
  store,
  problem,
  onExit,
  onRefresh,
}: {
  readonly store: PreviewStore;
  readonly problem: string | null;
  readonly onExit: () => void;
  readonly onRefresh: () => void;
}) {
  return (
    <div className="preview-band" role="region" aria-label="Preview">
      <p>
        Previewing {store.target.name}, read-only <span className="hint">{loadedLine(store.loadedAt)}</span>
      </p>
      <button type="button" onClick={onRefresh}>
        Refresh
      </button>
      <button type="button" className="primary" onClick={onExit}>
        Exit preview
      </button>
      {problem === null ? null : (
        <p role="status" className="problem">
          {problem}
        </p>
      )}
    </div>
  );
}
