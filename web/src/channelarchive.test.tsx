// Issue #844: a channel archived and restored, in the browser. The reducer
// learns archived_at; the list puts archived channels in a collapsed Archived
// group; the ⋯ menu offers Archive (asked first) and Restore to holders of
// manage_channels; an archived channel opens read-only; a refused write says
// so in the page's own line. No network, no browser.
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { archivedLine, isArchived } from './archived';
import { ChannelList } from './ChannelList';
import { ChannelSettingsAsk, refusalLine } from './ChannelSettingsAsk';
import { uploadRefusalLine } from './fileSize';
import type { ChannelSettingsResponseV1, ChannelV0 } from './schema/channel';
import { channelsReducer, type ChannelsState } from './state/channels';

vi.mock('./api', async (importOriginal) => {
  const real = await importOriginal<typeof import('./api')>();
  return { ...real, channelSettings: vi.fn() };
});

const at = '2026-11-04T00:00:00.000Z';
const stamp = '2026-11-04T12:00:00Z';
const leak = 'SERVER-WORDS-NOT-FOR-PEOPLE';
const general: ChannelV0 = { id: 1, name: 'general', created_at: at, visibility: 'public', default: true };
const plain: ChannelV0 = { id: 2, name: 'plain', created_at: at, visibility: 'public' };
const old: ChannelV0 = { id: 3, name: 'old', created_at: at, visibility: 'public', archived_at: stamp };

const settings = vi.mocked(api.channelSettings);
type Result = Awaited<ReturnType<typeof api.channelSettings>>;
const ok = (channel: ChannelV0, extra: Partial<ChannelSettingsResponseV1> = {}): Result => ({ ok: true, value: { channel, ...extra } });

let applied: ChannelV0[];

function view(list: readonly ChannelV0[], canManage: boolean, selected = 'general') {
  return (
    <ChannelList
      channels={list}
      selected={selected}
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
          onUnauthorized={() => undefined}
          onClose={close}
        />
      )}
    />
  );
}

beforeEach(() => {
  settings.mockReset();
  applied = [];
});
afterEach(cleanup);

const menuOf = (name: string) => {
  fireEvent.click(screen.getByRole('button', { name: `Actions for ${name}` }));
  return screen.getByRole('menu', { name: `Actions for ${name}` });
};
const labels = (name: string) => within(menuOf(name)).getAllByRole('menuitem').map((i) => i.textContent);

describe('the channels reducer', () => {
  const state: ChannelsState = { channels: [general, plain, old], loaded: true, selected: 'general' };

  it('keeps archived_at from the list as loaded and refreshed', () => {
    const loaded = channelsReducer({ channels: [], loaded: false, selected: null }, { type: 'loaded', channels: [general, old], preferred: null, remembered: null });
    expect(loaded.channels[1]?.archived_at).toBe(stamp);
    const refreshed = channelsReducer(state, { type: 'refreshed', channels: [general, { ...plain, archived_at: stamp }, old] });
    expect(isArchived(refreshed.channels[1])).toBe(true);
  });

  it('sets archived_at from a settings action and clears it when the server omits it', () => {
    const archived = channelsReducer(state, { type: 'settings', id: 2, visibility: undefined, default: undefined, archivedAt: stamp });
    expect(archived.channels[1]).toEqual({ ...plain, archived_at: stamp });
    expect(archived.selected).toBe('general');
    const restored = channelsReducer(archived, { type: 'settings', id: 2, visibility: undefined, default: undefined });
    expect(restored.channels[1]).toEqual(plain);
    expect('archived_at' in (restored.channels[1] ?? {})).toBe(false);
    // The same word twice changes nothing.
    expect(channelsReducer(archived, { type: 'settings', id: 2, visibility: undefined, default: undefined, archivedAt: stamp })).toBe(archived);
    expect(channelsReducer(state, { type: 'settings', id: 2, visibility: undefined, default: undefined })).toBe(state);
  });

  it('keeps the open channel open when it is archived', () => {
    const open: ChannelsState = { ...state, selected: 'plain' };
    const next = channelsReducer(open, { type: 'settings', id: 2, visibility: undefined, default: undefined, archivedAt: stamp });
    expect(next.selected).toBe('plain');
  });
});

describe('the Archived group', () => {
  it('is collapsed at the bottom, counts what is in it, and opens on click', () => {
    render(view([general, old, plain], false));
    expect(screen.getAllByRole('listitem').map((li) => li.textContent)).toEqual(['general', 'plain']);
    const toggle = screen.getByRole('button', { name: 'Archived (1)' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByRole('button', { name: 'old' })).toBeNull();
    fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    const group = screen.getByRole('list', { name: 'Archived channels' });
    expect(within(group).getByRole('button', { name: 'old' })).toBeDefined();
    fireEvent.click(toggle);
    expect(screen.queryByRole('list', { name: 'Archived channels' })).toBeNull();
  });

  it('draws nothing when no channel is archived', () => {
    render(view([general, plain], false));
    expect(screen.queryByRole('button', { name: /^Archived/ })).toBeNull();
  });

  it('is open while the open channel is archived, and a channel moves in and out live', () => {
    const shown = render(view([general, plain], false));
    expect(screen.queryByRole('button', { name: /^Archived/ })).toBeNull();
    shown.rerender(view([general, { ...plain, archived_at: stamp }], false, 'plain'));
    const group = screen.getByRole('list', { name: 'Archived channels' });
    expect(within(group).getByRole('button', { name: 'plain' }).getAttribute('aria-current')).toBe('true');
    shown.rerender(view([general, plain], false, 'plain'));
    expect(screen.queryByRole('list', { name: 'Archived channels' })).toBeNull();
    expect(screen.getByRole('button', { name: 'plain' })).toBeDefined();
  });

  it('is reached by the arrow keys once open', () => {
    render(view([general, old], false));
    fireEvent.click(screen.getByRole('button', { name: 'Archived (1)' }));
    fireEvent.keyDown(screen.getByRole('button', { name: 'general' }), { key: 'ArrowDown' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'old' }));
  });
});

describe('the ⋯ menu', () => {
  it('offers Archive channel… to a holder of manage_channels, and Restore on an archived one', () => {
    render(view([general, plain, old], true));
    expect(labels('plain')).toContain('Archive channel…');
    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    fireEvent.click(screen.getByRole('button', { name: 'Archived (1)' }));
    const items = labels('old');
    expect(items).toEqual(['Restore channel']);
  });

  it('offers neither to anyone else', () => {
    render(view([general, plain, old], false));
    expect(screen.queryByRole('button', { name: /^Actions for/ })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Archived (1)' }));
    expect(screen.queryByRole('button', { name: /^Actions for/ })).toBeNull();
  });

  it('disables Archive on a default channel, with the reason, and sends nothing', () => {
    render(view([general, plain], true));
    const item = within(menuOf('general')).getByRole('menuitem', { name: 'Archive channel…' });
    expect(item.getAttribute('aria-disabled')).toBe('true');
    expect(document.getElementById(item.getAttribute('aria-describedby') ?? '')?.textContent).toBe('Stop it being a default channel first.');
    fireEvent.click(item);
    expect(screen.queryByRole('group')).toBeNull();
    expect(settings).not.toHaveBeenCalled();
  });

  it('asks before archiving, says history stays, focuses Cancel, and Cancel sends nothing', () => {
    render(view([general, plain], true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Archive channel…' }));
    expect(screen.getByText(/Its history stays, and nothing new can be posted in it/)).toBeDefined();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('group')).toBeNull();
    expect(settings).not.toHaveBeenCalled();
  });

  it('archives once however often Archive channel is pressed, and hands the answer on', async () => {
    settings.mockResolvedValue(ok({ ...plain, archived_at: stamp }));
    render(view([general, plain], true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Archive channel…' }));
    const confirm = screen.getByRole('button', { name: 'Archive channel' });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    await waitFor(() => {
      expect(screen.queryByRole('group')).toBeNull();
    });
    expect(settings).toHaveBeenCalledTimes(1);
    expect(settings).toHaveBeenCalledWith(2, { archived: true });
    expect(applied).toEqual([{ ...plain, archived_at: stamp }]);
  });

  it('restores at once, with no question', async () => {
    settings.mockResolvedValue(ok(old.id === 3 ? { id: 3, name: 'old', created_at: at, visibility: 'public' } : old));
    render(view([general, old], true));
    fireEvent.click(screen.getByRole('button', { name: 'Archived (1)' }));
    fireEvent.click(within(menuOf('old')).getByRole('menuitem', { name: 'Restore channel' }));
    await waitFor(() => {
      expect(screen.queryByRole('group')).toBeNull();
    });
    expect(settings).toHaveBeenCalledWith(3, { archived: false });
    expect(applied).toHaveLength(1);
    expect(isArchived(applied[0])).toBe(false);
  });

  it('says a refusal in the page’s words and leaves it open to read', async () => {
    settings.mockResolvedValue({ ok: false, failure: 'invalid', code: 'channel_archive_default', message: leak });
    render(view([general, plain], true));
    fireEvent.click(within(menuOf('plain')).getByRole('menuitem', { name: 'Archive channel…' }));
    fireEvent.click(screen.getByRole('button', { name: 'Archive channel' }));
    expect(await screen.findByText('A default channel cannot be archived. Stop it being a default channel first.')).toBeDefined();
    expect(applied).toEqual([]);
  });
});

describe('the refusal lines', () => {
  const change = { kind: 'archived', to: true } as const;
  it('are the page’s own, by code', () => {
    expect(refusalLine('invalid', 'channel_archive_default', change)).toContain('Stop it being a default channel first');
    expect(refusalLine('invalid', 'channel_archived', change)).toBe(archivedLine);
    expect(refusalLine('invalid', 'permission_denied', change)).toContain('permission');
    expect(refusalLine('invalid', 'surprise', change)).toBe('The server could not change the channel. Nothing was changed.');
    expect(refusalLine('network', undefined, change)).toContain('Cannot reach the server');
  });

  it('turn an archived upload into the archived line, and other codes keep theirs', () => {
    expect(uploadRefusalLine('a.png', 'channel_archived', 'SERVER WORDS')).toBe(archivedLine);
    expect(uploadRefusalLine('a.png', 'uploads_disabled', 'off')).toBe('off');
  });
});
