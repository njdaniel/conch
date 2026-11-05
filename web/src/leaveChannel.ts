import * as api from './api';

// Leave refusals use the page's own words, by the server's code; the server's
// message is never shown.
export function leaveLine(failure: api.ApiFailure, code: string | undefined): string {
  if (failure === 'network') return 'Cannot reach the server. Nothing was changed; try again.';
  switch (code) {
    case 'channel_last_member':
      return 'You are the last member of this private channel. Add someone else before you leave.';
    case api.channelNotFound:
      return 'That channel is gone, or you can no longer see it.';
    default:
      return 'The server could not take you out of the channel. Try again.';
  }
}
