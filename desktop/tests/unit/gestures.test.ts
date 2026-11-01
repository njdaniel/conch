import { EventEmitter } from 'node:events';

import { describe, expect, it } from 'vitest';

import { arms, armsCopy, CLIPBOARD_GESTURE_MS, Gestures, LINK_GESTURE_MS } from '../../main/gestures.ts';

function contents(): EventEmitter {
  return new EventEmitter();
}

// Payloads as Electron really emits them on 'input-event' (captured from a
// headless Electron): a key has key, code and isAutoRepeat, and no keyCode.
interface Press {
  type: string;
  key?: string;
  code?: string;
  isAutoRepeat?: boolean;
  control?: boolean;
  clickCount?: number;
  button?: string;
  x?: number;
  y?: number;
  modifiers: string[];
}

const keyEvent = (type: string, key: string, code: string, over: Partial<Press> = {}): Press => ({ type, key, code, isAutoRepeat: false, modifiers: [], ...over });
const ENTER = keyEvent('rawKeyDown', 'Enter', 'Enter');
const SPACE = keyEvent('rawKeyDown', ' ', 'Space');
const CTRL_C = keyEvent('rawKeyDown', 'c', 'KeyC', { control: true, modifiers: ['control'] });
const META_C = keyEvent('rawKeyDown', 'c', 'KeyC', { modifiers: ['meta'] });
const REPEAT = keyEvent('rawKeyDown', 'Enter', 'Enter', { isAutoRepeat: true, modifiers: ['isautorepeat'] });
const MOUSE_UP: Press = { type: 'mouseUp', clickCount: 1, button: 'left', x: 10, y: 10, modifiers: [] };
const plain = (type: string, extra: Partial<Press> = {}): Press => ({ type, modifiers: [], ...extra });

const press = (c: EventEmitter, input: Press): void => {
  c.emit('input-event', {}, input);
};

describe('what arms a gesture', () => {
  it.each([
    ['a mouse release', MOUSE_UP],
    ['a tap', plain('gestureTap')],
    ['Enter', ENTER],
    ['Enter as keyDown', keyEvent('keyDown', 'Enter', 'Enter')],
    ['the numpad Enter', keyEvent('rawKeyDown', 'Enter', 'NumpadEnter')],
    ['Space', SPACE],
    ['Space by code alone', keyEvent('rawKeyDown', 'Unidentified', 'Space')],
  ])('%s arms a link or a save', (_name, input) => {
    expect(arms(input)).toBe(true);
    expect(armsCopy(input)).toBe(true);
  });

  it.each([
    ['a letter', keyEvent('rawKeyDown', 'a', 'KeyA')],
    ['an auto-repeating Enter', REPEAT],
    ['an auto-repeating Enter by flag alone', keyEvent('rawKeyDown', 'Enter', 'Enter', { isAutoRepeat: true })],
    ['an auto-repeating Enter by modifier alone', keyEvent('rawKeyDown', 'Enter', 'Enter', { modifiers: ['isautorepeat'] })],
    ['Enter as char', keyEvent('char', 'Enter', 'Enter')],
    ['Space as char', keyEvent('char', ' ', 'Space')],
    ['Enter released', keyEvent('keyUp', 'Enter', 'Enter')],
    ['Space released', keyEvent('keyUp', ' ', 'Space')],
    ['a mouse press', plain('mouseDown')],
    ['a mouse move', plain('mouseMove')],
    ['a wheel', plain('mouseWheel')],
    ['a mouse enter', plain('mouseEnter')],
    ['a scroll', plain('gestureScrollUpdate')],
    ['a touch move', plain('touchMove')],
    ['the end of a touch (every scroll or flick ends with one)', plain('touchEnd')],
    ['a pointer release (mouseUp and gestureTap are the real input)', plain('pointerUp')],
    ['Ctrl+C', CTRL_C],
    ['an unknown type', plain('undefined')],
    ['the old sendInputEvent shape (keyCode only)', { type: 'rawKeyDown', keyCode: 'Enter', modifiers: [] } as unknown as Press],
  ])('%s does not', (_name, input) => {
    expect(arms(input)).toBe(false);
  });

  it('a Control or Meta chord arms the clipboard only (Ctrl+C is a key)', () => {
    expect(armsCopy(CTRL_C)).toBe(true);
    expect(armsCopy(META_C)).toBe(true);
    expect(armsCopy(keyEvent('rawKeyDown', 'c', 'KeyC'))).toBe(false);
    expect(armsCopy(keyEvent('rawKeyDown', 'c', 'KeyC', { modifiers: ['shift'] }))).toBe(false);
    expect(armsCopy({ ...CTRL_C, isAutoRepeat: true })).toBe(false);
  });
});

describe('Gestures.take, for a link or a save', () => {
  it('lets a click be used once, for a short while', () => {
    const clock = { now: 1_000 };
    const gestures = new Gestures(() => clock.now);
    const c = contents();
    gestures.attach(c);
    expect(gestures.take(c, LINK_GESTURE_MS)).toBeNull();
    press(c, MOUSE_UP);
    clock.now += LINK_GESTURE_MS;
    expect(gestures.take(c, LINK_GESTURE_MS)).toBe('pointer');
    expect(gestures.take(c, LINK_GESTURE_MS)).toBeNull();
  });

  it('drops an act that is too old, and does not keep it for a longer window', () => {
    const clock = { now: 1_000 };
    const gestures = new Gestures(() => clock.now);
    const c = contents();
    gestures.attach(c);
    press(c, MOUSE_UP);
    clock.now += LINK_GESTURE_MS + 1;
    expect(gestures.take(c, LINK_GESTURE_MS)).toBeNull();
    expect(gestures.take(c, CLIPBOARD_GESTURE_MS)).toBeNull();
  });

  it('typing letters never authorizes anything', () => {
    const gestures = new Gestures(() => 5);
    const c = contents();
    gestures.attach(c);
    for (const ch of 'helloworld'.split('')) {
      const code = `Key${ch.toUpperCase()}`;
      press(c, keyEvent('rawKeyDown', ch, code));
      press(c, keyEvent('char', ch, code));
      press(c, keyEvent('keyUp', ch, code));
    }
    expect(gestures.take(c, 60_000)).toBeNull();
    expect(gestures.recent(c, 60_000)).toBe(false);
  });

  it('says whether a click or a key armed it', () => {
    const gestures = new Gestures(() => 5);
    const c = contents();
    gestures.attach(c);
    press(c, MOUSE_UP);
    expect(gestures.take(c, 100)).toBe('pointer');
    press(c, plain('gestureTap'));
    expect(gestures.take(c, 100)).toBe('pointer');
    press(c, ENTER);
    expect(gestures.take(c, 100)).toBe('key');
    press(c, SPACE);
    expect(gestures.take(c, 100)).toBe('key');
    press(c, ENTER);
    press(c, MOUSE_UP);
    expect(gestures.take(c, 100)).toBe('pointer');
  });

  it('a touch scroll or flick arms nothing, though it ends with touchEnd', () => {
    const gestures = new Gestures(() => 5);
    const c = contents();
    gestures.attach(c);
    for (const type of ['touchStart', 'touchMove', 'touchMove', 'touchScrollStarted', 'gestureScrollBegin', 'gestureScrollUpdate', 'touchEnd']) press(c, plain(type));
    expect(gestures.take(c, 100)).toBeNull();
    expect(gestures.recent(c, 100)).toBe(false);
    press(c, plain('gestureTap'));
    expect(gestures.take(c, 100)).toBe('pointer');
  });

  it('a new act replaces the old, never adds to it', () => {
    const gestures = new Gestures(() => 5);
    const c = contents();
    gestures.attach(c);
    press(c, MOUSE_UP);
    press(c, MOUSE_UP);
    expect(gestures.take(c, 100)).toBe('pointer');
    expect(gestures.take(c, 100)).toBeNull();
  });
});

describe('Gestures.recent, for the clipboard', () => {
  it('answers both the check and the request about one write, then the window ends', () => {
    const clock = { now: 0 };
    const gestures = new Gestures(() => clock.now);
    const c = contents();
    gestures.attach(c);
    press(c, MOUSE_UP);
    expect(gestures.recent(c, CLIPBOARD_GESTURE_MS)).toBe(true);
    expect(gestures.recent(c, CLIPBOARD_GESTURE_MS)).toBe(true);
    clock.now += CLIPBOARD_GESTURE_MS + 1;
    expect(gestures.recent(c, CLIPBOARD_GESTURE_MS)).toBe(false);
  });

  it('a clipboard write and a save or link cannot share one gesture, in either order', () => {
    const gestures = new Gestures(() => 5);
    const c = contents();
    gestures.attach(c);
    press(c, MOUSE_UP);
    expect(gestures.recent(c, 100)).toBe(true);
    expect(gestures.take(c, 100)).toBeNull();
    press(c, MOUSE_UP);
    expect(gestures.take(c, 100)).toBe('pointer');
    expect(gestures.recent(c, 100)).toBe(false);
  });

  it('a chord arms the clipboard but not a link or a save', () => {
    const gestures = new Gestures(() => 5);
    const c = contents();
    gestures.attach(c);
    press(c, CTRL_C);
    expect(gestures.take(c, 100)).toBeNull();
    press(c, CTRL_C);
    expect(gestures.recent(c, 100)).toBe(true);
  });
});

describe('Gestures in general', () => {
  it('ends with the document: a navigation or a lost renderer drops it', () => {
    const gestures = new Gestures(() => 5);
    const c = contents();
    gestures.attach(c);
    press(c, MOUSE_UP);
    c.emit('did-start-navigation', { isSameDocument: false });
    expect(gestures.take(c, 100)).toBeNull();
    press(c, MOUSE_UP);
    c.emit('render-process-gone');
    expect(gestures.recent(c, 100)).toBe(false);
  });

  it('survives a same-document navigation (the page\u2019s own route change), and ends at a new document', () => {
    const gestures = new Gestures(() => 5);
    const c = contents();
    gestures.attach(c);
    press(c, MOUSE_UP);
    c.emit('did-start-navigation', { isSameDocument: true, url: 'conch-app://ui/#/channel/general', isMainFrame: true });
    expect(gestures.take(c, 100)).toBe('pointer');
    press(c, MOUSE_UP);
    c.emit('did-start-navigation', { isSameDocument: false, url: 'conch-app://ui/', isMainFrame: true });
    expect(gestures.take(c, 100)).toBeNull();
  });

  it('keeps each contents apart, and knows none it was not given', () => {
    const gestures = new Gestures(() => 5);
    const a = contents();
    const b = contents();
    gestures.attach(a);
    press(a, MOUSE_UP);
    expect(gestures.take(b, 100)).toBeNull();
    expect(gestures.take(null, 100)).toBeNull();
    expect(gestures.recent(null, 100)).toBe(false);
    expect(gestures.take(a, 100)).toBe('pointer');
  });
});
