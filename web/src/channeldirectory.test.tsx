// Issue #991: the channel directory (states, Join, Leave and its last-member
// refusal), the lock of a private channel in the directory and the
// conversation's heading, and the community member panel. The API is a
// stand-in; nothing here needs a network or a browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { ChannelDirectory } from './ChannelDirectory';
import { CommunityMembers } from './Members';
import { ShowChat } from './phone';
import type { ChannelListingV1, ChannelV0 } from './schema/channel';
import type { ServerMemberV1 } from './schema/principal';
import { channelsReducer, type ChannelsState } from './state/channels';

vi.mock('./api', async (importOriginal) => {
  const real = await importOriginal<typeof import('./api')>();
  return { ...real, channelDirectory: vi.fn(), joinChannel: vi.fn(), leaveChannel: vi.fn(), directoryMembers: vi.fn() };
});

const at = '2026-11-02T00:00:00.000Z';
const directory = vi.mocked(api.channelDirectory);
const join = vi.mocked(api.joinChannel);
const leave = vi.mocked(api.leaveChannel);
const people = vi.mocked(api.directoryMembers);

const general: ChannelListingV1 = { id: 1, name: 'general', created_at: at, visibility: 'public', default: true, joined: true };
const ideas: ChannelListingV1 = { id: 2, name: 'ideas', created_at: at, visibility: 'public', joined: false };
const vault: ChannelListingV1 = { id: 3, name: 'vault', created_at: at, visibility: 'private', joined: true, manages: true };
const listing = [general, ideas, vault];

const asChannel = (c: ChannelListingV1): ChannelV0 => ({ id: c.id, name: c.name, created_at: c.created_at, visibility: c.visibility, ...(c.default === true ? { default: true } : {}) });

type Handlers = { joined: ChannelV0[]; left: string[]; opened: string[]; unauthorized: number; chat: number };
let seen: Handlers;

function view(channels: readonly ChannelV0[] = []) {
  return (
    <ShowChat.Provider
      value={() => {
        seen.chat++;
      }}
    >
      <ChannelDirectory
        channels={channels}
        onJoined={(c) => seen.joined.push(c)}
        onLeft={(n) => seen.left.push(n)}
        onOpen={(n) => seen.opened.push(n)}
        onUnauthorized={() => seen.unauthorized++}
      />
    </ShowChat.Provider>
  );
}

async function opened() {
  const out = render(view());
  fireEvent.click(screen.getByRole('button', { name: 'Browse channels' }));
  await screen.findByRole('list', { name: 'All channels' });
  return out;
}

const rowOf = (name: string) => {
  const found = screen.getAllByRole('listitem').find((li) => li.querySelector('.channel-name')?.textContent === name);
  if (found === undefined) throw new Error(`no row for ${name}`);
  return found;
};

beforeEach(() => {
  seen = { joined: [], left: [], opened: [], unauthorized: 0, chat: 0 };
  directory.mockReset();
  join.mockReset();
  leave.mockReset();
  people.mockReset();
  directory.mockResolvedValue({ ok: true, value: listing });
});
afterEach(cleanup);

describe('the directory', () => {
  it('reads nothing until it is opened, then lists what the server answered, marked', async () => {
    render(view());
    expect(directory).not.toHaveBeenCalled();
    const toggle = screen.getByRole('button', { name: 'Browse channels' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    await screen.findByRole('list', { name: 'All channels' });
    expect(directory).toHaveBeenCalledTimes(1);
    expect(screen.getAllByRole('listitem').map((li) => li.querySelector('.channel-name')?.textContent)).toEqual(['general', 'ideas', 'vault']);
    // general: a default channel, joined, no lock, can be left.
    const g = rowOf('general');
    expect(within(g).getByText('Default')).toBeDefined();
    expect(within(g).getByText('Joined')).toBeDefined();
    expect(g.querySelector('svg')).toBeNull();
    expect(within(g).getByRole('button', { name: 'Leave general' })).toBeDefined();
    // ideas: public, not joined: Join, and its name is not a link into it.
    const i = rowOf('ideas');
    expect(within(i).queryByText('Joined')).toBeNull();
    expect(within(i).getByRole('button', { name: 'Join ideas' })).toBeDefined();
    expect(within(i).queryByRole('button', { name: 'ideas' })).toBeNull();
    // vault: private and managed, with the lock and "Private" for a reader.
    const v = rowOf('vault');
    expect(v.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true');
    expect(v.querySelector('.private-mark .vh')?.textContent).toBe('Private');
    expect(within(v).getByText('Manager')).toBeDefined();
    expect(within(v).queryByText('Default')).toBeNull();
    // A public row has no private mark.
    expect(i.querySelector('.private-mark')).toBeNull();
  });

  it('shows no counts of people, even if a row carried one', async () => {
    directory.mockResolvedValue({ ok: true, value: [{ ...ideas, members: 41 }, { ...vault, members: 7 }] });
    await opened();
    const text = screen.getByRole('region', { name: 'Channel directory' }).textContent;
    expect(text).not.toMatch(/\b41\b|\b7\b|member|people/i);
  });

  it('says so when the server could not answer, and tries again', async () => {
    directory.mockResolvedValueOnce({ ok: false, failure: 'network' });
    render(view());
    fireEvent.click(screen.getByRole('button', { name: 'Browse channels' }));
    await screen.findByText('Cannot load the channels.');
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByRole('list', { name: 'All channels' });
  });

  it('goes back to sign-in on a 401', async () => {
    directory.mockResolvedValueOnce({ ok: false, failure: 'unauthorized' });
    render(view());
    fireEvent.click(screen.getByRole('button', { name: 'Browse channels' }));
    await waitFor(() => {
      expect(seen.unauthorized).toBe(1);
    });
  });

  it('reads again when the person’s own list changes while it is open', async () => {
    const out = await opened();
    expect(directory).toHaveBeenCalledTimes(1);
    out.rerender(view([asChannel(general)]));
    await waitFor(() => {
      expect(directory).toHaveBeenCalledTimes(2);
    });
  });

  it('keeps nothing once closed: a private row it listed is not drawn again from memory when it is reopened', async () => {
    await opened();
    expect(screen.getByText('vault')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Browse channels' }));
    expect(screen.queryByText('vault')).toBeNull();
    // Removed from the private channel meanwhile: the next answer omits it.
    let answer: (r: Awaited<ReturnType<typeof api.channelDirectory>>) => void = () => undefined;
    directory.mockReturnValueOnce(new Promise((resolve) => (answer = resolve)));
    fireEvent.click(screen.getByRole('button', { name: 'Browse channels' }));
    expect(screen.queryByText('vault')).toBeNull();
    expect(screen.getByText('Loading channels…')).toBeDefined();
    await act(async () => {
      answer({ ok: true, value: [general] });
      await Promise.resolve();
    });
    expect(screen.queryByText('vault')).toBeNull();
  });

  it('opens a joined channel from its name, and on a phone returns to Chat', async () => {
    await opened();
    fireEvent.click(within(rowOf('vault')).getByRole('button', { name: 'vault' }));
    expect(seen.opened).toEqual(['vault']);
    expect(seen.chat).toBe(1);
  });
});

describe('Join', () => {
  it('sends one request however often it is pressed, hands over the channel and opens it', async () => {
    let answer: (r: Awaited<ReturnType<typeof api.joinChannel>>) => void = () => undefined;
    join.mockReturnValue(new Promise((resolve) => (answer = resolve)));
    await opened();
    const button = within(rowOf('ideas')).getByRole('button', { name: 'Join ideas' });
    fireEvent.click(button);
    fireEvent.click(button);
    expect(join).toHaveBeenCalledTimes(1);
    expect(join).toHaveBeenCalledWith('ideas');
    expect(button.hasAttribute('disabled')).toBe(true);
    const channel = asChannel(ideas);
    await act(async () => {
      answer({ ok: true, value: { channel, member: { principal_id: 2, created_at: at, kind: 'human', can_decide: false, manages: false } } });
      await Promise.resolve();
    });
    expect(seen.joined).toEqual([channel]);
    expect(seen.chat).toBe(1);
    expect(await screen.findByText('Joined #ideas')).toBeDefined();
  });

  it.each([
    ['channel_not_joinable', 'You cannot join this channel.'],
    ['channel_not_found', 'That channel is gone, or you can no longer see it.'],
    ['internal_error', 'The server could not add you to the channel. Try again.'],
  ])('says the page’s own words for %s, never the server’s message', async (code, line) => {
    join.mockResolvedValue({ ok: false, failure: 'invalid', code, message: 'secret-channel-name exists but you may not see it' });
    await opened();
    fireEvent.click(within(rowOf('ideas')).getByRole('button', { name: 'Join ideas' }));
    const said = await screen.findByText(line);
    expect(said.getAttribute('role')).toBe('status');
    expect(document.body.textContent).not.toContain('secret-channel-name');
    expect(seen.joined).toEqual([]);
    // The directory may be out of date: it is read again.
    await waitFor(() => {
      expect(directory).toHaveBeenCalledTimes(2);
    });
  });

  it('says so when there is no answer, and changes nothing', async () => {
    join.mockResolvedValue({ ok: false, failure: 'network' });
    await opened();
    fireEvent.click(within(rowOf('ideas')).getByRole('button', { name: 'Join ideas' }));
    await screen.findByText('Cannot reach the server. Nothing was changed; try again.');
    expect(seen.joined).toEqual([]);
    expect(within(rowOf('ideas')).getByRole('button', { name: 'Join ideas' }).hasAttribute('disabled')).toBe(false);
  });
});

describe('Leave', () => {
  it('leaves a public channel at once', async () => {
    leave.mockResolvedValue({ ok: true, value: null });
    await opened();
    fireEvent.click(within(rowOf('general')).getByRole('button', { name: 'Leave general' }));
    await screen.findByText('Left #general');
    expect(leave).toHaveBeenCalledWith('general');
    expect(seen.left).toEqual(['general']);
  });

  it('asks before leaving a private channel, with focus on Cancel; Escape and Cancel send nothing', async () => {
    await opened();
    const leaveVault = within(rowOf('vault')).getByRole('button', { name: 'Leave vault' });
    fireEvent.click(leaveVault);
    const ask = screen.getByRole('group', { name: 'Leave vault?' });
    expect(within(ask).getByText(/not find it again/)).toBeDefined();
    expect(document.activeElement).toBe(within(ask).getByRole('button', { name: 'Cancel' }));
    fireEvent.keyDown(ask, { key: 'Escape' });
    expect(screen.queryByRole('group', { name: 'Leave vault?' })).toBeNull();
    expect(document.activeElement).toBe(leaveVault);
    fireEvent.click(leaveVault);
    fireEvent.click(within(screen.getByRole('group', { name: 'Leave vault?' })).getByRole('button', { name: 'Cancel' }));
    expect(leave).not.toHaveBeenCalled();
  });

  it('shows why the last member of a private channel cannot leave, and stays in it', async () => {
    leave.mockResolvedValue({ ok: false, failure: 'invalid', code: 'channel_last_member', message: 'you are the last member of this private channel: someone else must be added before you leave' });
    await opened();
    fireEvent.click(within(rowOf('vault')).getByRole('button', { name: 'Leave vault' }));
    fireEvent.click(within(screen.getByRole('group', { name: 'Leave vault?' })).getByRole('button', { name: 'Yes, leave' }));
    const said = await screen.findByText('You are the last member of this private channel. Add someone else before you leave.');
    expect(said.getAttribute('role')).toBe('status');
    expect(seen.left).toEqual([]);
    expect(within(rowOf('vault')).getByRole('button', { name: 'Leave vault' })).toBeDefined();
  });
});

describe('the channels reducer', () => {
  const state: ChannelsState = { channels: [asChannel(general), asChannel(ideas)], loaded: true, selected: 'ideas' };
  it('drops a channel the person left, and gives the open one way to the landing channel, with no loss notice', () => {
    const next = channelsReducer(state, { type: 'left', name: 'ideas' });
    expect(next.channels.map((c) => c.name)).toEqual(['general']);
    expect(next.selected).toBe('general');
    expect(next.lost).toBeUndefined();
    expect(channelsReducer(state, { type: 'left', name: 'general' }).selected).toBe('ideas');
    expect(channelsReducer(state, { type: 'left', name: 'nope' })).toBe(state);
  });
});

describe('the community member panel', () => {
  const ada: ServerMemberV1 = { id: 1, kind: 'human', name: 'ada', role: 'operator', created_at: at };
  const bob: ServerMemberV1 = { id: 2, kind: 'human', name: 'bob', role: 'member', created_at: at };
  const bot: ServerMemberV1 = { id: 3, kind: 'agent', name: 'builder', created_at: at };

  function panel(ids = '1') {
    return render(<CommunityMembers viewer={2} channelIds={ids} onUnauthorized={() => seen.unauthorized++} />);
  }

  it('lists every member the server sent, marks agents and shows roles, with no guests and no counts', async () => {
    people.mockResolvedValue({ ok: true, value: { members: [ada, bob, bot] } });
    panel();
    const list = await screen.findByRole('list', { name: 'Community members' });
    const items = within(list).getAllByRole('listitem');
    expect(items.map((li) => li.querySelector('.community-name')?.textContent)).toEqual(['ada', 'bob', 'builder']);
    expect(within(items[0] as HTMLElement).getByText('Operator')).toBeDefined();
    expect(within(items[1] as HTMLElement).getByText('You')).toBeDefined();
    expect(within(items[1] as HTMLElement).queryByText('Operator')).toBeNull();
    expect(within(items[2] as HTMLElement).getByText('Agent')).toBeDefined();
    expect(within(items[0] as HTMLElement).queryByText('Agent')).toBeNull();
    expect(screen.queryByText(/guest/i)).toBeNull();
    expect(screen.getByRole('region', { name: 'Community members' }).textContent).not.toMatch(/\b3\b/);
  });

  it('shows the operator fields only for rows that carry them', async () => {
    const detailed: ServerMemberV1 = { ...bob, owner: false, voice_allowed: false, may_invite: true, invited_by: 1 };
    people.mockResolvedValue({ ok: true, value: { members: [ada, detailed] } });
    panel();
    const list = await screen.findByRole('list', { name: 'Community members' });
    const [first, second] = within(list).getAllByRole('listitem');
    expect(first?.textContent).toBe('adaOperator');
    expect(second?.textContent).toContain('voice off');
    expect(second?.textContent).toContain('can invite');
    expect(second?.textContent).toContain('invited by ada');
    expect(second?.textContent).not.toContain('Owner');
  });

  it('says when the list is only the people the viewer shares a channel with', async () => {
    people.mockResolvedValue({ ok: true, value: { members: [bob], scope: 'shared_channels' } });
    panel();
    await screen.findByText('Showing the people you share a channel with.');
  });

  it('says nothing about who it could not list when the server refuses', async () => {
    people.mockResolvedValue({ ok: false, failure: 'invalid', code: 'forbidden' });
    panel();
    const said = await screen.findByText('Cannot load the community members.');
    expect(said.getAttribute('role')).toBe('status');
    expect(screen.queryByRole('list', { name: 'Community members' })).toBeNull();
  });

  it('is read again when the person’s own channels change, and on Refresh; a 401 signs out', async () => {
    people.mockResolvedValue({ ok: true, value: { members: [bob] } });
    const out = panel('1');
    await screen.findByRole('list', { name: 'Community members' });
    expect(people).toHaveBeenCalledTimes(1);
    out.rerender(<CommunityMembers viewer={2} channelIds="1,2" onUnauthorized={() => seen.unauthorized++} />);
    await waitFor(() => {
      expect(people).toHaveBeenCalledTimes(2);
    });
    people.mockResolvedValueOnce({ ok: false, failure: 'unauthorized' });
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(seen.unauthorized).toBe(1);
    });
  });
});
