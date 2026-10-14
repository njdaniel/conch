// The channel list and which channel is open.
import type { ChannelV0 } from '../schema/channel';

export interface ChannelsState {
  readonly channels: readonly ChannelV0[];
  readonly loaded: boolean;
  // The open channel's name, or null when there is none.
  readonly selected: string | null;
}

export type ChannelsAction =
  // preferred is the channel the URL hash names; remembered is the one last
  // open in this tab. Either may be null or no longer exist.
  | {
      readonly type: 'loaded';
      readonly channels: readonly ChannelV0[];
      readonly preferred: string | null;
      readonly remembered: string | null;
    }
  | { readonly type: 'select'; readonly name: string }
  | { readonly type: 'reset' };

export const initialChannels: ChannelsState = { channels: [], loaded: false, selected: null };

function has(channels: readonly ChannelV0[], name: string | null): name is string {
  return name !== null && channels.some((c) => c.name === name);
}

export function channelsReducer(state: ChannelsState, action: ChannelsAction): ChannelsState {
  switch (action.type) {
    case 'loaded': {
      const { channels, preferred, remembered } = action;
      const candidates = [preferred, state.selected, remembered];
      const selected = candidates.find((name) => has(channels, name)) ?? channels[0]?.name ?? null;
      return { channels, loaded: true, selected };
    }
    case 'select':
      return has(state.channels, action.name) ? { ...state, selected: action.name } : state;
    case 'reset':
      return initialChannels;
  }
}
