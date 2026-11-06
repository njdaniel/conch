import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { ReactionBar, ReactionScope } from './Reactions';
import { reactionSummarySchema, type ReactionSummaryV1 } from './schema/reaction';
vi.mock('./api', () => ({ readReactions: vi.fn(), listReactors: vi.fn(), writeReaction: vi.fn(), moderateReaction: vi.fn(), watchReactions: vi.fn(), whoAmI: vi.fn(), listMembers: vi.fn(), channelNotFound: 'channel_not_found' }));
let receive: (event: api.ReactionEvent) => void;
const sum = (count = 1, me = true, at = '2026-11-06T10:00:00Z'): ReactionSummaryV1 => ({ schema: reactionSummarySchema, channel_id: 2, message_id: 4, as_of: at, reactions: count > 0 ? [{ emoji: '👍', count, me }] : [] });
const people = new Map([[7, { name: 'Ada', kind: 'human' as const }]]);
const unauthorized = vi.fn();
const view = (channel = 'general', ids = [4]) => <ReactionScope key={channel} channel={channel} channelId={2} ids={ids} onUnauthorized={unauthorized}><ReactionBar id={4} channel={channel} label="Ada's message" people={people} onUnauthorized={unauthorized} /></ReactionScope>;
const chip = () => screen.getByRole('button', { name: /thumbs up: \d+ reaction/ });
const add = () => screen.getByRole('button', { name: "Add reaction to Ada's message" });
beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(api.watchReactions).mockImplementation((_channel, _id, listener) => { receive = listener; return vi.fn(); });
  vi.mocked(api.readReactions).mockResolvedValue({ ok: true, value: { summaries: [sum()] } });
  vi.mocked(api.writeReaction).mockResolvedValue({ ok: true, value: sum(0) });
  vi.mocked(api.listReactors).mockResolvedValue({ ok: true, value: { reactors: [{ principal_id: 7, emoji: '👍', created_at: sum().as_of }], next_after: 7 } });
  vi.mocked(api.whoAmI).mockResolvedValue({ ok: false, failure: 'network' });
  vi.mocked(api.listMembers).mockResolvedValue({ ok: true, value: [] });
});
afterEach(cleanup);
describe('reaction controls', () => {
  it('names the emoji, shows pressed/loading state and serializes duplicate touches', async () => {
    let resolve: (result: api.ApiResult<ReactionSummaryV1>) => void = () => undefined;
    vi.mocked(api.writeReaction).mockReturnValue(new Promise((done) => { resolve = done; }));
    render(view());
    await waitFor(() => { expect(chip().getAttribute('aria-pressed')).toBe('true'); });
    fireEvent.touchStart(chip()); fireEvent.touchEnd(chip()); fireEvent.click(chip());
    expect(screen.getByText('Saving reaction…')).toBeTruthy();
    fireEvent.click(add());
    expect(api.writeReaction).toHaveBeenCalledTimes(1);
    await act(async () => { resolve({ ok: true, value: sum(0) }); await Promise.resolve(); });
    expect(screen.queryByRole('button', { name: /thumbs up:/ })).toBeNull();
  });
  it('opens the shared picker, Escape restores focus, and choosing adds only the viewer', async () => {
    render(view());
    await waitFor(() => { expect(add().hasAttribute('disabled')).toBe(false); });
    add().focus(); fireEvent.click(add());
    expect(document.activeElement).toBe(screen.getByRole('searchbox'));
    fireEvent.keyDown(screen.getByRole('searchbox'), { key: 'Escape' });
    expect(document.activeElement).toBe(add());
    fireEvent.click(add());
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'red heart' } });
    await screen.findByRole('button', { name: 'red heart' });
    fireEvent.keyDown(screen.getByRole('searchbox'), { key: 'Enter' });
    expect(api.writeReaction).toHaveBeenCalledWith('general', 4, '❤️', true);
  });
  it('keeps the picker opener focused while saving, and blocks another picker', async () => {
    let finish!: (result: api.ApiResult<ReactionSummaryV1>) => void;
    vi.mocked(api.writeReaction).mockReturnValue(new Promise((resolve) => { finish = resolve; }));
    render(view());
    await waitFor(() => { expect(add().hasAttribute('disabled')).toBe(false); });
    fireEvent.click(add());
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'thumbs up' } });
    await screen.findByRole('button', { name: /^thumbs up$/ });
    fireEvent.keyDown(screen.getByRole('searchbox'), { key: 'Enter' });
    expect(document.activeElement).toBe(add());
    expect(add().getAttribute('aria-disabled')).toBe('true');
    expect(add().hasAttribute('disabled')).toBe(false);
    fireEvent.click(add());
    expect(screen.queryByRole('dialog')).toBeNull();
    await act(async () => { finish({ ok: true, value: sum(2) }); await Promise.resolve(); });
    expect(document.activeElement).toBe(add());
    expect(add().getAttribute('aria-disabled')).toBe('false');
  });
  it('replaces optimistic state with a live summary before the write answers', async () => {
    vi.mocked(api.writeReaction).mockReturnValue(new Promise(() => undefined));
    render(view()); await screen.findByRole('button', { name: 'thumbs up: 1 reaction' });
    fireEvent.click(chip());
    act(() => { receive({ kind: 'summary', summary: sum(3, false, '2026-11-06T10:00:01Z') }); });
    expect(chip().getAttribute('aria-label')).toContain('3 reactions');
    act(() => { receive({ kind: 'summary', summary: sum(1, true) }); });
    expect(chip().getAttribute('aria-pressed')).toBe('false');
  });
  it('rolls back failures and retries the same remove intent', async () => {
    vi.mocked(api.writeReaction).mockResolvedValueOnce({ ok: false, failure: 'network' });
    render(view()); await screen.findByRole('button', { name: 'thumbs up: 1 reaction' }); fireEvent.click(chip());
    await screen.findByText(/Reaction could not be saved/);
    expect(chip().getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(screen.getByRole('button', { name: 'Retry reaction' }));
    await waitFor(() => { expect(api.writeReaction).toHaveBeenCalledTimes(2); });
    expect(vi.mocked(api.writeReaction).mock.calls.map((args) => args[3])).toEqual([false, false]);
  });
  it('paginates authorized identities one bounded page at a time, including guest attribution', async () => {
    vi.mocked(api.listReactors).mockResolvedValueOnce({ ok: true, value: { reactors: [{ principal_id: 22, emoji: '👍', created_at: sum().as_of, guest_reactor: { handle: 'guest-abcd', label: 'Visitor', tag: 'abcd' } }], next_after: 22 } });
    render(view()); await screen.findByRole('button', { name: 'Who reacted with thumbs up' });
    fireEvent.click(screen.getByRole('button', { name: 'Who reacted with thumbs up' }));
    await screen.findByText('Guest: Visitor · abcd');
    expect(screen.queryByText('Ada')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Next reactors' }));
    await screen.findByText('Ada');
    expect(screen.queryByText('Guest: Visitor · abcd')).toBeNull();
    expect(api.listReactors).toHaveBeenLastCalledWith('general', 4, '👍', 22, 50);
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Who reacted with thumbs up' }));
  });
  it('shows list loading, failure and retry without losing the rest of chat', async () => {
    vi.mocked(api.listReactors).mockResolvedValueOnce({ ok: false, failure: 'network' });
    render(view()); await screen.findByRole('button', { name: 'Who reacted with thumbs up' });
    fireEvent.click(screen.getByRole('button', { name: 'Who reacted with thumbs up' }));
    expect(screen.getByText('Loading reactors…')).toBeTruthy();
    fireEvent.click(await screen.findByRole('button', { name: 'Retry list' }));
    await screen.findByText('Ada');
    expect(chip()).toBeTruthy();
  });
  it('clears counts and reactor identities on disconnect and rehydrates empty state on reconnect', async () => {
    render(view()); await screen.findByRole('button', { name: 'Who reacted with thumbs up' });
    fireEvent.click(screen.getByRole('button', { name: 'Who reacted with thumbs up' })); await screen.findByText('Ada');
    vi.mocked(api.readReactions).mockResolvedValue({ ok: true, value: { summaries: [] } });
    act(() => { receive({ kind: 'closed' }); });
    expect(screen.queryByRole('dialog')).toBeNull();
    act(() => { receive({ kind: 'open' }); });
    await waitFor(() => { expect(add().hasAttribute('disabled')).toBe(false); });
    expect(screen.queryByRole('button', { name: /thumbs up:/ })).toBeNull();
  });
  it('hides unsupported server controls and preserves schema refusals as errors', async () => {
    vi.mocked(api.readReactions).mockResolvedValueOnce({ ok: false, failure: 'invalid', code: 'reactions_unsupported' });
    const rendered = render(view());
    await waitFor(() => { expect(screen.queryByRole('group')).toBeNull(); });
    vi.mocked(api.readReactions).mockResolvedValue({ ok: false, failure: 'invalid', code: 'message_not_found' });
    rendered.rerender(view('other'));
    await screen.findByRole('button', { name: 'Retry reaction' });
    expect(add()).toBeTruthy();
  });
  it('bounds hydration and drops answers after switching channel', async () => {
    let done: (result: api.ApiResult<import('./schema/reaction').ReactionSummariesResponseV1>) => void = () => undefined;
    vi.mocked(api.readReactions).mockReturnValueOnce(new Promise((resolve) => { done = resolve; }));
    const rendered = render(view('general', Array.from({ length: 201 }, (_, i) => i + 1)));
    expect(vi.mocked(api.readReactions).mock.calls[0]?.[1]).toHaveLength(100);
    vi.mocked(api.readReactions).mockResolvedValue({ ok: true, value: { summaries: [] } });
    rendered.rerender(view('other'));
    await act(async () => { done({ ok: true, value: { summaries: [sum()] } }); await Promise.resolve(); });
    await waitFor(() => { expect(add().hasAttribute('disabled')).toBe(false); });
    expect(screen.queryByRole('button', { name: /thumbs up:/ })).toBeNull();
  });
});

it('only offers moderation to a server-authorized manager and requires confirmation', async () => {
  vi.mocked(api.whoAmI).mockResolvedValue({ ok: true, value: { id: 7, kind: 'human', name: 'Ada', role: 'member' } });
  vi.mocked(api.listMembers).mockResolvedValue({ ok: true, value: [{ principal_id: 7, kind: 'human', name: 'Ada', manages: true, created_at: sum().as_of }] });
  vi.mocked(api.moderateReaction).mockResolvedValue({ ok: true, value: sum(0) });
  render(view()); await screen.findByRole('button', { name: 'Who reacted with thumbs up' });
  fireEvent.click(screen.getByRole('button', { name: 'Who reacted with thumbs up' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Remove all their reactions' }));
  expect(api.moderateReaction).not.toHaveBeenCalled();
  expect(screen.getByText(/the person may react again/)).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Confirm removal' }));
  await waitFor(() => { expect(api.moderateReaction).toHaveBeenCalledWith('general', 4, 7, undefined); });
  await waitFor(() => { expect(screen.queryByRole('button', { name: /thumbs up:/ })).toBeNull(); });
});

it('does not merge a write response arriving after disconnect', async () => {
  let finish: (result: api.ApiResult<ReactionSummaryV1>) => void = () => undefined;
  vi.mocked(api.writeReaction).mockReturnValue(new Promise((done) => { finish = done; }));
  render(view()); await screen.findByRole('button', { name: 'thumbs up: 1 reaction' }); fireEvent.click(chip());
  act(() => { receive({ kind: 'closed' }); });
  await act(async () => { finish({ ok: true, value: sum(5) }); await Promise.resolve(); });
  expect(screen.queryByRole('button', { name: /thumbs up:/ })).toBeNull();
  expect(add().hasAttribute('disabled')).toBe(true);
});

it('keeps rate-limited retries disabled for the server wait', async () => {
  render(view()); await screen.findByRole('button', { name: 'thumbs up: 1 reaction' });
  vi.useFakeTimers();
  try {
    vi.mocked(api.writeReaction).mockResolvedValueOnce({ ok: false, failure: 'invalid', code: 'rate_limited', retryAfter: 8 });
    await act(async () => { fireEvent.click(chip()); await Promise.resolve(); });
    expect(screen.getByText(/Try again after 8 seconds/)).toBeTruthy();
    const retry = screen.getByRole('button', { name: 'Retry reaction' });
    expect(retry.hasAttribute('disabled')).toBe(true);
    act(() => { vi.advanceTimersByTime(8000); });
    expect(retry.hasAttribute('disabled')).toBe(false);
  } finally { vi.useRealTimers(); }
});

it('hydrates every history page in bounded batches', async () => {
  vi.mocked(api.readReactions).mockResolvedValue({ ok: true, value: { summaries: [] } });
  render(view('general', Array.from({ length: 201 }, (_, i) => i + 1)));
  await waitFor(() => { expect(api.readReactions).toHaveBeenCalledTimes(3); });
  expect(vi.mocked(api.readReactions).mock.calls.map((call) => call[1].length)).toEqual([100, 100, 1]);
  expect(vi.mocked(api.readReactions).mock.calls.flatMap((call) => call[1])).toEqual(Array.from({ length: 201 }, (_, i) => i + 1));
});

it('confirms frames after an unversioned empty batch before showing counts again', async () => {
  vi.mocked(api.readReactions).mockResolvedValue({ ok: true, value: { summaries: [] } });
  render(view()); await waitFor(() => { expect(add().hasAttribute('disabled')).toBe(false); });
  act(() => { receive({ kind: 'summary', summary: sum(3) }); });
  await waitFor(() => { expect(api.readReactions).toHaveBeenCalledTimes(2); });
  expect(screen.queryByRole('button', { name: /thumbs up:/ })).toBeNull();
  vi.mocked(api.readReactions).mockResolvedValue({ ok: true, value: { summaries: [sum(2, true, '2026-11-06T10:00:05Z')] } });
  act(() => { receive({ kind: 'summary', summary: sum(2, true, '2026-11-06T10:00:04Z') }); });
  await screen.findByRole('button', { name: 'thumbs up: 2 reactions' });
});
