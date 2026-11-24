import type { Plugin } from 'vite';

// The web app manifest is for the browser build served at /app/. The desktop
// shell loads the build from its own scheme under a CSP of default-src 'none'
// (desktop/main/csp.ts), which blocks the manifest fetch, and the manifest's
// /app/ paths do not exist there, so `--mode desktop` leaves the link out.
export function manifestLink(mode: string): Plugin {
  return {
    name: 'conch-manifest-link',
    transformIndexHtml(html) {
      if (mode !== 'desktop') return html;
      return html.replace(/[ \t]*<link rel="manifest"[^>]*>\r?\n?/g, '');
    },
  };
}
