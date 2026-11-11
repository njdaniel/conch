// The channel list and which channel is open.
import type { ChannelV0, ChannelVisibility } from '../schema/channel';

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
  | { readonly type: 'topic'; readonly id: number; readonly topic: string | undefined }
  // A channel's visibility and default mark (issue #1183), by its id: the
  // operator's own answer, or the events socket's conch.channel.v1. What the
  // server says replaces what is held; a field it omits keeps the value held
  // (a reader takes an absent visibility as private, so nothing is guessed).
  //
  // archivedAt (issue #844) is different: the server omits it while a channel is
  // live, so an absent value is live, and the channel comes back out of the
  // Archived group on restore with no reload.
  | {
      readonly type: 'settings';
      readonly id: number;
      readonly visibility: ChannelVisibility | undefined;
      readonly default: boolean | undefined;
      readonly archivedAt?: string | undefined;
    }
  // The caller's manager grant in one channel, from its accepted member list.
  | { readonly type: 'managed'; readonly id: number; readonly manages: boolean }
  // The person has left a channel themselves (issue #991): it goes from the
  // list, and the open one, if it was, gives way to the landing channel. Not a
  // loss: nothing is said of it.
  | { readonly type: 'left'; readonly name: string }
  | { readonly type: 'lost'; readonly name: string }
  // The list as the server now answers it (issue #1027): it replaces the one
  // held, never merges. If the open channel is not in it, it is lost.
  | { readonly type: 'refreshed'; readonly channels: readonly ChannelV0[] }
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
      // The channel already open is not opened again: the address bar echoing
      // a fallback the page has just opened must not clear the line that says
      // why (issue #1027).
      if (action.name === state.selected) return state;
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
    case 'topic': {
      const held = state.channels.find((channel) => channel.id === action.id);
      if (held === undefined || held.topic === action.topic) return state;
      const channels = state.channels.map((channel) => (channel.id === action.id ? { ...channel, topic: action.topic } : channel));
      return { ...state, channels };
    }
    case 'settings': {
      const held = state.channels.find((channel) => channel.id === action.id);
      if (held === undefined) return state;
      const visibility = action.visibility ?? held.visibility;
      const isDefault = action.default ?? held.default;
      if (held.visibility === visibility && held.default === isDefault && held.archived_at === action.archivedAt) return state;
      const next: ChannelV0 = {
        id: held.id,
        name: held.name,
        created_at: held.created_at,
        ...(held.manages === undefined ? {} : { manages: held.manages }),
        ...(visibility === undefined ? {} : { visibility }),
        ...(isDefault === undefined ? {} : { default: isDefault }),
        ...(action.archivedAt === undefined ? {} : { archived_at: action.archivedAt }),
        ...(held.topic === undefined ? {} : { topic: held.topic }),
      };
      return { ...state, channels: state.channels.map((channel) => (channel.id === action.id ? next : channel)) };
    }
    case 'managed': {
      const held = state.channels.find((channel) => channel.id === action.id);
      if (held === undefined || (held.manages ?? false) === action.manages) return state;
      const channels = state.channels.map((channel) => (channel.id === action.id ? { ...channel, manages: action.manages } : channel));
      return { ...state, channels };
    }
    case 'left': {
      const channels = state.channels.filter((c) => c.name !== action.name);
      // The channels_changed refresh may land before the 204 and have called the
      // channel lost already: leaving is not a loss, so that notice goes too.
      const cleared: ChannelsState = state.lost === action.name ? { channels: state.channels, loaded: state.loaded, selected: state.selected } : state;
      if (channels.length === state.channels.length) return cleared;
      return { ...cleared, channels, selected: state.selected === action.name ? fallback(channels) : state.selected };
    }
    case 'lost': {
      const channels = state.channels.filter((c) => c.name !== action.name);
      const selected = state.selected === action.name ? fallback(channels) : state.selected;
      return { ...state, channels, selected, lost: action.name };
    }
    case 'refreshed': {
      const { channels } = action;
      if (state.loaded && JSON.stringify(channels) === JSON.stringify(state.channels) && (state.selected === null) === (channels.length === 0)) return state;
      if (state.selected === null || has(channels, state.selected)) {
        // Nothing open (a person who was in no channel) lands on the default.
        return { ...state, channels, loaded: true, selected: state.selected ?? fallback(channels) };
      }
      return { channels, loaded: true, selected: fallback(channels), lost: state.selected };
    }
    case 'reset':
      return initialChannels;
  }
}
