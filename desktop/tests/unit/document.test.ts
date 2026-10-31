import { EventEmitter } from 'node:events';

import { describe, expect, it } from 'vitest';

import { DocumentGate, trackDocument } from '../../main/document.ts';

describe('the document gate', () => {
  it('knows the one frame that shows the current document', () => {
    const gate = new DocumentGate();
    expect(gate.current({ frameToken: 'a' })).toBe(false);
    gate.commit({ frameToken: 'a' });
    expect(gate.current({ frameToken: 'a' })).toBe(true);
    expect(gate.current({ frameToken: 'b' })).toBe(false);
    const before = gate.generation;
    gate.begin();
    expect(gate.generation).toBe(before + 1);
    expect(gate.current({ frameToken: 'a' })).toBe(false);
  });

  it('follows the window: a navigation ends the document, the app’s own page commits a new one, same-document changes do not end it', () => {
    const contents = Object.assign(new EventEmitter(), { mainFrame: { frameToken: 'f1' } });
    const gate = new DocumentGate();
    trackDocument(contents as never, gate);
    contents.emit('did-start-navigation', { isMainFrame: true, isSameDocument: false });
    contents.emit('did-navigate', {}, 'conch-app://ui/');
    expect(gate.current({ frameToken: 'f1' })).toBe(true);
    contents.emit('did-start-navigation', { isMainFrame: true, isSameDocument: true });
    contents.emit('did-start-navigation', { isMainFrame: false, isSameDocument: false });
    expect(gate.current({ frameToken: 'f1' })).toBe(true);
    contents.emit('did-start-navigation', { isMainFrame: true, isSameDocument: false });
    expect(gate.current({ frameToken: 'f1' })).toBe(false);
    // A reload of a page with a route in its fragment is still the app's page.
    contents.emit('did-start-navigation', { isMainFrame: true, isSameDocument: false });
    contents.emit('did-navigate', {}, 'conch-app://ui/#/c/general');
    expect(gate.current({ frameToken: 'f1' })).toBe(true);
    contents.emit('did-start-navigation', { isMainFrame: true, isSameDocument: false });
    // A page that is not the app's never commits.
    contents.emit('did-navigate', {}, 'https://evil.example/');
    expect(gate.current({ frameToken: 'f1' })).toBe(false);
    contents.emit('did-navigate', {}, 'conch-app://ui/');
    contents.emit('render-process-gone');
    expect(gate.current({ frameToken: 'f1' })).toBe(false);
  });

  it('tells what it holds for a document that the document has ended: at a navigation, a reload, a crash and the window going away', () => {
    const contents = Object.assign(new EventEmitter(), { mainFrame: { frameToken: 'f1' } });
    const gate = new DocumentGate();
    const ended: number[] = [];
    gate.onEnd((current) => ended.push(current));
    trackDocument(contents as never, gate);
    contents.emit('did-start-navigation', { isMainFrame: true, isSameDocument: false });
    contents.emit('did-navigate', {}, 'conch-app://ui/');
    const shown = gate.generation;
    expect(gate.shows(shown)).toBe(true);
    expect(gate.shows(shown - 1)).toBe(false);
    // A same-document change (the client's routes) ends nothing.
    contents.emit('did-start-navigation', { isMainFrame: true, isSameDocument: true });
    expect(ended).toEqual([shown]);
    contents.emit('did-start-navigation', { isMainFrame: true, isSameDocument: false });
    expect(gate.shows(shown)).toBe(false);
    contents.emit('did-navigate', {}, 'conch-app://ui/');
    contents.emit('render-process-gone');
    contents.emit('destroyed');
    expect(ended).toEqual([shown, shown + 1, shown + 2, shown + 3]);
    // Before a document commits, nothing may be sent to it.
    expect(gate.shows(gate.generation)).toBe(false);
  });
});
