// Which document of the window may call main (linux-desktop.md §4: "the exact
// local origin and the expected document generation"). Input: the window's
// navigation events. Output: a gate that knows the one frame whose document
// is the current one.
//
// A document generation begins when the main frame starts a navigation to
// another document and is committed, with the frame that now shows it, when
// that navigation commits to the app's own page. Between the two, and for a
// frame that has been replaced, no call is accepted: a call from the page
// that has just been left cannot be answered into the page that replaces it.
import type { WebContents } from 'electron';

import { INITIAL_URL } from './scheme.ts';

export interface FrameToken {
  readonly frameToken: string;
}

export class DocumentGate {
  #generation = 0;
  #frame: string | null = null;

  get generation(): number {
    return this.#generation;
  }

  // begin ends the current document.
  begin(): void {
    this.#generation++;
    this.#frame = null;
  }

  // commit makes the frame the one that shows the current document.
  commit(frame: FrameToken): void {
    this.#frame = frame.frameToken;
  }

  // current says whether frame is the one that shows the current document.
  current(frame: FrameToken): boolean {
    return this.#frame !== null && this.#frame === frame.frameToken;
  }
}

// trackDocument keeps a gate in step with a web contents.
export function trackDocument(contents: WebContents, gate: DocumentGate): void {
  contents.on('did-start-navigation', (details) => {
    if (details.isMainFrame && !details.isSameDocument) gate.begin();
  });
  contents.on('did-navigate', (_event, url) => {
    // A reload keeps the fragment the client routes by ("#/c/general"); it is
    // still the app's own page.
    if (url.split('#')[0] === INITIAL_URL) gate.commit(contents.mainFrame);
  });
  contents.on('render-process-gone', () => {
    gate.begin();
  });
  contents.on('destroyed', () => {
    gate.begin();
  });
}
