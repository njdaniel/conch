// formatSize writes a byte count the way a file chip shows it: "812 B",
// "3.4 KB", "10 MB". Decimal units, as the server's limit is stated in bytes.
export function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '';
  if (bytes < 1000) return `${String(Math.round(bytes))} B`;
  const units = ['KB', 'MB', 'GB'];
  let value = bytes / 1000;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit++;
  }
  const text = value >= 10 ? String(Math.round(value)) : value.toFixed(1).replace(/\.0$/, '');
  return `${text} ${units[unit] ?? 'GB'}`;
}

// uploadRefusalLine is the one line shown for a refused upload. The server's
// 413 (file_too_large) says its limit in bytes; the line names the file and
// states that limit.
export function uploadRefusalLine(filename: string, code: string | undefined, reason: string): string {
  if (code !== 'file_too_large') return reason;
  const limit = /(\d+)\s*bytes/.exec(reason)?.[1];
  return limit === undefined
    ? `${filename} is too large for this server.`
    : `${filename} is too large: this server takes files up to ${formatSize(Number(limit))}.`;
}

// isPreviewType reports whether a declared media type is one the browser
// previews (PNG, JPEG). It is a hint only: bytes are always sniffed.
export function isPreviewType(mediaType: string): boolean {
  return mediaType === 'image/png' || mediaType === 'image/jpeg';
}
