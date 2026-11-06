import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { ModerationConfirm, ModerationScope, moderationQuestion } from './Moderation';
import { Message } from './Conversation';
import { Members, type MemberControlsProps } from './Members';
import { parseEventFrame } from './agents/frames';
import { chatFor, chatReducer, initialChat, type ChatAction } from './state/messages';
import type { MessageV2 } from './schema/message';
import type { WhoAmIResponseV1 } from './schema/principal';

vi.mock('./api', async (original) => ({ ...await original<typeof import('./api')>(), moderate: vi.fn(), listChannelBans: vi.fn(), removeChannelMember: vi.fn() }));
afterEach(() => { cleanup(); vi.clearAllMocks(); });
const at = '2026-11-06T10:00:00Z';
const original: MessageV2 = { schema: 'conch.message.v2', id: 3, channel_id: 1, author_id: 2, created_at: at, body: 'secret text', payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [9] } }, edited_at: at };
const tombstone: MessageV2 = { schema: 'conch.message.v2', id: 3, channel_id: 1, author_id: 2, created_at: at, body: 'This message was removed by a moderator.', removed: true };
const who: WhoAmIResponseV1 = { id: 1, kind: 'human', name: 'manager', role: 'member', permissions: [] };
const member = { principal_id: 1, kind: 'human' as const, name: 'manager', created_at: at, role: 'member' as const, manages: true, can_decide: false };
const props: MemberControlsProps = { viewer: { ...who, guest: false }, channel: { id: 1, name: 'ops', created_at: at, manages: true }, channels: [], members: [member], refreshMembers: vi.fn(), onUnauthorized: vi.fn() };

it('voice confirms intent, cancels by default, and a 503 offers explicit retry without success', async () => {
  vi.mocked(api.moderate).mockResolvedValueOnce({ ok: false, failure: 'invalid', code: 'voice_unavailable', message: 'retry' }).mockResolvedValueOnce({ ok: true, value: null });
  const close = vi.fn();
  render(<ModerationConfirm op="kickVoice" channel="ops" id={2} name="Ada" onClose={close} />);
  expect(api.moderate).not.toHaveBeenCalled();
  expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
  expect(screen.getByText(/They can rejoin; chat access is unchanged/)).toBeDefined();
  fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
  await screen.findByText(/Voice removal was incomplete/);
  expect(screen.queryByText('Removed or already absent/removed.')).toBeNull();
  expect(api.moderate).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await screen.findByText('Removed or already absent/removed.');
  expect(api.moderate).toHaveBeenNthCalledWith(2, 'ops', 'kickVoice', 2);
  fireEvent.keyDown(screen.getByRole('group'), { key: 'Escape' });
  expect(close).toHaveBeenCalledOnce();
});

it('shows the actual server refusal and does not repeat a write automatically', async () => {
  vi.mocked(api.moderate).mockResolvedValue({ ok: false, failure: 'invalid', code: 'forbidden', message: 'a manager may lift only their own ban' });
  render(<ModerationConfirm op="unbanMember" channel="ops" id={2} name="Ada" onClose={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
  await screen.findByText('a manager may lift only their own ban');
  expect(api.moderate).toHaveBeenCalledOnce();
  expect(screen.getByText(/Unban does not re-add/)).toBeDefined();
});

it('ban list is an explicit read, offers only own-manager unbans and keeps idempotent feedback', async () => {
  vi.mocked(api.listChannelBans).mockResolvedValue({ ok: true, value: { bans: [{ principal_id: 2, name: 'Ada', created_at: at, created_by: 1 }, { principal_id: 3, name: 'Other', created_at: at, created_by: 4 }] } });
  vi.mocked(api.moderate).mockResolvedValue({ ok: true, value: null });
  render(<Members {...props} people={[]} />);
  expect(api.listChannelBans).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: 'Channel bans' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Unban Ada' }));
  expect(screen.queryByRole('button', { name: 'Unban Other' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
  await screen.findByText('Ban lifted or already absent. Unban does not re-add them.');
});

it.each([
  { role: 'member' as const, permissions: [], manages: false, guest: false, visible: false },
  { role: 'operator' as const, permissions: [], manages: false, guest: false, visible: false },
  { role: 'member' as const, permissions: [], manages: true, guest: true, visible: false },
  { role: 'member' as const, permissions: [], manages: true, guest: false, visible: true },
  { role: 'operator' as const, permissions: ['manage_channels' as const], manages: false, guest: false, visible: true },
])('message moderation uses effective permission and channel standing: %j', ({ role, permissions, manages, guest, visible }) => {
  render(<ModerationScope who={{ ...who, role, permissions }} members={[{ ...member, manages }]} guest={guest}><Message message={{ ...original, payload: undefined }} viewer={1} channel="ops" people={new Map()} nets={new Map()} sent={false} cont={false} /></ModerationScope>);
  const button = screen.queryByRole('button', { name: /More actions/ });
  expect(button !== null).toBe(visible);
  if (button !== null) {
    fireEvent.click(button);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Remove message' }));
    expect(api.moderate).not.toHaveBeenCalled();
    expect(screen.getByText(/copies already received cannot be recalled/)).toBeDefined();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
    fireEvent.keyDown(screen.getByRole('button', { name: 'Cancel' }), { key: 'Escape' });
    expect(document.activeElement).toBe(button);
    expect(api.moderate).not.toHaveBeenCalled();
  }
});

it('membership and ban controls disappear for a restricted operator or guest without standing', () => {
  for (const viewer of [{ ...who, role: 'operator' as const, permissions: [], guest: false }, { ...who, guest: true }]) {
    const view = render(<Members {...props} viewer={viewer} channel={{ ...props.channel, manages: false }} people={[]} />);
    expect(screen.queryByRole('button', { name: 'Channel bans' })).toBeNull();
    view.unmount();
  }
});

it('ban confirmation explains principal identity limits', () => {
  expect(moderationQuestion('banMember', 'Ada', 'ops')).toContain('A new account or guest visit is not identified by this ban.');
});

describe('safe tombstones', () => {
  it('replaces by id, removes payload, and defeats later history, edits and duplicate original frames', () => {
    let state = chatReducer(initialChat, { type: 'historyLoaded', channel: 'ops', messages: [original], more: false });
    state = chatReducer(state, { type: 'removed', channel: 'ops', message: tombstone });
    const late: ChatAction[] = [
      { type: 'received', channel: 'ops', message: original },
      { type: 'edited', channel: 'ops', message: { ...original, edited_at: '2026-11-06T11:00:00Z' } },
      { type: 'historyLoaded', channel: 'ops', messages: [{ ...original, id: 9, body: 'newer window' }], more: false },
      { type: 'olderLoaded', channel: 'ops', messages: [original], more: false },
      { type: 'historyLoaded', channel: 'ops', messages: [original], more: false },
    ];
    for (const action of late) state = chatReducer(state, action);
    expect([...chatFor(state, 'ops').byId.values()]).toEqual([tombstone]);
    expect(JSON.stringify([...chatFor(state, 'ops').byId.values()])).not.toContain('secret');
  });
  it('recognizes the generated events frame as an update and rejects malformed ones', () => {
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.message_removed.v1', message: tombstone }))).toEqual({ kind: 'messageRemoved', message: tombstone });
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.message_removed.v1', message: null }))).toBeNull();
  });
  it('a removed message never renders an editor, attachment, reaction or removal action', async () => {
    render(<ModerationScope who={who} members={[member]} guest={false}><Message message={tombstone} viewer={1} channel="ops" people={new Map()} nets={new Map()} sent={false} cont={false} editor={<textarea defaultValue="secret" />} actions={[{ key: 'edit', label: 'Edit', onSelect: vi.fn() }]} /></ModerationScope>);
    await act(() => Promise.resolve());
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.queryByRole('button')).toBeNull();
    expect(within(screen.getByRole('listitem')).getByText(tombstone.body)).toBeDefined();
  });
});

it('voice retry survives a participant disappearing before the 503 response', async () => {
  const { Roster } = await import('./voice/VoicePanel');
  let resolve: (result: api.InviteResult<null>) => void = () => undefined;
  vi.mocked(api.moderate).mockReturnValueOnce(new Promise((done) => { resolve = done; }));
  const person = { principal: 2, transmitting: false, sharing: false, sharingAudio: false, camera: false, joinedAt: at };
  const roster = (present: boolean) => <ModerationScope who={who} members={[member]} guest={false}><Roster channel="ops" roster={present ? [person] : []} people={new Map([[2, { kind: 'human', name: 'Ada' }]])} viewer={1} elsewhere={false} speaking={new Set()} volumes={new Map()} /></ModerationScope>;
  const view = render(roster(true));
  fireEvent.click(screen.getByRole('button', { name: 'Remove Ada from voice' }));
  fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
  view.rerender(roster(false));
  await act(async () => { resolve({ ok: false, failure: 'invalid', code: 'voice_unavailable' }); await Promise.resolve(); });
  expect(screen.getByRole('button', { name: 'Retry' })).toBeDefined();
  expect(screen.getByText(/Voice removal was incomplete/)).toBeDefined();
});

it('a delayed search response cannot restore the original after a live tombstone', async () => {
  const { SearchPanel } = await import('./SearchPanel');
  let receive: (message: MessageV2) => void = () => undefined;
  const watch = vi.spyOn(api, 'watchRemovals').mockImplementation((callback) => { receive = callback; return () => undefined; });
  let resolve: (result: Awaited<ReturnType<typeof api.searchMessages>>) => void = () => undefined;
  const search = vi.spyOn(api, 'searchMessages').mockReturnValue(new Promise((done) => { resolve = done; }));
  render(<SearchPanel channelId={1} channel="ops" live="open" people={new Map()} nets={new Map()} viewer={1} onClose={vi.fn()} onMissing={vi.fn()} />);
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'secret' } });
  fireEvent.click(screen.getByRole('button', { name: 'Search' }));
  await act(async () => {
    receive(tombstone);
    resolve({ ok: true, value: { messages: [original] } });
    await Promise.resolve();
  });
  expect(await screen.findByText(tombstone.body)).toBeDefined();
  expect(screen.queryByText('secret text')).toBeNull();
  expect(screen.queryByRole('img')).toBeNull();
  watch.mockRestore(); search.mockRestore();
});
