// The channel list and which channel is open.
import type { ChannelV0 } from '../schema/channel';

export interface ChannelsState {
  readonly channels: readonly ChannelV0[];
  readonly loaded: boolean;
  // The open channel's name, or null when there is none.
  readonly selected: string | null;
  // A channel the server has closed on this tab for good (1008) while the
  // credential still works: it is gone from the list and this says which.
  readonly lost?: string;
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
  | { readonly type: 'lost'; readonly name: string }
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
      return has(state.channels, action.name)
        ? { channels: state.channels, loaded: state.loaded, selected: action.name }
        : state;
    case 'lost': {
      const channels = state.channels.filter((c) => c.name !== action.name);
      const selected = state.selected === action.name ? (channels[0]?.name ?? null) : state.selected;
      return { ...state, channels, selected, lost: action.name };
    }
    case 'reset':
      return initialChannels;
  }
}
