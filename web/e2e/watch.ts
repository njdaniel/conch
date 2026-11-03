// What a page is sent, for the specs that look for a leak (A14, issue #447;
// C991, issue #991): every frame on every socket it opens (LiveKit's included)
// and the body of every answer from the API, each with where it came from.
import type { Page } from '@playwright/test';

// A binary frame (LiveKit's) is bytes; decoded one character per byte, a name
// in it reads as itself.
const latin1 = new TextDecoder('latin1');

interface Entry {
  // The API path an answer came from; empty for a socket frame.
  readonly path: string;
  readonly text: string;
}

export interface Traffic {
  // Everything received so far, joined.
  readonly text: () => Promise<string>;
  // Everything except the answers of one API path: where a name is allowed to
  // be, so that it turning up anywhere else is a leak.
  readonly textExcept: (path: string) => Promise<string>;
}

// watchTraffic records what page is sent from now on. Call it before the page
// loads.
export function watchTraffic(page: Page): Traffic {
  const seen: Entry[] = [];
  const reads: Promise<void>[] = [];
  page.on('websocket', (ws) => {
    ws.on('framereceived', (frame) => {
      const payload: unknown = frame.payload;
      seen.push({ path: '', text: typeof payload === 'string' ? payload : latin1.decode(payload as Uint8Array) });
    });
  });
  page.on('response', (res) => {
    // The API's answers; the client's own files are the same for everyone.
    const path = new URL(res.url()).pathname;
    if (!/^\/v[0-9]+\//.test(path)) return;
    reads.push(
      res.text().then(
        (body) => {
          seen.push({ path, text: body });
        },
        () => undefined,
      ),
    );
  });
  return {
    text: async () => {
      await Promise.all(reads);
      return seen.map((e) => e.text).join('\n');
    },
    textExcept: async (path) => {
      await Promise.all(reads);
      return seen.filter((e) => e.path !== path).map((e) => e.text).join('\n');
    },
  };
}
