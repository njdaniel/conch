// The 'share' media hook's teardown order (issue #1618), driven through the
// real hook registry (TeardownHooks) and the real shareTeardown that app.ts
// registers. A stub companion records what main's client was when end() ran.
import { describe, expect, it } from 'vitest';

import { shareTeardown } from '../../main/share-hook.ts';
import { TeardownHooks } from '../../main/profiles/hooks.ts';
import type { CompanionEndReason } from '../../../web/src/platform/desktop/bridge.ts';

interface Seen {
  readonly client: object | null;
  readonly reason: CompanionEndReason;
}

function setup() {
  const state: { client: object | null } = { client: {} };
  const seen: Seen[] = [];
  const order: string[] = [];
  const hooks = new TeardownHooks();
  hooks.media(
    'share',
    shareTeardown({
      dropClient: () => {
        order.push('dropClient');
        state.client = null;
      },
      companion: {
        end: (reason) => {
          order.push('end');
          seen.push({ client: state.client, reason });
          return Promise.resolve(true);
        },
      },
    }),
  );
  return { state, seen, order, hooks };
}

describe('the share media hook', () => {
  it.each(['switch', 'sign-out', 'quit'] as const)('drops the client before the companion is ended (%s)', async (reason) => {
    const { hooks, seen, order, state } = setup();
    for (const h of hooks.list('media')) await h.run({ reason });
    // Ordered, and the client was already null when end() ran.
    expect(order).toEqual(['dropClient', 'end']);
    expect(seen).toEqual([{ client: null, reason }]);
    expect(state.client).toBeNull();
  });
});
