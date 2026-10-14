// How times read in the panel and in run detail.

// ago is how long before now `at` was, as a short phrase (the TUI's).
export function ago(now: number, at: string): string {
  const ms = now - Date.parse(at);
  if (!(ms >= 5000)) return 'just now';
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${String(s)}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${String(m)}m ago`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${String(h)}h ago`;
  return `${String(Math.floor(h / 24))}d ago`;
}

const two = (n: number) => String(n).padStart(2, '0');

// clockSeconds is HH:MM:SS in the reader's own time zone.
export function clockSeconds(at: string): string {
  const d = new Date(at);
  return `${two(d.getHours())}:${two(d.getMinutes())}:${two(d.getSeconds())}`;
}
