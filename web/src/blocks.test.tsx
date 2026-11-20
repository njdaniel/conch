// Personal blocks in the page (issue #1832): the collapsed row and its Show,
// the decision-card and tombstone exceptions, the frame, the alert rule and
// the voice default. No network, no browser, no conchd.
import { act, cleanup, fireEvent, render, renderHook, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { parseEventFrame } from './agents/frames';
import { BlockConfirm, BlocksProvider, BlockToggle, isBlockExempt, isCollapsedFor, useBlockedVoiceMute, type Blocks } from './blocks';
import { Message } from './Conversation';
import type { MessageV2 } from './schema/message';

afterEach(cleanup);

const at = '2026-11-19T12:00:00.000Z';
const blocker = 1;
const blockedId = 9;

function message(over: Partial<MessageV2> = {}): MessageV2 {
  return { schema: 'conch.message.v2', id: 50, channel_id: 7, author_id: blockedId, body: 'something unkind', created_at: at, ...over };
}

function blocksOf(ids: readonly number[], over: Partial<Blocks> = {}): Blocks {
  return {
    blocked: new Set(ids),
    list: ids.map((id) => ({ principal_id: id, name: `p${String(id)}`, kind: 'human' as const, created_at: at })),
    loaded: true,
    block: vi.fn(() => Promise.resolve(true)),
    unblock: vi.fn(() => Promise.resolve(true)),
    signal: vi.fn(),
    ...over,
  };
}

function renderMessage(m: MessageV2, blocks: Blocks) {
  return render(
    <BlocksProvider value={blocks}>
      <ul>
        <Message message={m} people={new Map([[blockedId, { kind: 'human', name: 'marlow' }]])} nets={new Map()} viewer={blocker} channel="general" sent={false} cont={false} />
      </ul>
    </BlocksProvider>,
  );
}

describe('the collapsed row', () => {
  it('a blocked person’s message is one row, Blocked message, and Show reveals it', () => {
    renderMessage(message(), blocksOf([blockedId]));
    expect(screen.getByText('Blocked message')).toBeTruthy();
    expect(screen.queryByText('something unkind')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Show' }));
    expect(screen.getByText('something unkind')).toBeTruthy();
    expect(screen.queryByText('Blocked message')).toBeNull();
  });

  it('Show is for this view only: another rendering collapses it again', () => {
    const first = renderMessage(message(), blocksOf([blockedId]));
    fireEvent.click(screen.getByRole('button', { name: 'Show' }));
    first.unmount();
    renderMessage(message(), blocksOf([blockedId]));
    expect(screen.getByText('Blocked message')).toBeTruthy();
  });

  it('nobody blocked: the message shows as it is', () => {
    renderMessage(message(), blocksOf([]));
    expect(screen.getByText('something unkind')).toBeTruthy();
    expect(screen.queryByText('Blocked message')).toBeNull();
  });

  it.each([
    ['an approval card', { payload: { schema: 'conch.approval.v1', data: {} } }],
    ['an admin-action card', { payload: { schema: 'conch.admin_action.v1', data: {} } }],
    ['a tombstone', { removed: true, body: '' }],
  ])('%s from a blocked person is never collapsed', (_name, over) => {
    const m = message(over);
    expect(isBlockExempt(m)).toBe(true);
    expect(isCollapsedFor(new Set([blockedId]), m)).toBe(false);
  });

  it('another payload follows the block', () => {
    expect(isCollapsedFor(new Set([blockedId]), message({ payload: { schema: 'conch.chart.v1', data: {} } }))).toBe(true);
  });
});

describe('the controls', () => {
  it('Block… asks first, saying what it does and does not do', async () => {
    const blocks = blocksOf([]);
    render(<BlocksProvider value={blocks}><BlockToggle id={blockedId} name="marlow" viewer={blocker} /></BlocksProvider>);
    fireEvent.click(screen.getByRole('button', { name: 'Block…' }));
    const question = screen.getByRole('group');
    expect(question.textContent).toContain('Block marlow?');
    expect(question.textContent).toContain('does not remove them from anything');
    expect(question.textContent).toContain('they are not told');
    expect(document.activeElement?.textContent).toBe('Cancel');
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Yes, block' }));
      await Promise.resolve();
    });
    expect(blocks.block).toHaveBeenCalledWith(blockedId);
  });

  it('Cancel and Escape send nothing', () => {
    const blocks = blocksOf([]);
    const onClose = vi.fn();
    render(<BlocksProvider value={blocks}><BlockConfirm id={blockedId} name="marlow" onClose={onClose} /></BlocksProvider>);
    fireEvent.keyDown(screen.getByRole('group'), { key: 'Escape' });
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalledTimes(2);
    expect(blocks.block).not.toHaveBeenCalled();
  });

  it('a blocked person’s card offers Unblock; one’s own card and a guest’s page offer nothing', () => {
    const blocks = blocksOf([blockedId]);
    const { rerender } = render(<BlocksProvider value={blocks}><BlockToggle id={blockedId} name="marlow" viewer={blocker} /></BlocksProvider>);
    fireEvent.click(screen.getByRole('button', { name: 'Unblock' }));
    expect(blocks.unblock).toHaveBeenCalledWith(blockedId);
    rerender(<BlocksProvider value={blocks}><BlockToggle id={blocker} name="me" viewer={blocker} /></BlocksProvider>);
    expect(screen.queryByRole('button')).toBeNull();
    rerender(<BlocksProvider value={blocksOf([], { loaded: false })}><BlockToggle id={blockedId} name="marlow" viewer={blocker} /></BlocksProvider>);
    expect(screen.queryByRole('button')).toBeNull();
  });
});

describe('the blocks_changed frame', () => {
  it('is read as a signal with no content', () => {
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.blocks_changed.v1' }))).toEqual({ kind: 'blocksChanged' });
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.blocks_changed.v1', extra: 1 }))).toEqual({ kind: 'blocksChanged' });
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.blocks_changed.v2' }))).toBeNull();
  });
});

describe('voice', () => {
  it('a blocked person in the room is muted on this page by default, once, and unblocking plays them again', () => {
    const setVolume = vi.fn();
    const room = [{ principal: blockedId }, { principal: 4 }];
    const { rerender } = renderHook(({ blocked, volumes }) => { useBlockedVoiceMute(blocked, room, volumes, setVolume); }, {
      initialProps: { blocked: new Set([blockedId]), volumes: new Map<number, number>() },
    });
    expect(setVolume).toHaveBeenCalledTimes(1);
    expect(setVolume).toHaveBeenCalledWith(blockedId, 0);
    // The person unmutes them: the choice is kept.
    rerender({ blocked: new Set([blockedId]), volumes: new Map([[blockedId, 0.5]]) });
    expect(setVolume).toHaveBeenCalledTimes(1);
    // Unblocked while at 0: played again.
    rerender({ blocked: new Set([blockedId]), volumes: new Map([[blockedId, 0]]) });
    rerender({ blocked: new Set(), volumes: new Map([[blockedId, 0]]) });
    expect(setVolume).toHaveBeenLastCalledWith(blockedId, 1);
    // Nobody else's volume was touched.
    expect(setVolume.mock.calls.every(([id]) => id === blockedId)).toBe(true);
  });
});
