import { beforeEach, describe, expect, it, vi } from 'vitest';

import { DocumentGate } from '../../main/document.ts';
import { Notifications, type NotificationOptions } from '../../main/notifications.ts';
import { NOTIFICATION_EVENTS } from '../../shared/bridge-names.ts';

class FakeNotification {
  static all: FakeNotification[] = [];
  static supported = true;
  readonly shown = vi.fn();
  readonly close = vi.fn(() => {
    if (this.closed) return;
    this.closed = true;
    this.emit('close');
  });
  readonly #listeners = new Map<'click' | 'close', () => void>();
  closed = false;

  constructor(readonly options: NotificationOptions) {
    FakeNotification.all.push(this);
  }

  static isSupported(): boolean {
    return FakeNotification.supported;
  }

  on(event: 'click' | 'close', listener: () => void): this {
    this.#listeners.set(event, listener);
    return this;
  }

  show(): void {
    this.shown();
  }

  emit(event: 'click' | 'close'): void {
    this.#listeners.get(event)?.();
  }
}

function harness() {
  const activation = {};
  const other = {};
  const gate = new DocumentGate();
  gate.begin();
  gate.commit({ frameToken: 'main' });
  const sent: [string, unknown][] = [];
  const window = {
    webContents: { send: (channel: string, event: unknown) => void sent.push([channel, event]) },
    isDestroyed: () => false,
    isMinimized: () => false,
    restore: vi.fn(),
    show: vi.fn(),
    focus: vi.fn(),
  };
  let current = { activation, gate };
  const notifications = new Notifications({
    Notification: FakeNotification,
    icon: '/app/icon.png',
    window: () => window,
    current: () => current,
  });
  const request = { tag: 'tag-1', title: 'Mention in #general', body: 'private message', channel: 'general' };
  return { activation, other, gate, sent, window, notifications, request, setCurrent: (value: { activation: object; gate: DocumentGate }) => { current = value; } };
}

beforeEach(() => {
  FakeNotification.all = [];
  FakeNotification.supported = true;
});

describe('desktop notifications (issue #1590)', () => {
  it('creates and shows only the fixed native fields with the app icon', () => {
    const h = harness();
    expect(h.notifications.show(h.activation, h.gate.generation, h.request)).toBe(true);
    expect(FakeNotification.all).toHaveLength(1);
    expect(FakeNotification.all[0]?.options).toEqual({ title: h.request.title, body: h.request.body, icon: '/app/icon.png' });
    expect(FakeNotification.all[0]?.shown).toHaveBeenCalledOnce();
  });

  it('reports unsupported before constructing a notification', () => {
    const h = harness();
    FakeNotification.supported = false;
    expect(h.notifications.supported()).toBe(false);
    expect(h.notifications.show(h.activation, h.gate.generation, h.request)).toBe(false);
    expect(FakeNotification.all).toHaveLength(0);
  });

  it('closes existing text on lock, redacts while locked, and restores text after unlock', () => {
    const h = harness();
    h.notifications.show(h.activation, h.gate.generation, h.request);
    const before = FakeNotification.all[0];
    h.notifications.setLocked(true);
    expect(before?.closed).toBe(true);
    h.notifications.show(h.activation, h.gate.generation, { ...h.request, tag: 'locked' });
    expect(FakeNotification.all[1]?.options.body).toBe('New message');
    expect(JSON.stringify(FakeNotification.all[1]?.options)).not.toContain('private message');
    h.notifications.setLocked(false);
    h.notifications.show(h.activation, h.gate.generation, { ...h.request, tag: 'unlocked' });
    expect(FakeNotification.all[2]?.options.body).toBe(h.request.body);
  });

  it('focuses the window and routes a current click by tag alone', () => {
    const h = harness();
    h.notifications.show(h.activation, h.gate.generation, h.request);
    FakeNotification.all[0]?.emit('click');
    expect(h.window.show).toHaveBeenCalledOnce();
    expect(h.window.focus).toHaveBeenCalledOnce();
    expect(h.sent).toEqual([[NOTIFICATION_EVENTS, { type: 'click', tag: h.request.tag }]]);
    expect(JSON.stringify(h.sent)).not.toContain(h.request.body);
    expect(JSON.stringify(h.sent)).not.toContain(h.request.channel);
    expect(FakeNotification.all[0]?.closed).toBe(true);
  });

  it('only shows the window for a stale document or another activation', () => {
    const h = harness();
    h.notifications.show(h.activation, h.gate.generation, h.request);
    h.gate.begin();
    FakeNotification.all[0]?.emit('click');
    expect(h.window.show).toHaveBeenCalledOnce();
    expect(h.sent).toEqual([]);

    h.gate.commit({ frameToken: 'main' });
    h.notifications.show(h.activation, h.gate.generation, { ...h.request, tag: 'old-profile' });
    h.setCurrent({ activation: h.other, gate: h.gate });
    FakeNotification.all[1]?.emit('click');
    expect(h.window.show).toHaveBeenCalledTimes(2);
    expect(h.sent).toEqual([]);
  });

  it('closes one by tag and closes every notification of an activation on teardown', () => {
    const h = harness();
    const document = h.gate.generation;
    h.notifications.show(h.activation, document, h.request);
    h.notifications.show(h.activation, document, { ...h.request, tag: 'tag-2' });
    h.notifications.show(h.other, document, { ...h.request, tag: 'other' });
    expect(h.notifications.close(h.activation, document, h.request.tag)).toBe(true);
    expect(FakeNotification.all[0]?.closed).toBe(true);
    expect(FakeNotification.all[1]?.closed).toBe(false);
    h.notifications.closeActivation(h.activation);
    expect(FakeNotification.all[1]?.closed).toBe(true);
    expect(FakeNotification.all[2]?.closed).toBe(false);
    expect(h.notifications.close(h.activation, document, h.request.tag)).toBe(false);
  });
});
