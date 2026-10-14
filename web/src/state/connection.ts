// The status line's connection state. Wording follows the TUI's. Until the
// live feed exists (#281) it follows the REST calls: reaching the server is
// Connected, a call that gets no answer is Reconnecting.

export type ConnectionState = 'connecting' | 'connected' | 'reconnecting' | 'signedOut';

export type ConnectionAction =
  | { readonly type: 'started' }
  | { readonly type: 'reached' }
  | { readonly type: 'lost' }
  | { readonly type: 'closed' };

export function connectionReducer(state: ConnectionState, action: ConnectionAction): ConnectionState {
  switch (action.type) {
    case 'started':
      return 'connecting';
    case 'reached':
      return state === 'signedOut' ? state : 'connected';
    case 'lost':
      return state === 'signedOut' ? state : 'reconnecting';
    case 'closed':
      return 'signedOut';
  }
}

const labels: Record<ConnectionState, string> = {
  connecting: 'Connecting',
  connected: 'Connected',
  reconnecting: 'Reconnecting',
  signedOut: 'Signed out',
};

export function connectionLabel(state: ConnectionState): string {
  return labels[state];
}
