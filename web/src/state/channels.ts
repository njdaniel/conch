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
  // A channel the person has just made (issue #695): the server's answer.
  | { readonly type: 'created'; readonly channel: ChannelV0 }
  // A channel renamed (issue #728), by its id: the operator's own answer, the
  // events socket's conch.channel.v1, or the list read again after a
  // channel_not_found. It stays where it is in the list, and stays open if it
  // was.
  | { readonly type: 'renamed'; readonly id: number; readonly name: string }
  | { readonly type: 'lost'; readonly name: string }
  | { readonly type: 'reset' };

export const initialChannels: ChannelsState = { channels: [], loaded: false, selected: null };

// The channel a person lands in when nothing they chose names another (issue
// #556): #general when they can see it, else the first channel, as before.
export const defaultChannel = 'general';

// fallbackChannel picks the landing channel from the names a person can see.
export function fallbackChannel(names: readonly string[]): string | undefined {
  return names.includes(defaultChannel) ? defaultChannel : names[0];
}

function fallback(channels: readonly ChannelV0[]): string | null {
  return fallbackChannel(channels.map((c) => c.name)) ?? null;
}

function has(channels: readonly ChannelV0[], name: string | null): name is string {
  return name !== null && channels.some((c) => c.name === name);
}

export function channelsReducer(state: ChannelsState, action: ChannelsAction): ChannelsState {
  switch (action.type) {
    case 'loaded': {
      const { channels, preferred, remembered } = action;
      // The link's channel, the open one and the remembered one are the
      // person's own choices, so each wins over the default.
      const candidates = [preferred, state.selected, remembered];
      const selected = candidates.find((name) => has(channels, name)) ?? fallback(channels);
      return { channels, loaded: true, selected };
    }
    case 'select':
      return has(state.channels, action.name)
        ? { channels: state.channels, loaded: state.loaded, selected: action.name }
        : state;
    case 'created': {
      // In id order, as the server lists them, and opened: the person made it
      // to use it. A channel already listed (a reload got there first) is not
      // added twice.
      const { channel } = action;
      const channels = state.channels.some((c) => c.id === channel.id)
        ? state.channels
        : [...state.channels, channel].sort((a, b) => a.id - b.id);
      return { ...state, channels, selected: channel.name };
    }
    case 'renamed': {
      const was = state.channels.find((c) => c.id === action.id);
      if (was === undefined || was.name === action.name) return state;
      const channels = state.channels.map((c) => (c.id === action.id ? { ...c, name: action.name } : c));
      const selected = state.selected === was.name ? action.name : state.selected;
      return { ...state, channels, selected };
    }
    case 'lost': {
      const channels = state.channels.filter((c) => c.name !== action.name);
      const selected = state.selected === action.name ? fallback(channels) : state.selected;
      return { ...state, channels, selected, lost: action.name };
    }
    case 'reset':
      return initialChannels;
  }
}
