// The Content-Security-Policy of every response the conch-app: scheme serves
// (linux-desktop.md §3). The renderer makes no network request in any child of
// #770: main makes them all, so `connect-src 'none'`. The LiveKit origin is
// the one allowance a later child adds (#772), per load, from the session.
//
// Like conchd's /app/ policy there is no inline script or style, no eval and
// no remote origin; unlike it, this one forbids frames, workers, forms and
// base URLs outright.
export const CONTENT_SECURITY_POLICY = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self'",
  "font-src 'self'",
  "img-src 'self' blob: data:",
  "connect-src 'none'",
  "object-src 'none'",
  "base-uri 'none'",
  "frame-src 'none'",
  "frame-ancestors 'none'",
  "form-action 'none'",
  "worker-src 'none'",
].join('; ');
