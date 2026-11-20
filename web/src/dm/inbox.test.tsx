import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useEffect } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { windowOf } from '../fakewindow';
import { Lobby } from '../Lobby';
import { ProfileCard } from '../ProfileCard';
import { initialChannels } from '../state/channels';
import type { DmApi } from './api';
import { DmContextProbe } from './probe';
import { DmInbox } from './DmInbox';
import { DmProvider } from './DmContext';
import { DmRoute, type DmConversationView } from './DmRoute';
import { canMessage } from './MessageAction';
import { conversation, fake, fixture, later, me, snapshot } from './testkit';

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
    const fourth = render(provide(fake({ snapshot: snap as unknown as DmApi['snapshot'] }).api, <DmInbox />));
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

  it('hides a conversation for the viewer only, and shows hidden ones again on request', async () => {
    const f = fake({ list: vi.fn(() => Promise.resolve({ ok: true as const, value: { conversations: [{ ...conversation(), id: 20, peer_name: 'Ken', state: 'hidden' as const }] } })) });
    render(provide(f.api, <DmInbox />));
    fireEvent.click(await screen.findByRole('button', { name: 'Hide the conversation with Grace' }));
    await waitFor(() => { expect(screen.queryByRole('button', { name: 'Hide the conversation with Grace' })).toBeNull(); });
    expect(f.api.setState).toHaveBeenCalledWith(12, 'hidden');
    fireEvent.click(screen.getByRole('button', { name: 'Show hidden conversations' }));
    const hidden = await screen.findByRole('list', { name: 'Hidden conversations' });
    expect(within(hidden).getByText('Ken')).toBeDefined();
    expect(within(hidden).getByRole('button', { name: 'Show the conversation with Ken' })).toBeDefined();
  });

  it('shows and changes the contact preference', async () => {
    const f = fake();
    render(provide(f.api, <DmInbox />));
    const select = await screen.findByRole<HTMLSelectElement>('combobox', { name: 'Who can message me' });
    await waitFor(() => { expect(select.value).toBe(fixture<{ accept: string }>('dm-preferences-v1').accept); });
    fireEvent.change(select, { target: { value: 'nobody' } });
    await waitFor(() => { expect(f.api.setPreferences).toHaveBeenCalledWith('nobody'); });
    expect(select.value).toBe('nobody');
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
  function lobby(api: DmApi) {
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
          panel={<p>Nobody</p>}
          voice={<Probe name="voice" />}
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
  beforeEach(() => { events.length = 0; });

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
});
