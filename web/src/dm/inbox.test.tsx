import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useContext, useEffect } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { listChannels, restore } from '../api';
import * as api from '../api';
import { BlocksProvider } from '../blocks';
import { windowOf } from '../fakewindow';
import { Lobby } from '../Lobby';
import { VoiceLive, VoiceSheet } from '../phone';
import type { DMPreferencesV1 } from '../schema/generated';
import { ProfileCard } from '../ProfileCard';
import { initialChannels } from '../state/channels';
import { useVoice } from '../voice/useVoice';
import { dmApi, type DmApi } from './api';
import { DmContextProbe } from './probe';
import { DmInbox } from './DmInbox';
import { DmProvider } from './DmContext';
import { DmRoute, type DmConversationView } from './DmRoute';
import { canMessage } from './MessageAction';
import { conversation, fake, fixture, later, me, peerMessage, snapshot } from './testkit';

beforeEach(() => {
  Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
  vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))));
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const provide = (api: DmApi, ui: React.ReactNode, scope = 'https://a.example|3') => <DmProvider api={api} scope={scope} me={me}>{ui}</DmProvider>;

describe('the inbox', () => {
  it('hides Message for a locally blocked person even when there is an existing conversation', async () => {
    const f = fake();
    const card = <ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} />;
    const view = (blocked: ReadonlySet<number>) => provide(f.api, (
      <BlocksProvider value={{
        blocked, loaded: true, list: [],
        block: () => Promise.resolve(false), unblock: () => Promise.resolve(false), signal: () => undefined,
      }}>
        {card}
      </BlocksProvider>
    ));
    const { rerender } = render(view(new Set()));
    fireEvent.click(screen.getByRole('button', { name: 'Grace' }));
    expect(await screen.findByRole('button', { name: 'Message Grace' })).toBeDefined();
    rerender(view(new Set([5])));
    expect(screen.queryByRole('button', { name: 'Message Grace' })).toBeNull();
    expect(f.api.open).not.toHaveBeenCalled();
  });

  it('lists conversations with the recipient, unread counts and a per-viewer hide, apart from any member directory', async () => {
    const f = fake();
    render(provide(f.api, <DmInbox />));
    const nav = await screen.findByRole('navigation', { name: 'Direct messages' });
    const list = within(nav).getByRole('list', { name: 'Conversations' });
    const rows = within(list).getAllByRole('listitem');
    expect(rows).toHaveLength(3);
    expect(rows[0]?.textContent).toContain('Grace');
    expect(rows[0]?.textContent).toContain('1 unread');
    expect(rows[1]?.textContent).toContain('Former member');
    expect(rows[2]?.textContent).toContain('Linus');
    expect(within(nav).getByRole('heading', { name: /Direct messages/ }).textContent).toContain('1 unread');
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
  });

  it('states loading, empty, failed, off and own-sign-in-required, and shows nothing for a server with no DMs', async () => {
    const wait = later<never>();
    const { unmount } = render(provide(fake({ snapshot: vi.fn(() => wait.promise) }).api, <DmInbox />));
    expect(screen.getByText('Loading messages…')).toBeDefined();
    unmount();

    const empty = { ...snapshot(), conversations: [] };
    const second = render(provide(fake({ snapshot: vi.fn(() => Promise.resolve({ ok: true as const, value: empty })) }).api, <DmInbox />));
    expect(await screen.findByText(/No direct messages yet/)).toBeDefined();
    second.unmount();

    const off = { ...snapshot(), dms_enabled: false };
    const third = render(provide(fake({ snapshot: vi.fn(() => Promise.resolve({ ok: true as const, value: off })) }).api, <DmInbox />));
    expect(await screen.findByText('Direct messages are off.')).toBeDefined();
    third.unmount();

    const snap = vi.fn()
      .mockResolvedValueOnce({ ok: false, failure: 'network' })
      .mockResolvedValue({ ok: true, value: snapshot() });
    const fourth = render(provide(fake({ snapshot: snap as NonNullable<DmApi['snapshot']> }).api, <DmInbox />));
    fireEvent.click(await screen.findByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('list', { name: 'Conversations' })).toBeDefined();
    fourth.unmount();

    const own = render(provide(fake({ snapshot: vi.fn(() => Promise.resolve({ ok: false as const, failure: 'own_sign_in_required' as const })) }).api, <DmInbox />));
    expect(await screen.findByText(/need your own sign-in/)).toBeDefined();
    own.unmount();

    const none = render(provide(fake({ snapshot: vi.fn(() => Promise.resolve({ ok: false as const, failure: 'not_found' as const })) }).api, <DmInbox />));
    await act(async () => {});
    expect(none.container.textContent).toBe('');
  });

  it('moves between rows with the arrow keys and opens one with Enter’s click', async () => {
    const f = fake();
    render(provide(f.api, <><DmInbox /><DmContextProbe /></>));
    const rows = await screen.findAllByRole('button', { name: /^(Grace|Former member)/ });
    rows[0]?.focus();
    fireEvent.keyDown(rows[0] as HTMLElement, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(rows[1]);
    fireEvent.keyDown(rows[1] as HTMLElement, { key: 'ArrowUp' });
    expect(document.activeElement).toBe(rows[0]);
    fireEvent.click(rows[1] as HTMLElement);
    expect(screen.getByTestId('selected').textContent).toBe('13');
  });

  it('hides a conversation and can reopen it with Message', async () => {
    const f = fake();
    render(provide(f.api, <><DmInbox /><ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} /></>));
    fireEvent.click(await screen.findByRole('button', { name: 'Hide the conversation with Grace' }));
    await waitFor(() => { expect(screen.queryByRole('button', { name: 'Hide the conversation with Grace' })).toBeNull(); });
    expect(f.api.setState).toHaveBeenCalledWith(12, 'hidden');
    fireEvent.click(screen.getByRole('button', { name: 'Grace' }));
    fireEvent.click(within(screen.getByRole('dialog', { name: 'Grace' })).getByRole('button', { name: 'Message Grace' }));
    await screen.findByRole('button', { name: 'Hide the conversation with Grace' });
    expect(f.api.list).not.toHaveBeenCalled();
  });

  it('restores focus to the next inbox row after hiding the focused row', async () => {
    render(provide(fake().api, <DmInbox />));
    const hide = await screen.findByRole('button', { name: 'Hide the conversation with Grace' });
    const next = screen.getByRole('button', { name: /^Former member/ });
    hide.focus();
    fireEvent.click(hide);
    await waitFor(() => { expect(document.activeElement).toBe(next); });
  });

  it('pages open and archived rows beyond the snapshot cap without double-counting overlap', async () => {
    const top = conversation();
    const older = { ...top, id: 21, peer_name: 'Ken', unread: 3 };
    const archived = { ...top, id: 22, peer_name: 'Margaret', unread: 5, state: 'archived' as const };
    const list = vi.fn<DmApi['list']>()
      .mockResolvedValueOnce({ ok: true, value: { conversations: [top], next_cursor: 'older' } })
      .mockResolvedValueOnce({ ok: true, value: { conversations: [older] } })
      .mockResolvedValueOnce({ ok: true, value: { conversations: [archived] } });
    const f = fake({ snapshot: vi.fn(() => Promise.resolve({ ok: true as const, value: { ...snapshot(), more: true } })), list });
    render(provide(f.api, <DmInbox />));
    expect(await screen.findByRole('button', { name: /^Margaret/ })).toBeDefined();
    expect(screen.getByRole('heading', { name: /Direct messages/ }).textContent).toContain('9 unread');
    expect(list.mock.calls).toEqual([['open', undefined], ['open', 'older'], ['archived', undefined]]);
  });

  it('refreshes for incoming changes while the page stays visible', async () => {
    const snapshotCall = vi.fn<NonNullable<DmApi['snapshot']>>()
      .mockResolvedValueOnce({ ok: true, value: { ...snapshot(), conversations: [] } })
      .mockResolvedValueOnce({ ok: true, value: snapshot() });
    const f = fake({ snapshot: snapshotCall });
    render(provide(f.api, <DmInbox />));
    await screen.findByText(/No direct messages yet/);
    act(() => { f.frames({ type: 'poll' }); });
    expect(await screen.findByRole('list', { name: 'Conversations' })).toBeDefined();
  });

  it('polls with the production adapter only while visible and subscribed', () => {
    vi.useFakeTimers();
    const listener = vi.fn();
    const stop = dmApi.live(listener);
    try {
      vi.advanceTimersByTime(15_000);
      expect(listener).toHaveBeenCalledWith({ type: 'poll' });
      Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true });
      vi.advanceTimersByTime(15_000);
      expect(listener).toHaveBeenCalledTimes(1);
      stop();
      Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
      vi.advanceTimersByTime(15_000);
      expect(listener).toHaveBeenCalledTimes(1);
    } finally {
      stop();
      vi.useRealTimers();
    }
  });

  it('shows and changes the contact preference', async () => {
    const f = fake();
    render(provide(f.api, <DmInbox />));
    const select = await screen.findByRole<HTMLSelectElement>('combobox', { name: 'Who can message me' });
    await waitFor(() => { expect(select.value).toBe(fixture<DMPreferencesV1>('dm-preferences-v1').accept); });
    fireEvent.change(select, { target: { value: 'nobody' } });
    await waitFor(() => { expect(f.api.setPreferences).toHaveBeenCalledWith('nobody'); });
    expect(select.value).toBe('nobody');
  });

  it('opens, hides and resumes through the routes main serves when history and settings are absent', async () => {
    const row = conversation();
    let hidden = false;
    const json = (body: unknown) => new Response(JSON.stringify(body));
    const calls: string[] = [];
    window.sessionStorage.setItem('conch.web.token', 'conch_member_secret');
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(input instanceof Request ? input.url : input, window.location.href);
      const method = init?.method ?? 'GET';
      calls.push(`${method} ${path.pathname}`);
      if (path.pathname === '/v1/whoami') return Promise.resolve(json({ id: me, kind: 'human', name: 'Ada', role: 'member' }));
      if (path.pathname === '/v1/dms' && method === 'GET') {
        return Promise.resolve(json({ conversations: hidden || path.searchParams.get('state') === 'archived' ? [] : [row] }));
      }
      if (path.pathname === '/v1/dms' && method === 'POST') {
        hidden = false;
        expect(init?.body).toBe(JSON.stringify({ peer_id: row.peer_id }));
        return Promise.resolve(json({ conversation: row }));
      }
      if (path.pathname === '/v1/dms/12') return Promise.resolve(json(row));
      if (path.pathname === '/v1/dms/12/state' && method === 'PUT') {
        hidden = true;
        expect(init?.body).toBe(JSON.stringify({ state: 'hidden' }));
        return Promise.resolve(json({ ...row, state: 'hidden' }));
      }
      return Promise.resolve(new Response('404 page not found', { status: 404 }));
    }));
    try {
      expect((await restore()).ok).toBe(true);
      render(provide(dmApi, <><DmInbox /><ProfileCard id={row.peer_id} person={{ name: 'Grace', kind: 'human' }} viewer={me} /><DmContextProbe /></>));
      const nav = screen.getByRole('navigation', { name: 'Direct messages' });
      fireEvent.click(await within(nav).findByRole('button', { name: /^Grace/ }));
      expect(screen.getByTestId('selected').textContent).toBe('12');
      const view = render(provide(dmApi, <DmRoute conversationId={12} />));
      expect(await within(view.container).findByRole('heading', { name: 'Grace' })).toBeDefined();
      expect(within(view.container).getByText('Message history is unavailable on this server.')).toBeDefined();
      expect(within(view.container).queryByRole('textbox')).toBeNull();
      view.unmount();
      expect(within(nav).queryByRole('combobox', { name: 'Who can message me' })).toBeNull();
      fireEvent.click(within(nav).getByRole('button', { name: /Hide.*Grace/ }));
      await within(nav).findByText(/No direct messages yet/);
      fireEvent.click(screen.getByRole('button', { name: 'Grace' }));
      fireEvent.click(screen.getByRole('button', { name: 'Message Grace' }));
      await within(nav).findByRole('button', { name: /^Grace/ });
      expect(calls).not.toContain('GET /v1/dms/snapshot');
    } finally {
      window.sessionStorage.clear();
      await listChannels();
    }
  });
});

describe('the Message action', () => {
  const human = { kind: 'human' as const };

  it('is offered for a person and hidden for an agent, a guest and yourself', () => {
    expect(canMessage(human, 5, me)).toBe(true);
    expect(canMessage({ kind: 'agent' }, 5, me)).toBe(false);
    expect(canMessage({ kind: 'human', guest: { expires_at: '2026-11-20T00:00:00Z' } as never }, 5, me)).toBe(false);
    expect(canMessage(human, me, me)).toBe(false);
    expect(canMessage({ kind: null }, 5, me)).toBe(false);
  });

  it('opens or resumes the pair from a profile card and selects the conversation', async () => {
    const f = fake();
    render(provide(f.api, <><ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} /><DmContextProbe /></>));
    await screen.findByText('Grace', { selector: 'strong, .profile-card-name' }).catch(() => undefined);
    await waitFor(() => { expect(vi.mocked(f.api.snapshot)).toHaveBeenCalled(); });
    await act(async () => {});
    fireEvent.click(screen.getByRole('button', { name: 'Grace' }));
    const card = screen.getByRole('dialog', { name: 'Grace' });
    fireEvent.click(within(card).getByRole('button', { name: 'Message Grace' }));
    await waitFor(() => { expect(f.api.open).toHaveBeenCalledWith(5); });
    await waitFor(() => { expect(screen.getByTestId('selected').textContent).toBe('12'); });
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('reaches Message from the card by keyboard, in order before Close', async () => {
    render(provide(fake().api, <ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} />));
    await act(async () => {});
    fireEvent.click(screen.getByRole('button', { name: 'Grace' }));
    const card = screen.getByRole('dialog', { name: 'Grace' });
    const labels = within(card).getAllByRole('button').map((b) => b.textContent);
    expect(labels).toEqual(['Message', 'Close']);
    // Tab from Message stays in the card; Tab past Close leaves it.
    const message = within(card).getByRole('button', { name: 'Message Grace' });
    message.focus();
    fireEvent.keyDown(message, { key: 'Tab' });
    expect(screen.queryByRole('dialog')).not.toBeNull();
    const close = within(card).getByRole('button', { name: 'Close' });
    close.focus();
    fireEvent.keyDown(close, { key: 'Tab' });
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('says one generic line when the person cannot be messaged', async () => {
    const f = fake({ open: vi.fn(() => Promise.resolve({ ok: false as const, failure: 'unavailable' as const })) });
    render(provide(f.api, <ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} />));
    await act(async () => {});
    fireEvent.click(screen.getByRole('button', { name: 'Grace' }));
    fireEvent.click(screen.getByRole('button', { name: 'Message Grace' }));
    expect((await screen.findByRole('alert')).textContent).toBe("Can't send a message to this person");
  });

  it('is not there without a provider, for an agent, or while DMs are off', async () => {
    render(<ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} />);
    fireEvent.click(screen.getByRole('button', { name: 'Grace' }));
    expect(screen.queryByRole('button', { name: 'Message Grace' })).toBeNull();
    cleanup();
    const off = { ...snapshot(), dms_enabled: false };
    render(provide(fake({ snapshot: vi.fn(() => Promise.resolve({ ok: true as const, value: off })) }).api, <ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} />));
    await act(async () => {});
    fireEvent.click(screen.getByRole('button', { name: 'Grace' }));
    expect(screen.queryByRole('button', { name: 'Message Grace' })).toBeNull();
  });
});

describe('the route shell', () => {
  it('mounts the exported transcript by default', async () => {
    const f = fake();
    render(provide(f.api, <DmRoute conversationId={12} />));
    expect(await screen.findByRole('log', { name: 'Messages' })).toBeDefined();
    expect(f.api.history).toHaveBeenCalledWith(12, { order: 'desc' });
  });

  it('gives any view of the slot’s type the conversation, the scope and the viewer, keyed so another conversation is a new view', () => {
    const seen: string[] = [];
    const view: DmConversationView = (props) => {
      return <Mounted label={`${props.scope}#${String(props.conversationId)}:${String(props.viewer)}`} seen={seen} />;
    };
    const f = fake();
    const { rerender } = render(provide(f.api, <DmRoute conversationId={12} view={view} />));
    rerender(provide(f.api, <DmRoute conversationId={14} view={view} />));
    expect(seen).toEqual(['mount https://a.example|3#12:3', 'unmount https://a.example|3#12:3', 'mount https://a.example|3#14:3']);
  });

  it('does not carry a conversation across two origins that use the same ids', async () => {
    const a = fake();
    const b = fake({ get: vi.fn(() => Promise.resolve({ ok: true as const, value: { ...conversation(), peer_name: 'Hopper' } })), history: vi.fn(() => Promise.resolve({ ok: true as const, value: { order: 'desc' as const, messages: [] } })) });
    const { rerender } = render(provide(a.api, <DmRoute conversationId={12} />, 'https://a.example|3'));
    expect(await screen.findByRole('heading', { name: 'Grace' })).toBeDefined();
    fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: 'private to a' } });
    rerender(provide(b.api, <DmRoute conversationId={12} />, 'https://b.example|3'));
    expect(await screen.findByRole('heading', { name: 'Hopper' })).toBeDefined();
    expect(screen.queryByText('Thanks, reading it now.')).toBeNull();
    expect(screen.getByRole<HTMLTextAreaElement>('textbox', { name: 'Message to Hopper' }).value).toBe('');
  });
});

function Mounted({ label, seen }: { readonly label: string; readonly seen: string[] }) {
  useEffect(() => {
    seen.push(`mount ${label}`);
    return () => { seen.push(`unmount ${label}`); };
  }, [label, seen]);
  return <p>{label}</p>;
}

describe('in the lobby', () => {
  const events: string[] = [];
  function Probe({ name }: { readonly name: string }) {
    useEffect(() => {
      events.push(`mount ${name}`);
      return () => { events.push(`unmount ${name}`); };
    }, [name]);
    return <p>{name}</p>;
  }
  function ActiveVoice() {
    const report = useContext(VoiceLive);
    useEffect(() => { report(true); }, [report]);
    return <Probe name="voice" />;
  }
  function lobby(api: DmApi, panel: React.ReactNode = <p>Nobody</p>, voice: React.ReactNode = <ActiveVoice />) {
    return render(
      <DmProvider api={api} scope="https://a.example|3" me={me}>
        <Lobby
          origin="http://127.0.0.1:8080"
          name="ada"
          me={me}
          channels={initialChannels}
          unread={new Map()}
          connection="connected"
          conversation={<Probe name="conversation" />}
          panel={panel}
          voice={voice}
          invites={null}
          onOpen={() => undefined}
          onRetry={() => undefined}
          onSignOut={() => undefined}
          onSignOutEverywhere={() => undefined}
          signOutProblem={null}
        />
      </DmProvider>,
    );
  }
  function phoneWindow() {
    vi.stubGlobal('matchMedia', vi.fn((query: string) => ({
      matches: query.includes('max-width'),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })));
  }
  beforeEach(() => { events.length = 0; });
  afterEach(() => { vi.restoreAllMocks(); });

  it('opens a conversation in the middle without touching the voice section, and keeps the channel mounted', async () => {
    windowOf(true);
    const f = fake();
    lobby(f.api);
    fireEvent.click(await screen.findByRole('button', { name: /^Grace/ }));
    expect(await screen.findByRole('log', { name: 'Messages' })).toBeDefined();
    expect(events).toEqual(['mount conversation', 'mount voice']);
    fireEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect(screen.queryByRole('log', { name: 'Messages' })).toBeNull();
    expect(events).toEqual(['mount conversation', 'mount voice']);
  });

  it('works on a narrow window too, with the inbox and its rows reachable', async () => {
    windowOf(false);
    const f = fake();
    lobby(f.api);
    const rows = await screen.findAllByRole('button', { name: /^(Grace|Former member)/ });
    rows[0]?.focus();
    fireEvent.keyDown(rows[0] as HTMLElement, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(rows[1]);
    fireEvent.click(rows[0] as HTMLElement);
    expect(await screen.findByRole('log', { name: 'Messages' })).toBeDefined();
    expect(events).not.toContain('unmount voice');
  });

  it('clears all modal inertness when Voice options closes with no DM selected', async () => {
    function Voice() {
      const { report } = useContext(VoiceSheet);
      return (
        <>
          <button type="button" onClick={() => { report(true); }}>Voice options</button>
          <button type="button" onClick={() => { report(false); }}>Close options</button>
        </>
      );
    }
    phoneWindow();
    const { container } = lobby(fake().api, undefined, <Voice />);
    await screen.findByRole('button', { name: /^Grace/ });
    fireEvent.click(screen.getByRole('button', { name: 'Voice' }));
    fireEvent.click(screen.getByRole('button', { name: 'Voice options' }));
    expect(container.querySelector('.areas[inert]')).not.toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Close options' }));
    expect(container.querySelectorAll('[inert]:not(.reserved)')).toHaveLength(0);
  });

  it('keeps the same Voice view and a single join request through a DM round trip', async () => {
    const session = later<api.VoiceSessionResult>();
    const createSession = vi.spyOn(api, 'createVoiceSession').mockReturnValue(session.promise);
    const onUnauthorized = vi.fn();
    function Voice() {
      const voice = useVoice({ channel: 'general', signedIn: true, voiceAllowed: true, reachable: true, onUnauthorized });
      const report = useContext(VoiceLive);
      const joining = voice.state.phase === 'connecting';
      useEffect(() => { report(joining); }, [joining, report]);
      return (
        <section aria-label="Voice call">
          <Probe name="voice" />
          {joining ? <p>Connecting</p> : <button type="button" onClick={() => { voice.join(); }}>Join voice</button>}
        </section>
      );
    }
    phoneWindow();
    const { container } = lobby(fake().api, <ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} />, <Voice />);
    const voiceTab = screen.getByRole('button', { name: 'Voice' });
    fireEvent.click(voiceTab);
    const voice = screen.getByRole('region', { name: 'Voice call' });
    fireEvent.click(within(voice).getByRole('button', { name: 'Join voice' }));
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    const drawer = within(screen.getByRole('region', { name: 'Members list' }));
    fireEvent.click(drawer.getByRole('button', { name: 'Grace' }));
    const message = await drawer.findByRole('button', { name: 'Message Grace' });
    message.focus();
    fireEvent.click(message);
    const back = await screen.findByRole('button', { name: 'Back' });
    expect(screen.getByRole('region', { name: 'Voice call' })).toBe(voice);
    fireEvent.click(back);
    expect(voiceTab.getAttribute('aria-current')).toBe('page');
    expect(screen.getByRole('region', { name: 'Voice call' })).toBe(voice);
    expect(within(voice).getByText('Connecting')).toBeDefined();
    expect(events).toEqual(['mount conversation', 'mount voice']);
    expect(createSession.mock.calls).toEqual([['general']]);
    expect(container.querySelectorAll('[inert]:not(.reserved)')).toHaveLength(0);
  });

  it('moves focus from the phone inbox into Chat and back while voice stays active', async () => {
    phoneWindow();
    lobby(fake().api);
    const opener = await screen.findByRole('button', { name: /^Grace/ });
    opener.focus();
    fireEvent.click(opener);
    const back = await screen.findByRole('button', { name: 'Back' });
    expect(document.activeElement).toBe(back);
    expect(opener.closest('[inert]')).not.toBeNull();
    expect(screen.getByRole('button', { name: 'Voice' }).getAttribute('aria-describedby')).toBe('nav-voice-live');
    fireEvent.keyDown(back, { key: 'Escape' });
    expect(screen.queryByRole('button', { name: 'Back' })).toBeNull();
    expect(document.activeElement).toBe(opener);
    expect(events).toEqual(['mount conversation', 'mount voice']);
  });

  it('marks a new DM read only when its Chat view is visible', async () => {
    phoneWindow();
    const f = fake();
    lobby(f.api);
    fireEvent.click(await screen.findByRole('button', { name: /^Grace/ }));
    await waitFor(() => { expect(f.api.markRead).toHaveBeenCalledWith(12, 4310); });
    fireEvent.click(screen.getByRole('button', { name: 'Voice' }));
    act(() => { f.frames({ type: 'message', message: { ...peerMessage(), id: 4311, revision: 7 } }); });
    expect(f.api.markRead).not.toHaveBeenCalledWith(12, 4311);
    fireEvent.click(screen.getByRole('button', { name: 'Chat' }));
    await waitFor(() => { expect(f.api.markRead).toHaveBeenCalledWith(12, 4311); });
  });

  it('closes the modal Members drawer on Message and restores focus to Members from Back', async () => {
    windowOf(false);
    lobby(fake().api, <ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} />);
    const members = screen.getByRole('button', { name: 'Members' });
    fireEvent.click(members);
    const drawer = screen.getByRole('region', { name: 'Members list' });
    expect(screen.getByRole('main').hasAttribute('inert')).toBe(true);
    fireEvent.click(within(drawer).getByRole('button', { name: 'Grace' }));
    const message = await within(drawer).findByRole('button', { name: 'Message Grace' });
    message.focus();
    fireEvent.click(message);
    const back = await screen.findByRole('button', { name: 'Back' });
    expect(drawer.hasAttribute('hidden')).toBe(true);
    expect(screen.getByRole('main').hasAttribute('inert')).toBe(false);
    expect(document.activeElement).toBe(back);
    fireEvent.click(back);
    expect(document.activeElement).toBe(members);
  });

  it('keeps focus in Settings if it opens while Message is still pending', async () => {
    const answer = later<Awaited<ReturnType<DmApi['open']>>>();
    lobby(fake({ open: vi.fn(() => answer.promise) }).api, <ProfileCard id={5} person={{ name: 'Grace', kind: 'human' }} viewer={me} />);
    const drawer = screen.getByRole('region', { name: 'Members list' });
    fireEvent.click(within(drawer).getByRole('button', { name: 'Grace' }));
    const message = await within(drawer).findByRole('button', { name: 'Message Grace' });
    message.focus();
    fireEvent.click(message);
    fireEvent.click(screen.getByRole('button', { name: /^Settings/ }));
    const settings = screen.getByRole('dialog', { name: 'Settings' });
    await act(async () => {
      answer.resolve({ ok: true, value: conversation() });
      await answer.promise;
    });
    expect(settings.contains(document.activeElement)).toBe(true);
    expect(screen.queryByRole('button', { name: 'Back' })).toBeNull();
  });
});
