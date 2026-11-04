// Issue #1095: Leave channel in a channel row's ⋯ menu. Public channels are
// left at once; private ones ask first. The API is a stand-in; nothing here
// needs a network or a browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { SetStateAction } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { ChannelList } from './ChannelList';
import type { ChannelV0 } from './schema/channel';

const reactState = vi.hoisted(() => ({ track: false, calls: 0 }));
vi.mock('react', async (importOriginal) => {
  const real = await importOriginal<typeof import('react')>();
  return {
    ...real,
    useState: <T,>(initial: T | (() => T)) => {
      const [value, setValue] = real.useState<T>(initial);
      if (!reactState.track) return [value, setValue] as const;
      return [
        value,
        (next: SetStateAction<T>) => {
          reactState.calls++;
          setValue(next);
        },
      ] as const;
    },
  };
});

vi.mock('./api', async (importOriginal) => {
  const real = await importOriginal<typeof import('./api')>();
  return { ...real, leaveChannel: vi.fn() };
});

const at = '2026-11-03T00:00:00.000Z';
const general: ChannelV0 = { id: 1, name: 'general', created_at: at, visibility: 'public', default: true };
const secret: ChannelV0 = { id: 2, name: 'secret', created_at: at, visibility: 'private' };
const managed: ChannelV0 = { id: 3, name: 'managed', created_at: at, manages: true };
const channels = [general, secret, managed];
const leave = vi.mocked(api.leaveChannel);

let left: string[];
let unauthorized: number;

function view(extra: Partial<Parameters<typeof ChannelList>[0]> = {}) {
  return (
    <ChannelList
      channels={channels}
      selected="general"
      unread={new Map()}
      onOpen={() => undefined}
      onLeft={(name) => {
        left.push(name);
      }}
      onUnauthorized={() => {
        unauthorized++;
      }}
      {...extra}
    />
  );
}

const row = (name: string) => screen.getByRole('button', { name }).closest('li') as HTMLElement;
const more = (name: string) => within(row(name)).getByRole('button', { name: `Actions for ${name}` });
const menuOf = (name: string) => {
  fireEvent.click(more(name));
  return screen.getByRole('menu', { name: `Actions for ${name}` });
};
const choose = (name: string, item: string) => {
  fireEvent.click(within(menuOf(name)).getByRole('menuitem', { name: item }));
};

beforeEach(() => {
  leave.mockReset();
  left = [];
  unauthorized = 0;
  reactState.track = false;
  reactState.calls = 0;
});
afterEach(cleanup);

describe('Leave channel in the row menu', () => {
  it('is offered to a member for public, private, default and managed channels', () => {
    render(view());
    for (const channel of channels) {
      expect(within(menuOf(channel.name)).getAllByRole('menuitem').map((item) => item.textContent)).toEqual(['Leave channel']);
      fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    }
  });

  it('is added without displacing Invite, Rename or the operator settings', () => {
    render(
      view({
        onInvite: () => undefined,
        canRename: () => true,
        renderRename: () => null,
        canManage: true,
        renderSettings: () => null,
      }),
    );
    expect(within(menuOf('general')).getAllByRole('menuitem').map((item) => item.textContent)).toEqual([
      'Invite people',
      'Rename channel…',
      'Leave channel',
      'Make private',
      'Stop being a default channel',
    ]);
  });

  it('leaves a public channel at once through the existing request and focuses the next row', async () => {
    leave.mockResolvedValue({ ok: true, value: null });
    render(view());
    choose('general', 'Leave channel');
    await waitFor(() => {
      expect(left).toEqual(['general']);
    });
    expect(leave).toHaveBeenCalledTimes(1);
    expect(leave).toHaveBeenCalledWith('general');
    expect(screen.queryByRole('group', { name: 'Leave general?' })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'secret' }));
  });

  it('asks before leaving a private channel; Escape and Cancel send nothing and return focus to the ⋯', () => {
    render(view());
    choose('secret', 'Leave channel');
    const ask = screen.getByRole('group', { name: 'Leave secret?' });
    expect(ask.textContent).toContain('Leave #secret? You will not find it again unless someone adds you back.');
    expect(document.activeElement).toBe(within(ask).getByRole('button', { name: 'Cancel' }));
    fireEvent.keyDown(ask, { key: 'Escape' });
    expect(screen.queryByRole('group', { name: 'Leave secret?' })).toBeNull();
    expect(document.activeElement).toBe(more('secret'));
    choose('secret', 'Leave channel');
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('group', { name: 'Leave secret?' })).toBeNull();
    expect(document.activeElement).toBe(more('secret'));
    expect(leave).not.toHaveBeenCalled();
  });

  it('sends the private-channel request only after Yes, leave', async () => {
    leave.mockResolvedValue({ ok: true, value: null });
    render(view());
    choose('secret', 'Leave channel');
    fireEvent.click(screen.getByRole('button', { name: 'Yes, leave' }));
    await waitFor(() => {
      expect(left).toEqual(['secret']);
    });
    expect(leave).toHaveBeenCalledTimes(1);
    expect(leave).toHaveBeenCalledWith('secret');
  });

  it('shows the last-member refusal in the page’s own words, stays in the channel and returns focus to the ⋯', async () => {
    leave.mockResolvedValue({
      ok: false,
      failure: 'invalid',
      code: 'channel_last_member',
      message: 'HOSTILE-SERVER-WORDS: you are the last member',
    });
    render(view());
    choose('secret', 'Leave channel');
    fireEvent.click(screen.getByRole('button', { name: 'Yes, leave' }));
    const said = await screen.findByRole('status');
    expect(said.textContent).toBe('You are the last member of this private channel. Add someone else before you leave.');
    expect(said.className).toContain('notice');
    expect(document.body.textContent).not.toContain('HOSTILE-SERVER-WORDS');
    expect(left).toEqual([]);
    expect(row('secret')).toBeDefined();
    expect(document.activeElement).toBe(more('secret'));
  });

  it.each([
    { name: 'network', result: { ok: false, failure: 'network' } as const, line: 'Cannot reach the server. Nothing was changed; try again.' },
    { name: 'channel_not_found', result: { ok: false, failure: 'invalid', code: 'channel_not_found', message: 'not shown' } as const, line: 'That channel is gone, or you can no longer see it.' },
  ])('shows the $name refusal and stays in the channel', async ({ result, line }) => {
    leave.mockResolvedValue(result);
    render(view());
    choose('general', 'Leave channel');
    expect((await screen.findByRole('status')).textContent).toBe(line);
    expect(left).toEqual([]);
    expect(row('general')).toBeDefined();
  });

  it('sends one request for two rapid Yes, leave clicks', () => {
    leave.mockReturnValue(new Promise(() => undefined));
    render(view());
    choose('secret', 'Leave channel');
    const yes = screen.getByRole('button', { name: 'Yes, leave' });
    fireEvent.click(yes);
    fireEvent.click(yes);
    expect(leave).toHaveBeenCalledTimes(1);
    expect(leave).toHaveBeenCalledWith('secret');
  });

  it('sets no state when a refusal arrives after the row unmounts', async () => {
    let answer: (result: Awaited<ReturnType<typeof api.leaveChannel>>) => void = () => undefined;
    leave.mockReturnValue(new Promise((resolve) => {
      answer = resolve;
    }));
    reactState.track = true;
    const mounted = render(view());
    choose('secret', 'Leave channel');
    fireEvent.click(screen.getByRole('button', { name: 'Yes, leave' }));
    mounted.unmount();
    reactState.calls = 0;
    await act(async () => {
      answer({ ok: false, failure: 'network' });
      await Promise.resolve();
    });
    reactState.track = false;
    expect(reactState.calls).toBe(0);
    expect(left).toEqual([]);
  });

  it('hands an expired credential to the page and clears the pending state without leaving locally', async () => {
    leave.mockResolvedValue({ ok: false, failure: 'unauthorized' });
    render(view());
    choose('general', 'Leave channel');
    await waitFor(() => {
      expect(unauthorized).toBe(1);
    });
    expect(left).toEqual([]);
    expect(screen.queryByRole('status')).toBeNull();
    expect(within(menuOf('general')).getByRole('menuitem', { name: 'Leave channel' }).getAttribute('aria-disabled')).toBeNull();
  });
});
