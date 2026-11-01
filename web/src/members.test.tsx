import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { MemberRow, Members, type MemberControlsProps } from './Members';
import type { Entry } from './agents/join';
import { longPressMs } from './MessageMenu';
import type { ChannelMemberV1 } from './schema/channel';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    addChannelMember: vi.fn(),
    listMembers: vi.fn(),
    putChannelMember: vi.fn(),
    removeChannelMember: vi.fn(),
  };
});

const at = '2026-10-25T00:00:00.000Z';
const channel = { id: 1, name: 'general', created_at: at, manages: true };
const channels = [channel, { id: 2, name: 'ops', created_at: at }, { id: 3, name: 'dev', created_at: at }];
const memberList: ChannelMemberV1[] = [
  { principal_id: 1, created_at: at, name: 'olivia', kind: 'human', role: 'operator', can_decide: true, manages: false },
  { principal_id: 2, created_at: at, name: 'ada', kind: 'human', role: 'member', can_decide: false, manages: true },
  { principal_id: 3, created_at: at, name: 'avery', kind: 'human', role: 'member', can_decide: true, manages: false },
  { principal_id: 4, created_at: at, name: 'bot', kind: 'agent', manages: false },
  {
    principal_id: 5,
    created_at: at,
    kind: 'human',
    can_decide: false,
    manages: false,
    guest: { handle: 'guest-a7k4m2q9x3h5t8wz', label: 'Pat', tag: 'a7k4' },
  },
];

function entry(member: ChannelMemberV1): Entry {
  const name = member.name ?? member.guest?.label ?? `#${String(member.principal_id)}`;
  return { id: member.principal_id, name, kind: member.kind ?? 'human', sessions: [], live: 0, state: 'offline', status: 'offline' };
}

const people = memberList.filter((member) => member.kind === 'human').map(entry);
const refreshMembers = vi.fn();
const onUnauthorized = vi.fn();

function props(role: 'operator' | 'member', guest = false): MemberControlsProps {
  return { channel, channels, members: memberList, viewer: { role, guest }, refreshMembers, onUnauthorized };
}

function controlsView(controlProps: MemberControlsProps) {
  return (
    <>
      <Members {...controlProps} people={people} />
      <ul aria-label="Agents">
        <MemberRow key={`${String(controlProps.channel.id)}:4`} {...controlProps} principalId={4} className="ag-agent">
          <span className="ag-name">bot</span>
        </MemberRow>
      </ul>
    </>
  );
}

function draw(controlProps: MemberControlsProps) {
  return render(controlsView(controlProps));
}

// The row of a person, found by the name in it.
function row(name: string): HTMLElement {
  const found = screen.getAllByRole('listitem').find((item) => within(item).queryByText(name, { exact: true }) !== null);
  if (found === undefined) throw new Error(`no row for ${name}`);
  return found;
}

function openMenu(name: string): HTMLElement {
  fireEvent.click(within(row(name)).getByRole('button', { name: `Actions for ${name}` }));
  return screen.getByRole('menu', { name: `Actions for ${name}` });
}

function labels(menu: HTMLElement): string[] {
  return within(menu).getAllByRole('menuitem').map((item) => item.textContent);
}

function choose(name: string, item: string) {
  fireEvent.click(within(openMenu(name)).getByRole('menuitem', { name: item }));
}

// confirmRemoval answers the question "Remove from channel" asks.
function confirmRemoval(name: string) {
  fireEvent.click(within(row(name)).getByRole('button', { name: 'Yes, remove' }));
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.listMembers).mockResolvedValue({ ok: true, value: [] });
});

afterEach(cleanup);

describe('removing someone from a channel asks first (issue #1083)', () => {
  it.each([
    { who: 'an operator', role: 'operator' as const, target: 'bot', id: 4, channelName: 'general' },
    { who: 'an operator, of a person', role: 'operator' as const, target: 'avery', id: 3, channelName: 'general' },
    { who: 'a channel manager', role: 'member' as const, target: 'ada', id: 2, channelName: 'general' },
  ])('$who: the menu item shows the question, with focus on Cancel, and sends nothing', ({ role, target, channelName }) => {
    draw(props(role));
    choose(target, 'Remove from channel');
    expect(api.removeChannelMember).not.toHaveBeenCalled();
    expect(screen.queryByRole('menu')).toBeNull();
    const group = within(row(target)).getByRole('group', { name: `Remove ${target} from #${channelName}? They lose this channel and its history.` });
    expect(within(group).getByRole('button', { name: 'Yes, remove' })).toBeDefined();
    expect(document.activeElement).toBe(within(group).getByRole('button', { name: 'Cancel' }));
  });

  it.each([
    { how: 'Cancel', leave: (name: string) => fireEvent.click(within(row(name)).getByRole('button', { name: 'Cancel' })) },
    { how: 'Escape', leave: (name: string) => fireEvent.keyDown(within(row(name)).getByRole('button', { name: 'Cancel' }), { key: 'Escape' }) },
  ])('$how leaves the person in the channel, sends nothing and returns focus to the row’s ⋯ button', ({ leave }) => {
    draw(props('operator'));
    choose('avery', 'Remove from channel');
    leave('avery');
    expect(within(row('avery')).queryByRole('group')).toBeNull();
    expect(api.removeChannelMember).not.toHaveBeenCalled();
    expect(refreshMembers).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(within(row('avery')).getByRole('button', { name: 'Actions for avery' }));
  });

  it.each([
    { who: 'an operator', role: 'operator' as const, target: 'avery', id: 3 },
    { who: 'a channel manager', role: 'member' as const, target: 'ada', id: 2 },
  ])('$who: "Yes, remove" sends one request', async ({ role, target, id }) => {
    vi.mocked(api.removeChannelMember).mockResolvedValue({ ok: true, value: null });
    draw(props(role));
    choose(target, 'Remove from channel');
    confirmRemoval(target);
    await waitFor(() => {
      expect(refreshMembers).toHaveBeenCalledTimes(1);
    });
    expect(api.removeChannelMember).toHaveBeenCalledTimes(1);
    expect(api.removeChannelMember).toHaveBeenCalledWith('general', id);
    expect(within(row(target)).queryByRole('group')).toBeNull();
  });

  it('a refusal after the yes shows the same line as before, in the row', async () => {
    vi.mocked(api.removeChannelMember).mockResolvedValue({ ok: false, failure: 'invalid', code: 'forbidden', message: 'the owner membership is protected' });
    draw(props('operator'));
    choose('avery', 'Remove from channel');
    confirmRemoval('avery');
    expect((await within(row('avery')).findByText('the owner membership is protected')).getAttribute('role')).toBe('status');
  });

  it('making a manager and the approver item do not ask', async () => {
    vi.mocked(api.putChannelMember).mockResolvedValue({ ok: true, value: null });
    draw(props('operator'));
    choose('avery', 'Make manager');
    expect(within(row('avery')).queryByRole('group')).toBeNull();
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledTimes(1);
    });
    await waitFor(() => {
      expect(refreshMembers).toHaveBeenCalledTimes(1);
    });
    choose('avery', 'Stop deciding approvals');
    expect(within(row('avery')).queryByRole('group')).toBeNull();
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledTimes(2);
    });
  });
});

describe('administration controls', () => {
  it('gives an operator add and, in each row, a menu with the items the target allows', () => {
    draw(props('operator'));
    expect(screen.getByRole('form', { name: 'Add person by name' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Add from another channel' })).toBeDefined();
    // Nothing is inline any more.
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(screen.queryByRole('button', { name: /^(Remove|Make)/ })).toBeNull();
    expect(screen.queryByRole('menu')).toBeNull();
    expect(labels(openMenu('olivia'))).toEqual(['Remove from channel', 'Make manager', 'Stop deciding approvals']);
    expect(labels(openMenu('ada'))).toEqual(['Remove from channel', 'Remove as manager', 'Let decide approvals']);
    expect(labels(openMenu('avery'))).toEqual(['Remove from channel', 'Make manager', 'Stop deciding approvals']);
    fireEvent.click(within(row('Pat')).getByRole('button', { name: 'Actions for Guest: Pat · a7k4' }));
    expect(labels(screen.getByRole('menu', { name: 'Actions for Guest: Pat · a7k4' }))).toEqual(['Remove from channel']);
    expect(labels(openMenu('bot'))).toEqual(['Remove from channel']);
    expect(screen.getAllByText('Manager')).toHaveLength(1);
    expect(screen.getAllByText('Approver')).toHaveLength(2);
  });

  it('gives a manager add and a menu of only the removals the rules allow, with no per-row explanations', () => {
    draw(props('member'));
    expect(screen.getByRole('form', { name: 'Add person by name' })).toBeDefined();
    expect(labels(openMenu('ada'))).toEqual(['Remove from channel']);
    for (const name of ['olivia', 'avery', 'bot']) {
      expect(within(row(name)).queryByRole('button', { name: `Actions for ${name}` })).toBeNull();
    }
    expect(screen.queryByText('Operators can only be removed by an operator.')).toBeNull();
    expect(screen.queryByText('Approvers are removed by an operator.')).toBeNull();
    expect(screen.queryByText('Agents are managed by operators.')).toBeNull();
    expect(screen.getByText('Only operators choose who manages a channel.')).toBeDefined();
    expect(screen.getByText('Only operators choose who decides approvals.')).toBeDefined();
    expect(screen.queryByRole('checkbox')).toBeNull();
  });

  it.each([false, true])('gives a plain member or guest no menu and no button (guest: %s)', (guest) => {
    draw({ ...props('member', guest), channel: { ...channel, manages: false } });
    expect(screen.queryByRole('form', { name: 'Add person by name' })).toBeNull();
    expect(screen.queryByRole('button', { name: /^(Add|Remove|Make|Actions)/ })).toBeNull();
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(screen.getByText('Only operators and this channel\'s managers add people.')).toBeDefined();
    expect(screen.getByText('Only operators choose who manages a channel.')).toBeDefined();
    expect(screen.getByText('Only operators choose who decides approvals.')).toBeDefined();
    if (guest) expect(screen.getByText('A guest visit cannot do this; create an account and ask an operator.')).toBeDefined();
    // The native menu is left alone on a row with no actions.
    expect(fireEvent.contextMenu(row('ada'))).toBe(true);
  });
});

describe('opening a row\'s menu', () => {
  it('opens by right-click on the row, suppressing the native menu there only', () => {
    draw(props('operator'));
    expect(fireEvent.contextMenu(row('ada'))).toBe(false);
    expect(screen.getByRole('menu', { name: 'Actions for ada' })).toBeDefined();
    expect(fireEvent.contextMenu(screen.getByRole('heading', { name: 'People' }))).toBe(true);
  });

  it('opens by a long press', () => {
    vi.useFakeTimers();
    try {
      draw(props('operator'));
      fireEvent.touchStart(row('ada'), { touches: [{ clientX: 5, clientY: 5 }] });
      act(() => {
        vi.advanceTimersByTime(longPressMs);
      });
      expect(screen.getByRole('menu', { name: 'Actions for ada' })).toBeDefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it('opens from the button, focuses the first item, and Escape closes it and focuses the button', () => {
    draw(props('operator'));
    const button = within(row('ada')).getByRole('button', { name: 'Actions for ada' });
    expect(button.getAttribute('aria-haspopup')).toBe('menu');
    expect(button.getAttribute('aria-expanded')).toBe('false');
    // Enter and Space on a button are clicks in a browser.
    fireEvent.click(button);
    const menu = screen.getByRole('menu', { name: 'Actions for ada' });
    expect(button.getAttribute('aria-expanded')).toBe('true');
    expect(document.activeElement).toBe(within(menu).getAllByRole('menuitem')[0]);
    fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(within(menu).getAllByRole('menuitem')[1]);
    fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });
    expect(screen.queryByRole('menu')).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it('closes on an outside press', () => {
    draw(props('operator'));
    openMenu('ada');
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole('menu')).toBeNull();
  });
});

describe('add by exact name', () => {
  it('adds the exact text and refreshes the list', async () => {
    vi.mocked(api.addChannelMember).mockResolvedValue({ ok: true, value: memberList[1] as ChannelMemberV1 });
    draw(props('member'));
    fireEvent.change(screen.getByLabelText('Exact name'), { target: { value: '  exact <name>  ' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add by name' }));
    await waitFor(() => {
      expect(api.addChannelMember).toHaveBeenCalledWith('general', '  exact <name>  ');
    });
    await waitFor(() => {
      expect(refreshMembers).toHaveBeenCalledTimes(1);
    });
    expect(screen.getByLabelText<HTMLInputElement>('Exact name').value).toBe('');
  });

  it.each(['principal_not_found', 'forbidden', 'invalid_request', 'rate_limited', 'internal_error'])('collapses %s to the one refusal text', async (code) => {
    vi.mocked(api.addChannelMember).mockResolvedValue({ ok: false, failure: 'invalid', code, message: 'a server detail that must not be shown' });
    draw(props('operator'));
    fireEvent.change(screen.getByLabelText('Exact name'), { target: { value: 'nobody' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add by name' }));
    expect(await screen.findByText('No one by that name can be added here.')).toBeDefined();
    expect(screen.queryByText(/server detail/)).toBeNull();
    expect(refreshMembers).toHaveBeenCalledTimes(1);
  });
});

describe('the shared-channel picker', () => {
  it('reads only other channels, excludes current people, guests and agents, deduplicates and adds by id', async () => {
    const cara: ChannelMemberV1 = { principal_id: 6, created_at: at, name: 'cara', kind: 'human', can_decide: false };
    const dan: ChannelMemberV1 = { principal_id: 7, created_at: at, name: 'dan', kind: 'human', can_decide: false };
    const visitor: ChannelMemberV1 = { principal_id: 8, created_at: at, kind: 'human', guest: { handle: 'guest-z7k4m2q9x3h5t8wz', tag: 'z7k4' } };
    vi.mocked(api.listMembers).mockImplementation((name) => {
      if (name === 'ops') return Promise.resolve({ ok: true, value: [memberList[0] as ChannelMemberV1, cara, memberList[1] as ChannelMemberV1, memberList[3] as ChannelMemberV1, visitor] });
      if (name === 'dev') return Promise.resolve({ ok: true, value: [cara, dan] });
      throw new Error(`the current channel must not be read: ${name}`);
    });
    vi.mocked(api.putChannelMember).mockResolvedValue({ ok: true, value: null });
    draw(props('member'));
    fireEvent.click(screen.getByRole('button', { name: 'Add from another channel' }));
    const form = await screen.findByRole('form', { name: 'Add person from another channel' });
    await within(form).findByRole('option', { name: 'cara' });
    expect(api.listMembers).toHaveBeenCalledTimes(2);
    expect(vi.mocked(api.listMembers).mock.calls.map(([name]) => name).sort()).toEqual(['dev', 'ops']);
    expect(within(form).getAllByRole('option').map((option) => option.textContent)).toEqual(['Choose a person', 'cara', 'dan']);
    fireEvent.change(within(form).getByLabelText('Person from another channel'), { target: { value: '6' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Add selected person' }));
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledWith('general', 6, {});
    });
    expect(refreshMembers).toHaveBeenCalledTimes(1);
  });
});

describe('membership writes', () => {
  it('sets manages and can_decide independently and removes by principal id', async () => {
    vi.mocked(api.putChannelMember).mockResolvedValue({ ok: true, value: null });
    vi.mocked(api.removeChannelMember).mockResolvedValue({ ok: true, value: null });
    draw(props('operator'));
    choose('ada', 'Remove as manager');
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledWith('general', 2, { manages: false });
    });
    choose('avery', 'Stop deciding approvals');
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledWith('general', 3, { can_decide: false });
    });
    choose('ada', 'Let decide approvals');
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledWith('general', 2, { can_decide: true });
    });
    choose('avery', 'Make manager');
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledWith('general', 3, { manages: true });
    });
    choose('bot', 'Remove from channel');
    confirmRemoval('bot');
    await waitFor(() => {
      expect(api.removeChannelMember).toHaveBeenCalledWith('general', 4);
    });
    expect(refreshMembers).toHaveBeenCalledTimes(5);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('shows the server message in the row and refreshes after a refusal of an offered action', async () => {
    vi.mocked(api.putChannelMember).mockResolvedValue({ ok: false, failure: 'invalid', code: 'forbidden', message: 'the owner membership is protected' });
    draw(props('operator'));
    choose('olivia', 'Make manager');
    const line = await screen.findByText('the owner membership is protected');
    expect(line.getAttribute('role')).toBe('status');
    expect(row('olivia').contains(line)).toBe(true);
    expect(refreshMembers).toHaveBeenCalledTimes(1);
  });

  it('uses the existing unauthorized path and does not claim a change', async () => {
    vi.mocked(api.removeChannelMember).mockResolvedValue({ ok: false, failure: 'unauthorized' });
    draw(props('member'));
    choose('ada', 'Remove from channel');
    confirmRemoval('ada');
    await waitFor(() => {
      expect(onUnauthorized).toHaveBeenCalledTimes(1);
    });
    expect(refreshMembers).not.toHaveBeenCalled();
  });
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const laterChannel = { id: 9, name: 'later', created_at: at, manages: true };
const switched = (role: 'operator' | 'member'): MemberControlsProps => ({ ...props(role), channel: laterChannel, channels: [...channels, laterChannel] });

async function refuseLater<T>(pending: { readonly promise: Promise<T>; readonly resolve: (value: T) => void }, result: T) {
  await act(async () => {
    pending.resolve(result);
    await pending.promise;
  });
}

describe('a pending action', () => {
  it('shows every item of the row as disabled, sends nothing for a choice made meanwhile, and is usable again after', async () => {
    const pending = deferred<api.InviteResult<null>>();
    vi.mocked(api.putChannelMember).mockReturnValueOnce(pending.promise);
    vi.mocked(api.removeChannelMember).mockResolvedValue({ ok: true, value: null });
    draw(props('operator'));
    choose('avery', 'Make manager');
    expect(api.putChannelMember).toHaveBeenCalledTimes(1);

    const menu = openMenu('avery');
    for (const item of within(menu).getAllByRole('menuitem')) expect(item.getAttribute('aria-disabled')).toBe('true');
    fireEvent.click(within(menu).getByRole('menuitem', { name: 'Remove from channel' }));
    expect(api.removeChannelMember).not.toHaveBeenCalled();
    expect(api.putChannelMember).toHaveBeenCalledTimes(1);
    // It stays open: nothing was chosen.
    expect(screen.getByRole('menu', { name: 'Actions for avery' })).toBeDefined();

    await refuseLater(pending, { ok: true, value: null });
    for (const item of within(screen.getByRole('menu', { name: 'Actions for avery' })).getAllByRole('menuitem')) expect(item.getAttribute('aria-disabled')).toBeNull();
    fireEvent.click(within(screen.getByRole('menu', { name: 'Actions for avery' })).getByRole('menuitem', { name: 'Remove from channel' }));
    confirmRemoval('avery');
    await waitFor(() => {
      expect(api.removeChannelMember).toHaveBeenCalledWith('general', 3);
    });
  });
});

describe('pending administration actions on a channel switch', () => {
  it('resets add-by-name and ignores the old channel response', async () => {
    const pending = deferred<api.InviteResult<ChannelMemberV1>>();
    vi.mocked(api.addChannelMember).mockReturnValue(pending.promise);
    const view = draw(props('operator'));
    fireEvent.change(screen.getByLabelText('Exact name'), { target: { value: 'old channel name' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add by name' }));
    expect(screen.getByRole('button', { name: 'Add by name' }).hasAttribute('disabled')).toBe(true);

    view.rerender(controlsView(switched('operator')));
    fireEvent.change(screen.getByLabelText('Exact name'), { target: { value: 'new channel name' } });
    expect(screen.getByLabelText<HTMLInputElement>('Exact name').value).toBe('new channel name');
    expect(screen.getByRole('button', { name: 'Add by name' }).hasAttribute('disabled')).toBe(false);
    await refuseLater(pending, { ok: false, failure: 'invalid', code: 'principal_not_found' });
    expect(screen.queryByText('No one by that name can be added here.')).toBeNull();
    expect(screen.getByLabelText<HTMLInputElement>('Exact name').value).toBe('new channel name');
  });

  it('resets add-selected and ignores the old channel response', async () => {
    const cara: ChannelMemberV1 = { principal_id: 6, created_at: at, name: 'cara', kind: 'human' };
    vi.mocked(api.listMembers).mockResolvedValue({ ok: true, value: [cara] });
    const pending = deferred<api.InviteResult<null>>();
    vi.mocked(api.putChannelMember).mockReturnValue(pending.promise);
    const view = draw(props('member'));
    fireEvent.click(screen.getByRole('button', { name: 'Add from another channel' }));
    const option = await screen.findByRole('option', { name: 'cara' });
    fireEvent.change(screen.getByLabelText('Person from another channel'), { target: { value: option.getAttribute('value') } });
    fireEvent.click(screen.getByRole('button', { name: 'Add selected person' }));
    expect(screen.getByRole('button', { name: 'Add selected person' }).hasAttribute('disabled')).toBe(true);

    view.rerender(controlsView(switched('member')));
    expect(screen.getByRole('button', { name: 'Add from another channel' }).getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByRole('form', { name: 'Add person from another channel' })).toBeNull();
    await refuseLater(pending, { ok: false, failure: 'invalid', message: 'old channel add-selected refusal' });
    expect(screen.queryByText('old channel add-selected refusal')).toBeNull();
  });

  it('resets remove and ignores the old channel response', async () => {
    const pending = deferred<api.InviteResult<null>>();
    vi.mocked(api.removeChannelMember).mockReturnValueOnce(pending.promise).mockResolvedValue({ ok: true, value: null });
    const view = draw(props('member'));
    choose('ada', 'Remove from channel');
    confirmRemoval('ada');
    expect(api.removeChannelMember).toHaveBeenCalledTimes(1);

    view.rerender(controlsView(switched('member')));
    choose('ada', 'Remove from channel');
    confirmRemoval('ada');
    expect(api.removeChannelMember).toHaveBeenCalledTimes(2);
    await refuseLater(pending, { ok: false, failure: 'invalid', message: 'old channel remove refusal' });
    expect(screen.queryByText('old channel remove refusal')).toBeNull();
  });

  it('resets manage and decide and ignores the old channel responses', async () => {
    const pending = deferred<api.InviteResult<null>>();
    vi.mocked(api.putChannelMember).mockReturnValueOnce(pending.promise).mockResolvedValue({ ok: true, value: null });
    const view = draw(props('operator'));
    choose('olivia', 'Make manager');
    choose('avery', 'Stop deciding approvals');
    expect(api.putChannelMember).toHaveBeenCalledTimes(2);

    view.rerender(controlsView(switched('operator')));
    choose('olivia', 'Make manager');
    expect(api.putChannelMember).toHaveBeenCalledTimes(3);
    await refuseLater(pending, { ok: false, failure: 'invalid', message: 'old channel refusal' });
    expect(screen.queryByText('old channel refusal')).toBeNull();
  });
});
