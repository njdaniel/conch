import { describe, expect, it } from 'vitest';
import { newestSent } from './Conversation';
import type { MessageV2 } from './schema/message';

const message = (id: number, author_id: number): MessageV2 => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id, created_at: '2026-10-26T00:00:00Z', body: 'hi' });

// F26: "Sent" is under the viewer's newest message only, not under each one.
describe('which message says Sent', () => {
  it.each([
    ['several of the viewer’s own, all sent', [message(1, 2), message(2, 2), message(3, 2)], [1, 2, 3], 3],
    ['the newest own message among others’', [message(1, 2), message(2, 2), message(3, 9)], [1, 2], 2],
    ['the newest own message was sent from another device', [message(1, 2), message(2, 2)], [1], null],
    ['none of the viewer’s', [message(1, 9)], [], null],
    ['nothing', [], [], null],
  ] as const)('%s', (_what, shown, sent, want) => {
    expect(newestSent(shown, 2, new Set(sent))).toBe(want);
  });
});
