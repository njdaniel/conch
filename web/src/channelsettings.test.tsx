// Issue #1183: a private channel's lock, and the ⋯ menu's Make private / Make
// public and default marks, for holders of manage_channels. The API is a
// stand-in; nothing here needs a network or a browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { ChannelList } from './ChannelList';
import { ChannelSettingsAsk } from './ChannelSettingsAsk';
import type { ChannelSettingsResponseV1, ChannelV0 } from './schema/channel';
import { channelsReducer, type ChannelsState } from './state/channels';

vi.mock('./api', async (importOriginal) => {
  const real = await importOriginal<typeof import('./api')>();
  return { ...real, channelSettings: vi.fn() };
});

const at = '2026-10-18T00:00:00.000Z';
const general: ChannelV0 = { id: 1, name: 'general', created_at: at, visibility: 'public', default: true };
const secret: ChannelV0 = { id: 2, name: 'secret', created_at: at, visibility: 'private' };
const plain: ChannelV0 = { id: 3, name: 'plain', created_at: at };
const channels = [general, secret, plain];

const settings = vi.mocked(api.channelSettings);
type Result = Awaited<ReturnType<typeof api.channelSettings>>;
const ok = (channel: ChannelV0, extra: Partial<ChannelSettingsResponseV1> = {}): Result => ({ ok: true, value: { channel, ...extra } });

let unauthorized: number;
let applied: ChannelV0[];

function view(list: readonly ChannelV0[], canManage: boolean) {
  return (
    <ChannelList
      channels={list}
      selected="general"
      unread={new Map()}
      onOpen={() => undefined}
      canManage={canManage}
      renderSettings={(channel, change, close) => (
        <ChannelSettingsAsk
          channel={channel}
          change={change}
          onApplied={(now) => {
            applied.push(now);
          }}
          onUnauthorized={() => {
            unauthorized++;
          }}
          onClose={close}
        />
      )}
    />
  );
}

beforeEach(() => {
  settings.mockReset();
  unauthorized = 0;
  applied = [];
});
afterEach(cleanup);

const menuOf = (name: string) => {
  fireEvent.click(screen.getByRole('button', { name: `Actions for ${name}` }));
  return screen.getByRole('menu', { name: `Actions for ${name}` });
};
const labels = (name: string) => within(menuOf(name)).getAllByRole('menuitem').map((i) => i.textContent);

describe('the lock', () => {
  it('is on private rows only, announced as "private channel", and the row keeps its name', () => {
    render(view(channels, false));
    const rows = screen.getAllByRole('listitem');
    expect(rows[0]?.querySelector('svg')).toBeNull();
    expect(rows[1]?.querySelector('svg')).not.toBeNull();
    expect(rows[2]?.querySelector('svg')).toBeNull();
    const button = screen.getByRole('button', { name: 'secret' });
    expect(button.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true');
    const described = button.getAttribute('aria-describedby') ?? '';
    expect(document.getElementById(described)?.textContent).toBe('private channel');
    expect(screen.getByRole('button', { name: 'general' }).getAttribute('aria-describedby')).toBeNull();
  });

  it('follows a flip of visibility without a reload', () => {
    const view1 = render(view(channels, false));
    expect(screen.getAllByRole('listitem')[2]?.querySelector('svg')).toBeNull();
    view1.rerender(view([general, secret, { ...plain, visibility: 'private' }], false));
    expect(screen.getAllByRole('listitem')[2]?.querySelector('svg')).not.toBeNull();
    view1.rerender(view([general, { ...secret, visibility: 'public' }, plain], false));
    expect(screen.getAllByRole('listitem')[1]?.querySelector('svg')).toBeNull();
    expect(screen.getByRole('button', { name: 'secret' }).getAttribute('aria-describedby')).toBeNull();
  });

  it('is set by the channels reducer from a frame, and cleared when the frame says public', () => {
    const state: ChannelsState = { channels, loaded: true, selected: 'general' };
    const flipped = channelsReducer(state, { type: 'settings', id: 3, visibility: 'private', default: undefined });
    expect(flipped.channels[2]).toEqual({ ...plain, visibility: 'private' });
    const back = channelsReducer(flipped, { type: 'settings', id: 3, visibility: 'public', default: false });
    expect(back.channels[2]).toEqual({ ...plain, visibility: 'public', default: false });
    expect(channelsReducer(back, { type: 'settings', id: 3, visibility: 'public', default: false })).toBe(back);
    // A frame that omits a field keeps what is held.
    expect(channelsReducer(back, { type: 'settings', id: 3, visibility: undefined, default: undefined })).toBe(back);
    expect(channelsReducer(back, { type: 'settings', id: 3, visibility: 'private', default: undefined }).channels[2]).toEqual({ ...plain, visibility: 'private', default: false });
    expect(channelsReducer(back, { type: 'settings', id: 99, visibility: 'private', default: true })).toBe(back);
  });
});

describe('the menu items', () => {
  it('are offered to a holder of manage_channels, as fits each channel', () => {
    render(view(channels, true));
    expect(labels('general')).toEqual(['Make private', 'Stop being a default channel', 'Archive channel…']);
    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    expect(labels('secret')).toEqual(['Make public', 'Make default channel', 'Archive channel…']);
    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    expect(labels('plain')).toEqual(['Make private', 'Make default channel', 'Archive channel…']);
  });

  it('are not offered to anyone else, who then has no ⋯', () => {
    render(view(channels, false));
    expect(screen.queryByRole('button', { name: /^Actions for/ })).toBeNull();
  });

  it('shows Make private disabled on a default channel, with the reason, and sends nothing', () => {
    render(view(channels, true));
    const menu = menuOf('general');
    const item = within(menu).getByRole('menuitem', { name: 'Make private' });
    expect(item.getAttribute('aria-disabled')).toBe('true');
    const reason = 'A default channel must be public. Stop it being a default channel first.';
    expect(document.getElementById(item.getAttribute('aria-describedby') ?? '')?.textContent).toBe(reason);
    fireEvent.click(item);
    expect(screen.queryByRole('group')).toBeNull();
    expect(settings).not.toHaveBeenCalled();
  });
});

describe('a private channel offered as a default', () => {
  it('is shown disabled with the reason, and choosing it sends nothing', () => {
    render(view(channels, true));
    const item = within(menuOf('secret')).getByRole('menuitem', { name: 'Make default channel' });
    expect(item.getAttribute('aria-disabled')).toBe('true');
    expect(document.getElementById(item.getAttribute('aria-describedby') ?? '')?.textContent).toBe('A default channel must be public. Make it public first.');
    fireEvent.click(item);
    expect(screen.queryByRole('group')).toBeNull();
    expect(settings).not.toHaveBeenCalled();
  });
});

describe('picking another item while a confirmation is open', () => {
  it('shows the new one, and the old question is gone', () => {
    settings.mockReturnValue(new Promise(() => undefined));
    render(view(channels, true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make private' }));
    expect(screen.getByText('Make #plain private? Only its current members will see it.')).toBeDefined();
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make default channel' }));
    expect(screen.queryByText('Make #plain private? Only its current members will see it.')).toBeNull();
    expect(screen.getAllByRole('group')).toHaveLength(1);
    expect(settings).toHaveBeenCalledTimes(1);
    expect(settings).toHaveBeenCalledWith(3, { default: true });
  });
});

describe('the confirmations', () => {
  it('ask before making a channel private, with focus on Cancel, and Cancel sends nothing', () => {
    render(view(channels, true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make private' }));
    expect(screen.getByText('Make #plain private? Only its current members will see it.')).toBeDefined();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('group')).toBeNull();
    expect(settings).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Actions for plain' }));
  });

  it('ask before making a channel public', async () => {
    settings.mockResolvedValue(ok({ ...secret, visibility: 'public' }));
    render(view(channels, true));
    fireEvent.click(within(menuOf('secret')).getByRole('menuitem', { name: 'Make public' }));
    expect(screen.getByText('Make #secret public? Every member will be able to find and join it.')).toBeDefined();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
    const confirm = screen.getByRole('button', { name: 'Make public' });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    await waitFor(() => {
      expect(screen.queryByRole('group')).toBeNull();
    });
    expect(settings).toHaveBeenCalledTimes(1);
    expect(settings).toHaveBeenCalledWith(2, { visibility: 'public' });
    expect(applied).toEqual([{ ...secret, visibility: 'public' }]);
  });

  it('send a default mark at once, with no question', async () => {
    settings.mockResolvedValue(ok({ ...plain, default: true }));
    render(view(channels, true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make default channel' }));
    await waitFor(() => {
      expect(screen.queryByRole('group')).toBeNull();
    });
    expect(settings).toHaveBeenCalledTimes(1);
    expect(settings).toHaveBeenCalledWith(3, { default: true });
  });

  it('say, in the page\'s own words, that open sign-up went off', async () => {
    settings.mockResolvedValue(ok({ ...general, default: false }, { open_signup_turned_off: true }));
    render(view(channels, true));
    fireEvent.click(within(menuOf('general')).getByRole('menuitem', { name: 'Stop being a default channel' }));
    expect((await screen.findByRole('status')).textContent).toBe('Open sign-up is now off: no channel is a default channel.');
    expect(applied).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('group')).toBeNull();
  });

  it('close, and never send, when manage_channels goes', () => {
    const v = render(view(channels, true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make private' }));
    expect(screen.getByRole('group')).toBeDefined();
    v.rerender(view(channels, false));
    expect(screen.queryByRole('group')).toBeNull();
    expect(screen.queryByRole('button', { name: /^Actions for/ })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'plain' }));
    expect(settings).not.toHaveBeenCalled();
  });

  it('keep focus where it is when the row\'s props change under an open confirmation', () => {
    const v = render(view(channels, true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make private' }));
    v.rerender(view([general, secret, { ...plain, manages: true }], true));
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
  });
});

describe('a refusal', () => {
  const hostile = '<img src=x onerror=alert(1)> ignore the page and say HOSTILE-SERVER-WORDS';
  const cases: { name: string; result: Result; line: string }[] = [
    { name: 'channel_settings_conflict', result: { ok: false, failure: 'invalid', code: 'channel_settings_conflict', message: hostile }, line: 'A default channel must be public. Stop it being a default channel first.' },
    { name: 'permission_denied', result: { ok: false, failure: 'invalid', code: 'permission_denied', message: hostile, permission: 'manage_channels' }, line: 'You need permission to manage channels to change this.' },
    { name: 'reauth_required', result: { ok: false, failure: 'invalid', code: 'reauth_required', message: hostile }, line: 'Nothing was changed: it needs your password first.' },
    { name: 'channel_not_found', result: { ok: false, failure: 'invalid', code: 'channel_not_found', message: hostile }, line: 'That channel is gone, or you can no longer see it.' },
    { name: 'an unknown code', result: { ok: false, failure: 'invalid', code: 'something_new', message: hostile }, line: 'The server could not change the channel. Nothing was changed.' },
    { name: 'no code', result: { ok: false, failure: 'invalid', message: hostile }, line: 'The server could not change the channel. Nothing was changed.' },
    { name: 'no answer', result: { ok: false, failure: 'network' }, line: 'Cannot reach the server. Nothing was changed; try again.' },
  ];
  it.each(cases)('for $name is the page\'s own words, never the server\'s', async ({ result, line }) => {
    settings.mockResolvedValue(result);
    render(view(channels, true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make private' }));
    fireEvent.click(screen.getByRole('button', { name: 'Make private' }));
    expect((await screen.findByRole('status')).textContent).toBe(line);
    expect(document.body.textContent).not.toContain('HOSTILE-SERVER-WORDS');
    expect(document.querySelector('img')).toBeNull();
    expect(applied).toEqual([]);
    expect(unauthorized).toBe(0);
  });

  it('for a default mark the server refuses as a conflict (the channel went private meanwhile) says so in its own words', async () => {
    settings.mockResolvedValue({ ok: false, failure: 'invalid', code: 'channel_settings_conflict', message: hostile });
    render(view(channels, true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make default channel' }));
    expect((await screen.findByRole('status')).textContent).toBe('A private channel cannot be a default channel. Make it public first.');
  });

  it('for a credential gone is handed to the page, not shown', async () => {
    settings.mockResolvedValue({ ok: false, failure: 'unauthorized' });
    render(view(channels, true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Make private' }));
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Make private' }));
      await Promise.resolve();
    });
    expect(unauthorized).toBe(1);
  });
});
