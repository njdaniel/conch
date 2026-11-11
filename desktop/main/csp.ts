// The Content-Security-Policy of every response the conch-app: scheme serves
// (linux-desktop.md §3, as amended by Q-D4-1 on #1578, Nick, 2026-11-09).
// Main makes every request to conchd. The one network the renderer reaches
// itself is the screen companion's LiveKit (#772, model B: its JS SDK
// signals over wss and makes plain https requests to the same origin), so
// `connect-src wss: https:`, and main's request filter (guards.ts) lets
// through exactly the live companion grant's origin while that grant lives;
// every other wss or https origin, and every one when no grant is live, is
// cancelled there. One fixed header, not one per load: the origin is the
// filter's to name, not the CSP's. No plain ws: or http:.
//
// Like conchd's /app/ policy there is no inline script or style, no eval and
// no remote script, style, image or font; unlike it, this one forbids frames,
// workers, forms and base URLs outright.
export const CONTENT_SECURITY_POLICY = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self'",
  "font-src 'self'",
  "img-src 'self' blob: data:",
  'connect-src wss: https:',
  "object-src 'none'",
  "base-uri 'none'",
  "frame-src 'none'",
  "frame-ancestors 'none'",
  "form-action 'none'",
  "worker-src 'none'",
].join('; ');
