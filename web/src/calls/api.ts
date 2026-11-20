// The signed-in per-person stream and the REST routes plug in here when they
// land. Keeping the transport scoped also lets browser and desktop share the
// controls and exercise them against the generated fixtures.
import type {
  InvitePrivateCallRequestV1, ListPrivateCallsResponseV1, PrivateCallActionRequestV1,
  PrivateCallConflictV1, PrivateCallResponseV1, PrivateCallRingingV1, PrivateCallSessionV1,
  PrivateCallSnapshotV1, PrivateCallStateV1, PrivateCallStopRingingV1,
} from '../schema/generated';

export interface CallScope {
  readonly origin: string;
  readonly account: number;
}

export type CallAction = 'accept' | 'decline' | 'cancel' | 'hangup';

type CallFrame = PrivateCallRingingV1 | PrivateCallStopRingingV1 | PrivateCallStateV1 | PrivateCallSnapshotV1;
export type CallEvent = { readonly scope: CallScope } & (
  | { readonly frame: CallFrame }
  | { readonly accessLost: true }
);

export type CallReply =
  | { readonly kind: 'ok'; readonly value: PrivateCallResponseV1 }
  | { readonly kind: 'conflict'; readonly value: PrivateCallConflictV1 }
  | { readonly kind: 'access-lost' }
  | { readonly kind: 'failed' };

export interface PrivateCallAPI {
  // Each subscription delivers validated frames in stream order, with a
  // snapshot on reconnect. Access loss ends this signed-in binding.
  subscribe(scope: CallScope, receive: (event: CallEvent) => void, signal: AbortSignal): void;
  invite(scope: CallScope, conversation: number, request: InvitePrivateCallRequestV1, signal: AbortSignal): Promise<CallReply>;
  action(scope: CallScope, call: string, action: CallAction, request: PrivateCallActionRequestV1, signal: AbortSignal): Promise<CallReply>;
  history(scope: CallScope, conversation: number, signal: AbortSignal): Promise<ListPrivateCallsResponseV1>;
  session?(scope: CallScope, call: string, signal: AbortSignal): Promise<PrivateCallSessionV1>;
}

export interface PrivateCallMedia {
  // Called only after an explicit Join and a final check of the grant against
  // the current attempt. The adapter observes the signal before starting
  // capture and tears down the connection on abort, including during startup.
  connect(scope: CallScope, grant: PrivateCallSessionV1, signal: AbortSignal): Promise<void>;
}
