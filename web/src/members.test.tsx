import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { MemberControls, Members, type MemberControlsProps } from './Members';
import type { Entry } from './agents/join';
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
        <li>
          <span className="ag-name">bot</span>
          <MemberControls key={`${String(controlProps.channel.id)}:4`} {...controlProps} principalId={4} />
        </li>
      </ul>
    </>
  );
}

function draw(controlProps: MemberControlsProps) {
  return render(controlsView(controlProps));
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.listMembers).mockResolvedValue({ ok: true, value: [] });
});

afterEach(cleanup);

describe('administration controls', () => {
  it('gives an operator add, remove, manager and approval controls, including agent removal', () => {
    draw(props('operator'));
    expect(screen.getByRole('form', { name: 'Add person by name' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Add from another channel' })).toBeDefined();
    for (const name of ['olivia', 'ada', 'avery', 'Guest: Pat · a7k4', 'bot']) {
      expect(screen.getByRole('button', { name: `Remove ${name}` })).toBeDefined();
    }
    expect(screen.getByRole('button', { name: 'Remove ada as manager' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Make avery a manager' })).toBeDefined();
    expect(screen.getByRole('checkbox', { name: 'ada decides approvals' })).toBeDefined();
    expect(screen.queryByRole('button', { name: /bot.*manager/ })).toBeNull();
    expect(screen.queryByRole('checkbox', { name: /bot/ })).toBeNull();
    expect(screen.getAllByText('Manager')).toHaveLength(1);
    expect(screen.getAllByText('Approver')).toHaveLength(2);
  });

  it('gives a manager add and only the removals §9 allows, with the exact explanations', () => {
    draw(props('member'));
    expect(screen.getByRole('form', { name: 'Add person by name' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Remove ada' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Remove olivia' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Remove avery' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Remove bot' })).toBeNull();
    expect(screen.getByText('Operators can only be removed by an operator.')).toBeDefined();
    expect(screen.getByText('Approvers are removed by an operator.')).toBeDefined();
    expect(screen.getByText('Agents are managed by operators.')).toBeDefined();
    expect(screen.getByText('Only operators choose who manages a channel.')).toBeDefined();
    expect(screen.getByText('Only operators choose who decides approvals.')).toBeDefined();
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(screen.queryByRole('button', { name: /manager$/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /as manager/ })).toBeNull();
  });

  it.each([false, true])('gives a plain member or guest no management controls (guest: %s)', (guest) => {
    draw({ ...props('member', guest), channel: { ...channel, manages: false } });
    expect(screen.queryByRole('form', { name: 'Add person by name' })).toBeNull();
    expect(screen.queryByRole('button', { name: /^(Add|Remove|Make)/ })).toBeNull();
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(screen.getByText('Only operators and this channel\'s managers add people.')).toBeDefined();
    expect(screen.getByText('Only operators choose who manages a channel.')).toBeDefined();
    expect(screen.getByText('Only operators choose who decides approvals.')).toBeDefined();
    if (guest) expect(screen.getByText('A guest visit cannot do this; create an account and ask an operator.')).toBeDefined();
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
    fireEvent.click(screen.getByRole('button', { name: 'Remove ada as manager' }));
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledWith('general', 2, { manages: false });
    });
    fireEvent.click(screen.getByRole('checkbox', { name: 'avery decides approvals' }));
    await waitFor(() => {
      expect(api.putChannelMember).toHaveBeenCalledWith('general', 3, { can_decide: false });
    });
    fireEvent.click(screen.getByRole('button', { name: 'Remove bot' }));
    await waitFor(() => {
      expect(api.removeChannelMember).toHaveBeenCalledWith('general', 4);
    });
    expect(refreshMembers).toHaveBeenCalledTimes(3);
  });

  it('shows the server message and refreshes after a refusal of an offered action', async () => {
    vi.mocked(api.putChannelMember).mockResolvedValue({ ok: false, failure: 'invalid', code: 'forbidden', message: 'the owner membership is protected' });
    draw(props('operator'));
    fireEvent.click(screen.getByRole('button', { name: 'Make olivia a manager' }));
    await screen.findByText('the owner membership is protected');
    expect(refreshMembers).toHaveBeenCalledTimes(1);
  });

  it('uses the existing unauthorized path and does not claim a change', async () => {
    vi.mocked(api.removeChannelMember).mockResolvedValue({ ok: false, failure: 'unauthorized' });
    draw(props('member'));
    fireEvent.click(screen.getByRole('button', { name: 'Remove ada' }));
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
    vi.mocked(api.removeChannelMember).mockReturnValue(pending.promise);
    const view = draw(props('member'));
    fireEvent.click(screen.getByRole('button', { name: 'Remove ada' }));
    expect(screen.getByRole('button', { name: 'Remove ada' }).hasAttribute('disabled')).toBe(true);

    view.rerender(controlsView(switched('member')));
    expect(screen.getByRole('button', { name: 'Remove ada' }).hasAttribute('disabled')).toBe(false);
    await refuseLater(pending, { ok: false, failure: 'invalid', message: 'old channel remove refusal' });
    expect(screen.queryByText('old channel remove refusal')).toBeNull();
  });

  it('resets manage and ignores the old channel response', async () => {
    const pending = deferred<api.InviteResult<null>>();
    vi.mocked(api.putChannelMember).mockReturnValue(pending.promise);
    const view = draw(props('operator'));
    fireEvent.click(screen.getByRole('button', { name: 'Make olivia a manager' }));
    expect(screen.getByRole('button', { name: 'Make olivia a manager' }).hasAttribute('disabled')).toBe(true);

    view.rerender(controlsView(switched('operator')));
    expect(screen.getByRole('button', { name: 'Make olivia a manager' }).hasAttribute('disabled')).toBe(false);
    await refuseLater(pending, { ok: false, failure: 'invalid', message: 'old channel manage refusal' });
    expect(screen.queryByText('old channel manage refusal')).toBeNull();
  });

  it('resets decide and ignores the old channel response', async () => {
    const pending = deferred<api.InviteResult<null>>();
    vi.mocked(api.putChannelMember).mockReturnValue(pending.promise);
    const view = draw(props('operator'));
    fireEvent.click(screen.getByRole('checkbox', { name: 'avery decides approvals' }));
    expect(screen.getByRole('checkbox', { name: 'avery decides approvals' }).hasAttribute('disabled')).toBe(true);

    view.rerender(controlsView(switched('operator')));
    expect(screen.getByRole('checkbox', { name: 'avery decides approvals' }).hasAttribute('disabled')).toBe(false);
    await refuseLater(pending, { ok: false, failure: 'invalid', message: 'old channel decide refusal' });
    expect(screen.queryByText('old channel decide refusal')).toBeNull();
  });
});
