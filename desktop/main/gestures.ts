// Whether the person did something just now (linux-desktop.md §3, §4: "only
// explicit user link actions", "explicit user action for clipboard"). Input:
// the input events Chromium reports for a web contents. Output: an answer to
// "did the person press a button or activate a control in this contents a
// moment ago, and has that act not already been used".
//
// The only source is the browser process's own report of input it received
// from the operating system (webContents' 'input-event'). A script in the page
// cannot make one: dispatchEvent, element.click() and a synthetic event are
// renderer-side and never reach it.
//
// What counts, and for how long:
//  - for a link or a save (take): only an act that activates something, the
//    release of a mouse button, a tap (gestureTap), or Enter or Space going
//    down. Typing, scrolling, moving, the end of a touch (a scroll or a flick
//    ends with one), the other half of a click and a key held down arm nothing;
//  - one act, one action: taking it uses it up, so a second window.open, a
//    second dialog, or a save and then a clipboard write from one click, finds
//    nothing until the person acts again;
//  - for the clipboard (recent): the same acts, and a key chord with Control or
//    Meta (Ctrl+C is a key). It is not used up by a write, because Chromium asks
//    the permission check and the permission request about one write; but a
//    write that is allowed spends the act for links and saves, and a link or a
//    save spends it for the clipboard. Accepted residual, wider than one write:
//    within the 1.5 s window a compromised page may write as many times as it
//    likes (the last write wins); every mouse release and every Ctrl or Meta
//    chord arms it, so a Ctrl+V can be answered at once by an overwrite, and
//    Enter or Space (sending a message) arms it too. A link is not so lightly
//    let go: a key-armed link asks first (links.ts). The gate here is for the
//    async Clipboard API only: document.execCommand('copy') is gated by
//    Blink's own transient activation, not by Electron's permission handlers,
//    so main cannot refuse it;
//  - it ends with the document: a navigation of the contents, or the render
//    process going, drops it.
import type { InputEvent } from 'electron';

// The keys that activate a link or a button, by the event's key and by its
// physical code.
const ACTIVATE_KEYS = new Set(['Enter', ' ']);
const ACTIVATE_CODES = new Set(['Enter', 'NumpadEnter', 'Space']);

// The part of an input event this reads: what Electron emits on 'input-event'
// (captured from a real one: {"type":"rawKeyDown","key":"Enter","code":"Enter",
// "isAutoRepeat":false,"modifiers":[]}). There is no keyCode on it; that is a
// field of the objects sendInputEvent takes.
export interface InputLike {
  readonly type: string;
  readonly key?: string;
  readonly code?: string;
  readonly isAutoRepeat?: boolean;
  readonly modifiers?: readonly string[];
}

function repeating(input: InputLike): boolean {
  return input.isAutoRepeat === true || (input.modifiers ?? []).includes('isautorepeat');
}

// What armed an act: a pointer (a click or a tap), or a key (Enter or Space).
export type GestureKind = 'pointer' | 'key';

function pointer(input: InputLike): boolean {
  return input.type === 'mouseUp' || input.type === 'gestureTap';
}

function activation(input: InputLike): boolean {
  return (
    (input.type === 'rawKeyDown' || input.type === 'keyDown') &&
    ((input.key !== undefined && ACTIVATE_KEYS.has(input.key)) || (input.code !== undefined && ACTIVATE_CODES.has(input.code))) &&
    !repeating(input)
  );
}

// arms says whether one input event is the act of activating a control: what
// a link or a save follows.
export function arms(input: InputLike): boolean {
  return pointer(input) || activation(input);
}

// armsCopy is arms, and a chord with Control or Meta, for the clipboard.
export function armsCopy(input: InputLike): boolean {
  if (arms(input)) return true;
  const modifiers = input.modifiers ?? [];
  const chord = modifiers.some((m) => m === 'control' || m === 'ctrl' || m === 'meta' || m === 'command' || m === 'cmd');
  return (input.type === 'rawKeyDown' || input.type === 'keyDown') && chord && !repeating(input);
}

// The part of a web contents that is listened to.
export interface Listening {
  on(event: 'input-event', listener: (event: unknown, input: InputEvent) => void): unknown;
  on(event: 'did-start-navigation', listener: (details: { readonly isSameDocument?: boolean }) => void): unknown;
  on(event: 'render-process-gone', listener: () => void): unknown;
}

export class Gestures {
  readonly #now: () => number;
  // The time of the unused act of each contents, if there is one: for a link
  // or a save, and for the clipboard.
  readonly #act = new WeakMap<object, { readonly at: number; readonly kind: GestureKind }>();
  readonly #copy = new WeakMap<object, number>();

  constructor(now: () => number) {
    this.#now = now;
  }

  // attach starts listening to one web contents.
  attach(contents: Listening): void {
    contents.on('input-event', (_event, input) => {
      const like = input as InputLike;
      if (arms(like)) this.#act.set(contents, { at: this.#now(), kind: pointer(like) ? 'pointer' : 'key' });
      if (armsCopy(like)) this.#copy.set(contents, this.#now());
    });
    // A new document, or none: whatever the person did was for the old one.
    // (A same-document navigation, a fragment or a pushState, is the page's
    // own route change after a click: the document is the same.)
    contents.on('did-start-navigation', (details) => {
      if (details.isSameDocument !== true) this.clear(contents);
    });
    contents.on('render-process-gone', () => {
      this.clear(contents);
    });
  }

  clear(contents: object): void {
    this.#act.delete(contents);
    this.#copy.delete(contents);
  }

  #within(at: number | undefined, withinMs: number): boolean {
    if (at === undefined) return false;
    const age = this.#now() - at;
    return age >= 0 && age <= withinMs;
  }

  // take uses up the contents' act if it was within the last withinMs, and
  // says what kind it was, or null if there was none. An act too old is
  // dropped as well. The same act is spent for the clipboard too.
  take(contents: object | null, withinMs: number): GestureKind | null {
    if (contents === null) return null;
    const act = this.#act.get(contents);
    this.#act.delete(contents);
    this.#copy.delete(contents);
    return act !== undefined && this.#within(act.at, withinMs) ? act.kind : null;
  }

  // recent says whether the contents has an unused act within the last
  // withinMs, for the clipboard, without using it up; when it has, the act is
  // spent for links and saves.
  recent(contents: object | null, withinMs: number): boolean {
    if (contents === null) return false;
    const ok = this.#within(this.#copy.get(contents), withinMs);
    if (ok) this.#act.delete(contents);
    return ok;
  }
}

// How long after the act a link or a save still counts as its result: they
// follow the click in the same turn.
export const LINK_GESTURE_MS = 1_000;
// The copy buttons (AddDevice, InvitePanel) write in the click handler itself.
export const CLIPBOARD_GESTURE_MS = 1_500;
