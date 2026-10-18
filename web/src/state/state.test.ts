import { describe, expect, it } from 'vitest';
import type { ChannelV0 } from '../schema/channel';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { authReducer, initialAuth, noticeText, type AuthAction, type AuthState } from './auth';
import { channelsReducer, defaultChannel, fallbackChannel, initialChannels, type ChannelsState } from './channels';
import { connectionLabel, connectionReducer, type ConnectionAction, type ConnectionState } from './connection';

const member: WhoAmIResponseV1 = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const operator: WhoAmIResponseV1 = { id: 1, kind: 'human', name: 'nick', role: 'operator' };

describe('authReducer', () => {
  const cases: { name: string; from: AuthState; action: AuthAction; to: AuthState }[] = [
    { name: 'submit checks', from: { phase: 'signedOut', notice: null }, action: { type: 'submit' }, to: { phase: 'checking' } },
    { name: 'submit clears an old notice', from: { phase: 'signedOut', notice: 'rejected' }, action: { type: 'submit' }, to: { phase: 'checking' } },
    { name: 'submit while signed in is ignored', from: { phase: 'signedIn', principal: member }, action: { type: 'submit' }, to: { phase: 'signedIn', principal: member } },
    { name: 'member verified signs in', from: { phase: 'checking' }, action: { type: 'verified', principal: member }, to: { phase: 'signedIn', principal: member } },
    { name: 'operator verified is warned', from: { phase: 'checking' }, action: { type: 'verified', principal: operator }, to: { phase: 'operatorWarning', principal: operator } },
    { name: 'a restored operator is not warned again', from: { phase: 'restoring' }, action: { type: 'verified', principal: operator }, to: { phase: 'signedIn', principal: operator } },
    { name: 'a restored member signs in', from: { phase: 'restoring' }, action: { type: 'verified', principal: member }, to: { phase: 'signedIn', principal: member } },
    { name: 'a restored operator credential kept to last is warned about', from: { phase: 'restoring' }, action: { type: 'held', principal: operator }, to: { phase: 'operatorWarning', principal: operator } },
    { name: 'held outside a restore is ignored', from: { phase: 'signedIn', principal: member }, action: { type: 'held', principal: operator }, to: { phase: 'signedIn', principal: member } },
    { name: 'verified when signed out is ignored', from: { phase: 'signedOut', notice: null }, action: { type: 'verified', principal: member }, to: { phase: 'signedOut', notice: null } },
    { name: '401 at sign-in is rejected', from: { phase: 'checking' }, action: { type: 'failed', failure: 'unauthorized' }, to: { phase: 'signedOut', notice: 'rejected' } },
    { name: 'network failure at sign-in', from: { phase: 'checking' }, action: { type: 'failed', failure: 'network' }, to: { phase: 'signedOut', notice: 'network' } },
    { name: 'bad reply at sign-in', from: { phase: 'checking' }, action: { type: 'failed', failure: 'invalid' }, to: { phase: 'signedOut', notice: 'invalid' } },
    { name: '401 on restore is expired', from: { phase: 'restoring' }, action: { type: 'failed', failure: 'unauthorized' }, to: { phase: 'signedOut', notice: 'expired' } },
    { name: 'network failure on restore', from: { phase: 'restoring' }, action: { type: 'failed', failure: 'network' }, to: { phase: 'signedOut', notice: 'network' } },
    { name: 'failed when signed in is ignored', from: { phase: 'signedIn', principal: member }, action: { type: 'failed', failure: 'network' }, to: { phase: 'signedIn', principal: member } },
    { name: 'continue after the warning', from: { phase: 'operatorWarning', principal: operator }, action: { type: 'continue' }, to: { phase: 'signedIn', principal: operator } },
    { name: 'continue elsewhere is ignored', from: { phase: 'checking' }, action: { type: 'continue' }, to: { phase: 'checking' } },
    { name: 'sign out from the lobby', from: { phase: 'signedIn', principal: member }, action: { type: 'signOut' }, to: { phase: 'signedOut', notice: null } },
    { name: 'sign out from the warning', from: { phase: 'operatorWarning', principal: operator }, action: { type: 'signOut' }, to: { phase: 'signedOut', notice: null } },
    { name: 'sign out the server was not told of', from: { phase: 'signedIn', principal: member }, action: { type: 'signOut', notice: 'untold' }, to: { phase: 'signedOut', notice: 'untold' } },
    { name: 'sign out everywhere', from: { phase: 'signedIn', principal: member }, action: { type: 'signOut', notice: 'everywhere' }, to: { phase: 'signedOut', notice: 'everywhere' } },
    { name: 'signed out in another tab', from: { phase: 'signedIn', principal: member }, action: { type: 'signOut', notice: 'elsewhere' }, to: { phase: 'signedOut', notice: 'elsewhere' } },
    { name: '401 mid-session', from: { phase: 'signedIn', principal: member }, action: { type: 'unauthorized' }, to: { phase: 'signedOut', notice: 'expired' } },
    { name: '401 when already signed out changes nothing', from: { phase: 'signedOut', notice: null }, action: { type: 'unauthorized' }, to: { phase: 'signedOut', notice: null } },
    { name: 'a rename changes the name only', from: { phase: 'signedIn', principal: member }, action: { type: 'renamed', name: 'Ada L' }, to: { phase: 'signedIn', principal: { ...member, name: 'Ada L' } } },
    { name: 'a rename when signed out is ignored', from: { phase: 'signedOut', notice: null }, action: { type: 'renamed', name: 'Ada L' }, to: { phase: 'signedOut', notice: null } },
  ];
  it.each(cases)('$name', ({ from, action, to }) => {
    expect(authReducer(from, action)).toEqual(to);
  });

  it('starts restoring only when a token is stored', () => {
    expect(initialAuth(true)).toEqual({ phase: 'restoring' });
    expect(initialAuth(false)).toEqual({ phase: 'signedOut', notice: null });
  });

  it('words the lines as the issue gives them', () => {
    expect(noticeText('rejected')).toBe('That credential was not accepted');
    expect(noticeText('network')).toBe('Cannot reach the server');
    expect(noticeText('expired')).toBe('Signed out: the credential is no longer valid');
  });
});

describe('connectionReducer', () => {
  const cases: { from: ConnectionState; action: ConnectionAction; to: ConnectionState }[] = [
    { from: 'signedOut', action: { type: 'started' }, to: 'connecting' },
    { from: 'connecting', action: { type: 'reached' }, to: 'connected' },
    { from: 'connecting', action: { type: 'lost' }, to: 'reconnecting' },
    { from: 'connected', action: { type: 'lost' }, to: 'reconnecting' },
    { from: 'reconnecting', action: { type: 'reached' }, to: 'connected' },
    { from: 'connected', action: { type: 'closed' }, to: 'signedOut' },
    { from: 'signedOut', action: { type: 'reached' }, to: 'signedOut' },
    { from: 'signedOut', action: { type: 'lost' }, to: 'signedOut' },
  ];
  it.each(cases)('$from + $action.type = $to', ({ from, action, to }) => {
    expect(connectionReducer(from, action)).toBe(to);
  });

  it('labels each state', () => {
    expect(
      (['connecting', 'connected', 'reconnecting', 'signedOut'] as const).map(connectionLabel),
    ).toEqual(['Connecting', 'Connected', 'Reconnecting', 'Signed out']);
  });
});

describe('channelsReducer', () => {
  const ch = (id: number, name: string): ChannelV0 => ({ id, name, created_at: '2026-10-14T00:00:00Z' });
  const list = [ch(1, 'general'), ch(2, 'ops'), ch(3, 'dev')];
  const loaded = (selected: string | null): ChannelsState => ({ channels: list, loaded: true, selected });

  it('opens the first channel when nothing else is known', () => {
    expect(channelsReducer(initialChannels, { type: 'loaded', channels: list, preferred: null, remembered: null })).toEqual(loaded('general'));
  });
  it('prefers the hash, then the channel last open in this tab', () => {
    expect(channelsReducer(initialChannels, { type: 'loaded', channels: list, preferred: 'ops', remembered: 'dev' }).selected).toBe('ops');
    expect(channelsReducer(initialChannels, { type: 'loaded', channels: list, preferred: null, remembered: 'dev' }).selected).toBe('dev');
  });
  it('ignores a name that is not in the list', () => {
    expect(channelsReducer(initialChannels, { type: 'loaded', channels: list, preferred: 'gone', remembered: 'also-gone' }).selected).toBe('general');
  });
  it('has no selection and is loaded when there are no channels', () => {
    expect(channelsReducer(initialChannels, { type: 'loaded', channels: [], preferred: 'ops', remembered: null })).toEqual({
      channels: [],
      loaded: true,
      selected: null,
    });
  });
  it('keeps the open channel across a reload of the list', () => {
    expect(channelsReducer(loaded('dev'), { type: 'loaded', channels: list, preferred: null, remembered: null }).selected).toBe('dev');
  });
  it('selects only a channel it has', () => {
    expect(channelsReducer(loaded('general'), { type: 'select', name: 'dev' }).selected).toBe('dev');
    expect(channelsReducer(loaded('general'), { type: 'select', name: 'nope' }).selected).toBe('general');
  });
  it('resets', () => {
    expect(channelsReducer(loaded('ops'), { type: 'reset' })).toEqual(initialChannels);
  });

  // Issue #556: #general takes the place of "the first channel", after the
  // person's own choices. alpha sorts before general in these lists.
  const alphaFirst = [ch(1, 'alpha'), ch(2, 'general'), ch(3, 'ops')];
  const noGeneral = [ch(1, 'alpha'), ch(3, 'ops')];
  it.each<{
    name: string;
    channels: ChannelV0[];
    current: string | null;
    preferred: string | null;
    remembered: string | null;
    want: string | null;
  }>([
    { name: 'opens #general when nothing else is known', channels: alphaFirst, current: null, preferred: null, remembered: null, want: 'general' },
    { name: 'opens #general when every known name is gone', channels: alphaFirst, current: 'gone', preferred: 'gone', remembered: 'gone', want: 'general' },
    { name: "lets a link's channel win over #general", channels: alphaFirst, current: null, preferred: 'alpha', remembered: null, want: 'alpha' },
    { name: 'lets the open channel win over #general', channels: alphaFirst, current: 'ops', preferred: null, remembered: null, want: 'ops' },
    { name: 'lets the remembered channel win over #general', channels: alphaFirst, current: null, preferred: null, remembered: 'alpha', want: 'alpha' },
    { name: 'opens the first channel without #general', channels: noGeneral, current: null, preferred: null, remembered: null, want: 'alpha' },
    { name: 'opens nothing without channels', channels: [], current: null, preferred: null, remembered: null, want: null },
  ])('on load $name', ({ channels, current, preferred, remembered, want }) => {
    const state: ChannelsState = { channels, loaded: true, selected: current };
    expect(channelsReducer(state, { type: 'loaded', channels, preferred, remembered }).selected).toBe(want);
  });

  it.each<{ name: string; channels: ChannelV0[]; selected: string | null; lost: string; want: string | null; left: string[] }>([
    { name: 'moves to #general when the open channel is lost', channels: alphaFirst, selected: 'ops', lost: 'ops', want: 'general', left: ['alpha', 'general'] },
    { name: 'moves to the first channel when it is lost without #general', channels: noGeneral, selected: 'ops', lost: 'ops', want: 'alpha', left: ['alpha'] },
    { name: 'moves to the first channel when #general itself is lost', channels: alphaFirst, selected: 'general', lost: 'general', want: 'alpha', left: ['alpha', 'ops'] },
    { name: 'keeps the open channel when another is lost', channels: alphaFirst, selected: 'ops', lost: 'alpha', want: 'ops', left: ['general', 'ops'] },
    { name: 'has no selection when the last channel is lost', channels: [ch(1, 'alpha')], selected: 'alpha', lost: 'alpha', want: null, left: [] },
  ])('$name', ({ channels, selected, lost, want, left }) => {
    const next = channelsReducer({ channels, loaded: true, selected }, { type: 'lost', name: lost });
    expect(next.selected).toBe(want);
    expect(next.channels.map((c) => c.name)).toEqual(left);
    expect(next.lost).toBe(lost);
  });
});

describe('fallbackChannel', () => {
  it.each<{ names: string[]; want: string | undefined }>([
    { names: ['alpha', 'general'], want: 'general' },
    { names: ['general', 'alpha'], want: 'general' },
    { names: ['red', 'blue'], want: 'red' },
    { names: [], want: undefined },
  ])('picks $want from $names', ({ names, want }) => {
    expect(fallbackChannel(names)).toBe(want);
  });
  it('names #general', () => {
    expect(defaultChannel).toBe('general');
  });
});
