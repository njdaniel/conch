import { act, cleanup, fireEvent, render, renderHook, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Message } from './Conversation';
import { NotificationCenter } from './NotificationCenter';
import { NotificationSettings } from './NotificationSettings';
import * as api from './api';
import { mentions, useNotifications, type NotificationAlert } from './useNotifications';
import { platform } from './platform';
import { SPOILER_FILE_PREFIX } from './schema/file';
import { forgetFileNames, rememberFileName } from './spoilerFiles';
import type { MessageV2 } from './schema/message';

const at = '2026-10-26T00:00:00Z';
const channels = [{ id: 7, name: 'general', created_at: at }];
const message = (id: number, body = 'hello', author = 3): MessageV2 => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: author, created_at: at, body });
const alert = (id = 'one'): NotificationAlert => ({ id, channel: 'general', message: message(1, 'hello @ada'), mention: true });

class FakeNotification {
  static all: FakeNotification[] = [];
  static permission: NotificationPermission = 'default';
  static answer: NotificationPermission = 'granted';
  static requestPermission = vi.fn(() => Promise.resolve(FakeNotification.answer));
  readonly closed = vi.fn();
  constructor(readonly title: string, readonly options: NotificationOptions) {
    FakeNotification.all.push(this);
  }
  close(): void {
    this.closed();
  }
}

type Props = Parameters<typeof useNotifications>[0];
const props = (): Props => ({
  signedIn: true,
  origin: 'https://one.example',
  principal: { id: 2, name: 'ada' },
  channels,
  loaded: true,
  eventsLive: true,
  event: null,
  readStateEvent: null,
  onOpen: vi.fn(),
});

function visibility(value: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value });
}

beforeEach(() => {
  window.localStorage.clear();
  FakeNotification.all = [];
  FakeNotification.permission = 'default';
  FakeNotification.answer = 'granted';
  FakeNotification.requestPermission = vi.fn(() => Promise.resolve(FakeNotification.answer));
  vi.stubGlobal('Notification', FakeNotification);
  visibility('visible');
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  visibility('visible');
});

describe('mentions', () => {
  it.each([
    ['hello @ada', true],
    ['hello @ADA!', true],
    ['@ada, yes', true],
    ['hello @adam', false],
    ['hello @ada-more', false],
    ['hello ada', false],
    ['hello @bob', false],
  ])('%s', (body, want) => {
    expect(mentions(body, 'ada')).toBe(want);
    // The row's mark follows the same matcher, case for case (issue #1043).
    const { container } = render(
      <ol>
        <Message message={message(1, body, 3)} people={new Map([[2, { name: 'ada', kind: 'human' }], [3, { name: 'bob', kind: 'human' }]])} nets={new Map()} viewer={2} channel="general" sent={false} cont={false} />
      </ol>,
    );
    expect(container.querySelector('li.mentions-you') !== null).toBe(want);
    expect(screen.queryByText('Mentions you.') !== null).toBe(want);
  });
});

describe('notification policies and preferences', () => {
  it('is off by default, honors all/mentions/mute, and caps history', () => {
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    const emit = (m: MessageV2) => {
      rerender({ ...props(), event: m });
    };
    emit(message(1));
    expect(result.current.alerts).toHaveLength(0);
    act(() => {
      result.current.setPolicy('all');
    });
    for (let id = 2; id <= 13; id++) emit(message(id));
    expect(result.current.alerts).toHaveLength(10);
    expect(result.current.alerts[0]?.message.id).toBe(4);
    act(() => {
      result.current.setPolicy('mentions');
    });
    emit(message(14, 'hello @adam'));
    expect(result.current.alerts).toHaveLength(10);
    emit(message(15, 'hello @ADA!'));
    expect(result.current.alerts).toHaveLength(10);
    expect(result.current.alerts.at(-1)?.mention).toBe(true);
    act(() => {
      result.current.toggleMute(7);
    });
    expect(result.current.alerts).toHaveLength(0);
    expect(result.current.muted.has(7)).toBe(true);
    emit(message(16, 'hello @ada'));
    expect(result.current.alerts).toHaveLength(0);
    act(() => {
      result.current.toggleMute(7);
    });
    emit(message(17, 'hello @ada'));
    expect(result.current.alerts).toHaveLength(1);
  });

  it('persists by account and falls back to off for corrupt or another account', async () => {
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('all');
    });
    act(() => {
      result.current.toggleMute(7);
    });
    await act(async () => {
      await result.current.enableNative();
    });
    const stored = window.localStorage.getItem('conch.web.notifications.v1.https://one.example.2');
    expect(stored).toContain('"policy":"all"');
    expect(stored).toContain('"native":true');
    expect(stored).toContain('"muted":[7]');
    rerender({ ...props(), principal: { id: 3, name: 'bob' } });
    expect(result.current.policy).toBe('off');
    expect(result.current.native).toBe(false);
    expect(result.current.alerts).toHaveLength(0);
    window.localStorage.setItem('conch.web.notifications.v1.https://one.example.3', '{broken');
    rerender({ ...props(), principal: { id: 3, name: 'bob' } });
    expect(result.current.policy).toBe('off');
  });

  it('keeps working in memory when browser storage is blocked', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('Blocked', 'SecurityError');
    });
    const { result } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('all');
    });
    expect(result.current.policy).toBe('all');
  });

  it('does not alert for your own post and applies each event once', () => {
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('all');
    });
    rerender({ ...props(), event: message(1, 'mine', 2) });
    expect(result.current.alerts).toHaveLength(0);
    const event = message(2);
    rerender({ ...props(), event });
    expect(result.current.alerts).toHaveLength(1);
    rerender({ ...props(), event });
    expect(result.current.alerts).toHaveLength(1);
  });
});

describe('browser permission and cleanup', () => {
  it('drops stale alert copies on disconnect and backfills only unseen messages', async () => {
    visibility('hidden');
    const history = vi.spyOn(api, 'loadMessagesAscending').mockResolvedValue({ ok: true, complete: true, value: [message(1)] });
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    await act(async () => {
      await Promise.resolve();
    });
    act(() => {
      result.current.setPolicy('all');
    });
    await act(async () => {
      await result.current.enableNative();
    });
    rerender({ ...props(), event: message(2) });
    expect(FakeNotification.all).toHaveLength(1);
    expect(result.current.alerts).toHaveLength(1);
    rerender({ ...props(), eventsLive: false });
    expect(result.current.alerts).toHaveLength(0);
    expect(FakeNotification.all[0]?.closed).toHaveBeenCalled();
    history.mockResolvedValue({ ok: true, complete: true, value: [message(2), message(3), message(4)] });
    rerender({ ...props(), eventsLive: true });
    await vi.waitFor(() => {
      expect(result.current.alerts.map((item) => item.message.id)).toEqual([3, 4]);
    });
    expect(history).toHaveBeenLastCalledWith('general', 1);
    expect(FakeNotification.all).toHaveLength(3);
  });

  it('restarts backfill when a channel is renamed during reconciliation', async () => {
    const history = vi.spyOn(api, 'loadMessagesAscending').mockResolvedValue({ ok: true, complete: true, value: [message(1)] });
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    await act(async () => {
      await Promise.resolve();
    });
    act(() => {
      result.current.setPolicy('all');
    });
    rerender({ ...props(), eventsLive: false });
    history.mockReturnValueOnce(new Promise(() => undefined));
    rerender({ ...props(), eventsLive: true });
    history.mockResolvedValue({ ok: true, complete: true, value: [message(2)] });
    rerender({ ...props(), channels: [{ id: 7, name: 'renamed', created_at: at }] });
    await vi.waitFor(() => {
      expect(result.current.alerts.map((item) => item.message.id)).toEqual([2]);
    });
    expect(history).toHaveBeenLastCalledWith('renamed', 1);
  });

  it('does not restore alerts from a history read that finishes after revocation', async () => {
    const history = vi.spyOn(api, 'loadMessagesAscending').mockResolvedValue({ ok: true, complete: true, value: [message(1)] });
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    await act(async () => {
      await Promise.resolve();
    });
    act(() => {
      result.current.setPolicy('all');
    });
    rerender({ ...props(), eventsLive: false });
    let finish!: (value: api.AscendingMessagesResult) => void;
    history.mockReturnValueOnce(new Promise((resolve) => {
      finish = resolve;
    }));
    rerender({ ...props(), eventsLive: true });
    rerender({ ...props(), readStateEvent: { schema: 'conch.read_state.v1', channel_id: 7, last_read_id: 0, unread: 0 } });
    await act(async () => {
      finish({ ok: true, complete: true, value: [message(2)] });
      await Promise.resolve();
    });
    expect(result.current.alerts).toHaveLength(0);
  });

  it('closes evicted native alerts and all remaining alerts on unmount', async () => {
    visibility('hidden');
    const { result, rerender, unmount } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('all');
    });
    await act(async () => {
      await result.current.enableNative();
    });
    for (let id = 1; id <= 11; id++) rerender({ ...props(), event: message(id) });
    expect(FakeNotification.all).toHaveLength(11);
    expect(FakeNotification.all[0]?.closed).toHaveBeenCalledTimes(1);
    unmount();
    for (const notification of FakeNotification.all) expect(notification.closed).toHaveBeenCalledTimes(1);
  });

  it('opens the channel and closes the alert when a native notification is clicked', async () => {
    visibility('hidden');
    const onOpen = vi.fn();
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: { ...props(), onOpen } });
    act(() => {
      result.current.setPolicy('all');
    });
    await act(async () => {
      await result.current.enableNative();
    });
    rerender({ ...props(), onOpen, event: message(1) });
    const native = FakeNotification.all[0] as unknown as Notification;
    act(() => {
      native.onclick?.call(native, new MouseEvent('click'));
    });
    expect(onOpen).toHaveBeenCalledWith('general');
    expect(result.current.alerts).toHaveLength(0);
    expect(FakeNotification.all[0]?.closed).toHaveBeenCalledTimes(1);
  });

  it('keeps the in-app fallback when permission is denied', async () => {
    visibility('hidden');
    FakeNotification.answer = 'denied';
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('all');
    });
    await act(async () => {
      await result.current.enableNative();
    });
    expect(result.current.browserPermission).toBe('denied');
    expect(result.current.native).toBe(false);
    rerender({ ...props(), event: message(1) });
    expect(result.current.alerts).toHaveLength(1);
    expect(FakeNotification.all).toHaveLength(0);
  });

  it('clears native and in-app alerts on a zero read-state tombstone', async () => {
    visibility('hidden');
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('all');
    });
    await act(async () => {
      await result.current.enableNative();
    });
    rerender({ ...props(), event: message(1) });
    expect(result.current.alerts).toHaveLength(1);
    rerender({ ...props(), readStateEvent: { schema: 'conch.read_state.v1', channel_id: 7, last_read_id: 0, unread: 0 } });
    expect(result.current.alerts).toHaveLength(0);
    expect(FakeNotification.all[0]?.closed).toHaveBeenCalled();
  });

  it('clears native and in-app alerts on channel removal and account switch', async () => {
    visibility('hidden');
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('all');
    });
    await act(async () => {
      await result.current.enableNative();
    });
    rerender({ ...props(), event: message(1) });
    expect(result.current.alerts).toHaveLength(1);
    rerender({ ...props(), channels: [] });
    expect(result.current.alerts).toHaveLength(0);
    expect(FakeNotification.all[0]?.closed).toHaveBeenCalled();
    rerender({ ...props(), principal: { id: 3, name: 'bob' } });
    expect(result.current.alerts).toHaveLength(0);
    expect(result.current.policy).toBe('off');
  });

  it('opens and clears alerts through the accessible center', () => {
    const onOpen = vi.fn();
    const onClear = vi.fn();
    render(<NotificationCenter alerts={[alert()]} onOpen={onOpen} onClear={onClear} />);
    fireEvent.click(screen.getByRole('button', { name: 'Notifications (1)' }));
    fireEvent.click(screen.getByRole('button', { name: /Mention in #general/ }));
    expect(onOpen).toHaveBeenCalledWith('one');
    fireEvent.click(screen.getByRole('button', { name: 'Clear all' }));
    expect(onClear).toHaveBeenCalled();
  });

  it('reads an image-only spoiler as [spoiler] and its shown name, never the image', () => {
    const body = `${SPOILER_FILE_PREFIX}secret.png`;
    const image: NotificationAlert = {
      id: 'img',
      channel: 'general',
      message: { ...message(2, body), payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [9] } } },
      mention: false,
    };
    const { container } = render(<NotificationCenter alerts={[image]} onOpen={vi.fn()} onClear={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Notifications (1)' }));
    const row = screen.getByRole('button', { name: /New message in #general/ });
    expect(row.getAttribute('aria-label')).toContain('[spoiler] secret.png');
    expect(row.textContent).toContain('[spoiler] secret.png');
    expect(container.textContent).not.toContain(SPOILER_FILE_PREFIX);
    expect(container.querySelector('img')).toBeNull();
  });

  it('hides the text spoiler of a caption that starts with the prefix, in the center and in the native notification', async () => {
    const body = `${SPOILER_FILE_PREFIX}note ||the secret, in full||`;
    const withFile = (id: number): MessageV2 => ({ ...message(id, body), payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [9] } } });
    const caption: NotificationAlert = { id: 'cap', channel: 'general', message: withFile(2), mention: false };
    render(<NotificationCenter alerts={[caption]} onOpen={vi.fn()} onClear={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Notifications (1)' }));
    const row = screen.getByRole('button', { name: /New message in #general/ });
    expect(row.textContent).toContain('[spoiler]');
    expect(document.body.textContent).not.toContain('the secret');
    expect(row.getAttribute('aria-label')).not.toContain('the secret');
    cleanup();

    visibility('hidden');
    vi.spyOn(api, 'loadMessagesAscending').mockResolvedValue({ ok: true, complete: true, value: [] });
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    await act(async () => { await Promise.resolve(); });
    act(() => { result.current.setPolicy('all'); });
    await act(async () => { await result.current.enableNative(); });
    rerender({ ...props(), event: withFile(3) });
    expect(FakeNotification.all).toHaveLength(1);
    expect(FakeNotification.all[0]?.options.body ?? '').not.toContain('the secret');
    expect(FakeNotification.all[0]?.options.body).toContain('[spoiler]');
  });

  it('previews an image-only spoiler natively as [spoiler] and the shown name', async () => {
    visibility('hidden');
    vi.spyOn(api, 'loadMessagesAscending').mockResolvedValue({ ok: true, complete: true, value: [] });
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    await act(async () => { await Promise.resolve(); });
    act(() => { result.current.setPolicy('all'); });
    await act(async () => { await result.current.enableNative(); });
    rerender({ ...props(), event: { ...message(4, `${SPOILER_FILE_PREFIX}pic.png`), payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [9] } } } });
    expect(FakeNotification.all[0]?.options.body).toBe('[spoiler] pic.png');
  });

  it('reads image-only spoilers whose names hold ", " in the center and natively, once the names are known', async () => {
    const comma = `${SPOILER_FILE_PREFIX}trip, day 1.png`;
    const body = `${comma}, ${SPOILER_FILE_PREFIX}other.png`;
    const payload = { schema: 'conch.file_attachment.v1', data: { file_ids: [11, 12] } };
    const want = '[spoiler] trip, day 1.png, [spoiler] other.png';
    forgetFileNames();
    rememberFileName(11, comma);
    rememberFileName(12, `${SPOILER_FILE_PREFIX}other.png`);
    const image: NotificationAlert = { id: 'comma', channel: 'general', message: { ...message(2, body), payload }, mention: false };
    const { container } = render(<NotificationCenter alerts={[image]} onOpen={vi.fn()} onClear={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Notifications (1)' }));
    const row = screen.getByRole('button', { name: /New message in #general/ });
    expect(row.getAttribute('aria-label')).toContain(want);
    expect(row.textContent).toContain(want);
    expect(container.textContent).not.toContain(SPOILER_FILE_PREFIX);
    cleanup();

    visibility('hidden');
    vi.spyOn(api, 'loadMessagesAscending').mockResolvedValue({ ok: true, complete: true, value: [] });
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    await act(async () => { await Promise.resolve(); });
    act(() => { result.current.setPolicy('all'); });
    await act(async () => { await result.current.enableNative(); });
    rerender({ ...props(), event: { ...message(5, body), payload } });
    expect(FakeNotification.all[0]?.options.body).toBe(want);
    forgetFileNames();
  });

  it('hides a spoiler in the center, its accessible name included', () => {
    const hidden: NotificationAlert = { id: 'two', channel: 'general', message: message(2, '@ada ||the answer is X||'), mention: true };
    render(<NotificationCenter alerts={[hidden]} onOpen={vi.fn()} onClear={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Notifications (1)' }));
    const row = screen.getByRole('button', { name: /Mention in #general/ });
    expect(row.getAttribute('aria-label')).toContain('[spoiler]');
    expect(row.textContent).toContain('[spoiler]');
    expect(document.body.textContent).not.toContain('the answer is X');
    expect(row.getAttribute('aria-label')).not.toContain('the answer is X');
  });

  it('shows denied and unsupported fallbacks in settings', () => {
    const view = render(<NotificationSettings policy="mentions" native={false} permission="denied" supported={true} onPolicy={() => undefined} onEnable={() => undefined} />);
    expect(screen.getByLabelText('Message notifications')).toHaveProperty('value', 'mentions');
    expect(screen.getByText(/Browser permission is denied/).textContent).toContain('Alerts stay here');
    view.unmount();
    render(<NotificationSettings policy="off" native={false} permission="unsupported" supported={false} onPolicy={() => undefined} onEnable={() => undefined} />);
    expect(screen.getByText(/Browser notifications are not supported/).textContent).toContain('Alerts stay here');
  });

  it('calls them desktop notifications in the desktop build', () => {
    const original = platform.name;
    Object.defineProperty(platform, 'name', { value: 'desktop', configurable: true });
    try {
      render(<NotificationSettings policy="all" native={true} permission="granted" supported={true} onPolicy={() => undefined} onEnable={() => undefined} />);
      expect(screen.getByText(/Desktop notifications are on/)).toBeDefined();
      expect(document.body.textContent).not.toContain('Browser notifications');
    } finally {
      Object.defineProperty(platform, 'name', { value: original, configurable: true });
    }
  });

  // F20: the hint follows the choice, and keeps the channel-mute sentence.
  it.each([
    ['off', 'Notifications are off.'],
    ['mentions', 'You’re alerted when someone writes your name.'],
    ['all', 'You’re alerted for every new message.'],
  ] as const)('says what the %s policy does', (policy, line) => {
    render(<NotificationSettings policy={policy} native={false} permission="default" supported={true} onPolicy={() => undefined} onEnable={() => undefined} />);
    const hint = screen.getByText(/Channel mutes hide alerts/).textContent;
    expect(hint).toBe(`${line} Channel mutes hide alerts, not unread badges.`);
    expect(document.body.textContent).not.toContain('until you choose');
  });

  it('toggles channel mutes in settings', () => {
    const onToggleMute = vi.fn();
    render(<NotificationSettings policy="all" native={false} permission="default" supported={true} channels={channels} muted={new Set()} onPolicy={() => undefined} onEnable={() => undefined} onToggleMute={onToggleMute} />);
    fireEvent.click(screen.getByText('Channel mutes', { exact: true }));
    fireEvent.click(screen.getByLabelText('Mute notifications for #general'));
    expect(onToggleMute).toHaveBeenCalledWith(7);
  });
});

// Issue #845 (docs/design/message-editing.md §7.2): the events socket's
// conch.message_edited.v1 updates an alert's text in place. It never notifies,
// never matches a mention and never adds an alert: an edit is not a new message.
describe('an edited message', () => {
  const edited = (id: number, body: string, at: string, author = 3): MessageV2 => ({ ...message(id, body, author), edited_at: at });

  it('updates a held alert in place, and never notifies or counts', async () => {
    visibility('hidden');
    vi.spyOn(api, 'loadMessagesAscending').mockResolvedValue({ ok: true, complete: true, value: [] });
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('all');
    });
    await act(async () => {
      await result.current.enableNative();
    });
    rerender({ ...props(), event: message(1, 'helo') });
    expect(result.current.alerts).toHaveLength(1);
    expect(FakeNotification.all).toHaveLength(1);
    rerender({ ...props(), event: message(1, 'helo'), editEvent: edited(1, 'hello', '2026-10-26T00:01:00Z') });
    expect(result.current.alerts).toHaveLength(1);
    expect(result.current.alerts[0]?.message.body).toBe('hello');
    expect(FakeNotification.all).toHaveLength(1);
    // A stale copy (an earlier edit, late) never replaces a later one.
    rerender({ ...props(), event: message(1, 'helo'), editEvent: edited(1, 'hel', '2026-10-26T00:00:30Z') });
    expect(result.current.alerts[0]?.message.body).toBe('hello');
  });

  it('adds nothing for a message it holds no alert of, even one that now names the viewer', () => {
    const { result, rerender } = renderHook((p: Props) => useNotifications(p), { initialProps: props() });
    act(() => {
      result.current.setPolicy('mentions');
    });
    rerender({ ...props(), editEvent: edited(5, 'now with @ada', '2026-10-26T00:01:00Z') });
    expect(result.current.alerts).toHaveLength(0);
    expect(FakeNotification.all).toHaveLength(0);
  });
});
