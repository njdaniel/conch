// The 'share' media hook's body (issue #1618), apart from app.ts so a test can
// drive the real one without Electron. Input: the reason a switch, sign-out or
// quit gives, and two handles on main's state. Output: the companion is ended
// with the identity already gone from main. It owns the order of those two
// steps and nothing else.
import type { CompanionEndReason } from '../../web/src/platform/desktop/bridge.ts';

export interface ShareHookDeps {
  // Forgets the signed-in client, so no grant is asked for under the ending
  // identity.
  readonly dropClient: () => void;
  readonly companion: { end(reason: CompanionEndReason): Promise<boolean> };
}

export function shareTeardown(deps: ShareHookDeps): (arg: { reason: CompanionEndReason }) => Promise<void> {
  return async ({ reason }) => {
    // From here on the identity is ending: no grant is asked for under it.
    // A page's grant request after this end (the click of a switch or a
    // sign-out is still armed) is refused as failed, and one in flight finds
    // the client changed and removes what conchd issued. The next
    // activation's makeFiles sets the client again. This must run before
    // companion.end: #1580 keys the renderer's LiveKit allowance off
    // liveOrigin(), which leans on this order.
    deps.dropClient();
    await deps.companion.end(reason);
  };
}
