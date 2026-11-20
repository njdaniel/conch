import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import activeJSON from '../../../pkg/schema/testdata/private-call-state-v1.json';
import acceptedJSON from '../../../pkg/schema/testdata/private-call-response-v1-accepted.json';
import conflictJSON from '../../../pkg/schema/testdata/private-call-conflict-v1.json';
import failedJSON from '../../../pkg/schema/testdata/private-call-v1-failed-join-timeout.json';
import glareJSON from '../../../pkg/schema/testdata/private-call-response-v1-glare.json';
import historyJSON from '../../../pkg/schema/testdata/list-private-calls-response-v1.json';
import idleJSON from '../../../pkg/schema/testdata/private-call-snapshot-v1-idle.json';
import missedJSON from '../../../pkg/schema/testdata/private-call-v1-missed.json';
import outgoingJSON from '../../../pkg/schema/testdata/private-call-response-v1.json';
import ringingJSON from '../../../pkg/schema/testdata/private-call-ringing-v1.json';
import sessionJSON from '../../../pkg/schema/testdata/private-call-session-v1.json';
import snapshotJSON from '../../../pkg/schema/testdata/private-call-snapshot-v1.json';
import stoppedJSON from '../../../pkg/schema/testdata/private-call-stop-ringing-v1.json';
import unavailableJSON from '../../../pkg/schema/testdata/private-call-response-v1-unavailable.json';
import { Lobby } from '../Lobby';
import {
  listPrivateCallsResponseV1, privateCallConflictV1, privateCallResponseV1, privateCallRingingV1,
  privateCallSessionV1, privateCallSnapshotV1, privateCallStateV1, privateCallStopRingingV1, privateCallV1,
} from '../schema/generated';
import type { Validator } from '../schema/validate';
import { initialChannels } from '../state/channels';
import { VoiceClient } from '../voice/connection';
import { channelVoiceBusy, leaveVoice, useVoice } from '../voice/useVoice';
import { type CallEvent, type CallReply, type CallScope, type PrivateCallAPI, type PrivateCallMedia } from './api';
import { DMCallControls, PrivateCallBanner, PrivateCallProvider } from './PrivateCallControls';

function fixture<T>(value: unknown, validate: Validator<T>): T {
  const problems = validate(value);
  if (problems.length > 0) throw new Error(problems.join('\n'));
  return value as T;
}

const active = fixture(activeJSON, privateCallStateV1);
const accepted = fixture(acceptedJSON, privateCallResponseV1);
const conflict = fixture(conflictJSON, privateCallConflictV1);
const failed = fixture(failedJSON, privateCallV1);
const glare = fixture(glareJSON, privateCallResponseV1);
const history = fixture(historyJSON, listPrivateCallsResponseV1);
const idle = fixture(idleJSON, privateCallSnapshotV1);
const missed = fixture(missedJSON, privateCallV1);
const outgoing = fixture(outgoingJSON, privateCallResponseV1);
const ringing = fixture(ringingJSON, privateCallRingingV1);
const session = fixture(sessionJSON, privateCallSessionV1);
const snapshot = fixture(snapshotJSON, privateCallSnapshotV1);
const stopped = fixture(stoppedJSON, privateCallStopRingingV1);
const unavailable = fixture(unavailableJSON, privateCallResponseV1);

const callee = { origin: 'https://conch.example.test', account: 5 };
const caller = { ...callee, account: 3 };
const conversationId = 12;
const ok = (value: Extract<CallReply, { kind: 'ok' }>['value']): CallReply => ({ kind: 'ok', value });
const noVoice = { isBusy: () => false, leave: () => Promise.resolve() };
const validGrant = () => ({ ...session, identity: 'p5', expires_at: new Date(Date.now() + 15_000).toISOString() });

function fakeAPI() {
  const subscribers: ((event: CallEvent) => void)[] = [];
  const api = {
    subscribe: vi.fn<PrivateCallAPI['subscribe']>((_scope, receive) => { subscribers.push(receive); }),
    invite: vi.fn<PrivateCallAPI['invite']>().mockResolvedValue(ok(outgoing)),
    action: vi.fn<PrivateCallAPI['action']>().mockResolvedValue(ok(accepted)),
    history: vi.fn<PrivateCallAPI['history']>().mockResolvedValue({ calls: [] }),
    session: vi.fn<NonNullable<PrivateCallAPI['session']>>().mockImplementation(() => Promise.resolve(validGrant())),
  };
  const emit = (event: CallEvent) => {
    act(() => { for (const receive of subscribers) receive(event); });
  };
  return { api, emit, subscribers };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function controls(api: PrivateCallAPI, scope: CallScope = callee, media?: PrivateCallMedia, channelVoice = noVoice) {
  return (
    <PrivateCallProvider scope={scope} options={{ api, media }} channelVoice={channelVoice}>
      <PrivateCallBanner conversationId={conversationId} />
      <p>DM text</p>
      <DMCallControls conversationId={conversationId} />
    </PrivateCallProvider>
  );
}

const ignoreUnauthorized = () => undefined;

function ChannelVoiceButtons() {
  const voice = useVoice({ signedIn: true, channel: 'general', voiceAllowed: true, reachable: false, onUnauthorized: ignoreUnauthorized });
  return <>
    <button type="button" onClick={voice.join}>Join voice</button>
    <button type="button" onClick={() => { voice.watchOnly(); }}>Watch only</button>
  </>;
}

const button = (name: string) => screen.getByRole('button', { name });
const click = (name: string) => { fireEvent.click(button(name)); };

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('private call controls', () => {
  it('mounts Call for the DM route and an incoming prompt across Lobby navigation', async () => {
    const { api, emit } = fakeAPI();
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('No server'))));
    const lobby = (open?: number) => <Lobby origin={callee.origin} me={callee.account} name="Ada" channels={initialChannels}
      unread={new Map()} connection="connected" conversation={<p>{open === undefined ? 'Channel text' : 'DM text'}</p>}
      panel={null} voice={null} invites={null} signOutProblem={null} onOpen={() => undefined}
      onRetry={() => undefined} onSignOut={() => undefined} onSignOutEverywhere={() => undefined}
      privateCalls={{ api, conversationId: open }} />;
    const view = render(lobby());
    expect(screen.queryByRole('button', { name: 'Call' })).toBeNull();
    emit({ scope: callee, frame: ringing });
    expect(button('Answer')).toBeDefined();
    view.rerender(lobby(conversationId));
    expect(screen.getAllByRole('button', { name: 'Answer' })).toHaveLength(1);
    expect(api.subscribe).toHaveBeenCalledTimes(1);
    emit({ scope: callee, frame: stopped });
    expect(button('Call')).toBeDefined();
    await act(async () => {});
  });

  it('offers, cancels, and shows the same unavailable outcome for a refused invite', async () => {
    const { api } = fakeAPI();
    api.action.mockResolvedValue(ok({ call: { ...outgoing.call, revision: 2, state: 'cancelled', selected_here: false } }));
    render(controls(api, caller));
    click('Call');
    expect(await screen.findByText('Calling…')).toBeDefined();
    click('Cancel');
    expect(await screen.findByText('Call cancelled')).toBeDefined();
    expect(api.action).toHaveBeenCalledWith(caller, outgoing.call.id, 'cancel', expect.objectContaining({ revision: 1 }), expect.any(AbortSignal));
    api.invite.mockResolvedValue(ok({ call: { ...unavailable.call, id: 'another-attempt', created_at: '2026-11-19T10:01:00.000Z' } }));
    click('Call');
    expect(await screen.findByText('Call unavailable')).toBeDefined();
    expect(api.session).not.toHaveBeenCalled();
  });

  it('retries a committed invite whose response was lost without ringing again', async () => {
    const { api } = fakeAPI();
    const inviteSent = vi.fn();
    const sent = new Set<string>();
    api.invite.mockImplementation((_scope, _conversation, { action_id }) => {
      if (sent.has(action_id)) {
        return Promise.resolve(ok({ call: { ...missed, role: 'caller' }, replayed: true }));
      }
      sent.add(action_id);
      inviteSent();
      return Promise.reject(new Error('Lost reply'));
    });
    render(controls(api, caller));
    click('Call');
    await screen.findByRole('alert');
    click('Call');
    expect(await screen.findByText('Call went unanswered')).toBeDefined();
    expect(inviteSent).toHaveBeenCalledTimes(1);
    click('Call');
    await screen.findByRole('alert');
    expect(inviteSent).toHaveBeenCalledTimes(2);
  });

  it('requires a separate Call click after the server retry window expires', async () => {
    const { api } = fakeAPI();
    const now = Date.now();
    const clock = vi.spyOn(Date, 'now').mockReturnValue(now);
    api.invite.mockRejectedValueOnce(new Error('Lost reply'));
    render(controls(api, caller));
    click('Call');
    await screen.findByRole('alert');
    clock.mockReturnValue(now + 60 * 60 * 1000);
    click('Call');
    expect(screen.getByRole('alert').textContent).toContain('The call retry window expired.');
    expect(api.invite).toHaveBeenCalledTimes(1);
    click('Call');
    expect(await screen.findByText('Calling…')).toBeDefined();
    expect(api.invite.mock.calls[0]?.[2].action_id).not.toBe(api.invite.mock.calls[1]?.[2].action_id);
  });

  it('shows glare as incoming and supports decline without requesting audio', async () => {
    const { api } = fakeAPI();
    api.invite.mockResolvedValue(ok(glare));
    api.action.mockResolvedValue(ok({ call: { ...glare.call, revision: 2, state: 'declined' } }));
    render(controls(api, caller));
    click('Call');
    const prompt = await screen.findByRole('region', { name: 'Private call' });
    expect(prompt.textContent).toContain('Incoming private call');
    click('Decline');
    expect(await screen.findByText('Call declined')).toBeDefined();
    expect(api.action).toHaveBeenCalledWith(caller, glare.call.id, 'decline', expect.objectContaining({ revision: 1 }), expect.any(AbortSignal));
    expect(api.session).not.toHaveBeenCalled();
  });

  it('dismisses the sibling window on stop, ignores duplicate and late ringing, and only lets the winning tab join', async () => {
    const { api, emit } = fakeAPI();
    const connect = vi.fn<PrivateCallMedia['connect']>().mockResolvedValue(undefined);
    const first = render(controls(api, callee, { connect }));
    const second = render(controls(api, callee, { connect }));
    expect(connect).not.toHaveBeenCalled();

    emit({ scope: callee, frame: ringing });
    emit({ scope: callee, frame: ringing });
    expect(screen.getAllByRole('button', { name: 'Answer' })).toHaveLength(2);
    const win = deferred<CallReply>();
    const lose = deferred<CallReply>();
    api.action.mockReturnValueOnce(win.promise).mockReturnValueOnce(lose.promise);
    fireEvent.click(within(first.container).getByRole('button', { name: 'Answer' }));
    fireEvent.click(within(second.container).getByRole('button', { name: 'Answer' }));
    emit({ scope: callee, frame: stopped });
    emit({ scope: callee, frame: ringing });
    expect(screen.queryByRole('button', { name: 'Answer' })).toBeNull();

    // Both tabs can see selected_here when they share a credential.
    emit({ scope: callee, frame: { ...active, call: accepted.call } });
    await act(async () => {
      win.resolve(ok(accepted));
      lose.resolve({ kind: 'conflict', value: { ...conflict, call: accepted.call } });
      await Promise.all([win.promise, lose.promise]);
    });
    expect(within(second.container).queryByRole('button', { name: 'Join call' })).toBeNull();
    expect(api.session).not.toHaveBeenCalled();
    expect(connect).not.toHaveBeenCalled();

    fireEvent.click(within(first.container).getByRole('button', { name: 'Join call' }));
    await waitFor(() => { expect(connect).toHaveBeenCalledTimes(1); });
    expect(connect).toHaveBeenCalledWith(callee, expect.objectContaining({ call_id: accepted.call.id, revision: 2 }), expect.any(AbortSignal));
    emit({ scope: callee, frame: active });
    const signal = connect.mock.calls[0]?.[2];
    expect(signal?.aborted).toBe(false);
    const ended = history.calls[0];
    if (ended === undefined) throw new Error('missing ended fixture');
    api.action.mockResolvedValue(ok({ call: ended }));
    fireEvent.click(within(first.container).getByRole('button', { name: 'Hang up' }));
    await waitFor(() => { expect(signal?.aborted).toBe(true); });
    expect(api.action).toHaveBeenLastCalledWith(callee, accepted.call.id, 'hangup', expect.objectContaining({ revision: 3 }), expect.any(AbortSignal));
    expect(screen.getAllByText('Call ended')).toHaveLength(1);
  });

  it('prompts before leaving channel voice and supports Escape back to the Answer button', async () => {
    const { api, emit } = fakeAPI();
    const left = deferred<undefined>();
    const leave = vi.fn(() => left.promise);
    render(controls(api, callee, undefined, { isBusy: () => true, leave }));
    emit({ scope: callee, frame: ringing });
    click('Answer');
    const question = screen.getByRole('group', { name: 'Leave channel voice to answer this call?' });
    expect(document.activeElement).toBe(within(question).getByRole('button', { name: 'Keep channel voice' }));
    fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
    expect(document.activeElement).toBe(button('Answer'));
    expect(leave).not.toHaveBeenCalled();
    expect(api.action).not.toHaveBeenCalled();
    click('Answer');
    click('Leave and answer');
    expect(leave).toHaveBeenCalledTimes(1);
    expect(api.action).not.toHaveBeenCalled();
    await act(async () => { left.resolve(undefined); await left.promise; });
    expect(await screen.findByText('Call audio is unavailable.')).toBeDefined();
    expect(api.session).not.toHaveBeenCalled();
  });

  it('blocks channel voice and watch-only joins while private audio connects and after it joins', async () => {
    const { api, emit } = fakeAPI();
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('No server'))));
    vi.stubGlobal('AudioContext', undefined);
    const connected = deferred<undefined>();
    const connect = vi.fn<PrivateCallMedia['connect']>(() => connected.promise);
    const channelJoin = vi.spyOn(VoiceClient.prototype, 'join').mockImplementation(() => undefined);
    render(
      <PrivateCallProvider scope={callee} options={{ api, media: { connect } }} channelVoice={{ isBusy: channelVoiceBusy, leave: leaveVoice }}>
        <PrivateCallBanner />
        <ChannelVoiceButtons />
      </PrivateCallProvider>,
    );
    emit({ scope: callee, frame: ringing });
    click('Answer');
    fireEvent.click(await screen.findByRole('button', { name: 'Join call' }));
    await waitFor(() => { expect(connect).toHaveBeenCalledOnce(); });
    click('Join voice');
    click('Watch only');
    expect(channelJoin).not.toHaveBeenCalled();
    await act(async () => { connected.resolve(undefined); await connected.promise; });
    expect(screen.queryByRole('button', { name: 'Joining…' })).toBeNull();
    click('Join voice');
    click('Watch only');
    expect(channelJoin).not.toHaveBeenCalled();
    emit({ scope: callee, accessLost: true });
    expect(connect.mock.calls[0]?.[2].aborted).toBe(true);
    click('Join voice');
    expect(channelJoin).toHaveBeenCalledExactlyOnceWith('general', expect.any(Object));
  });

  it('reconciles missed history after an idle snapshot and shows failed calls to the caller', async () => {
    const { api, emit } = fakeAPI();
    const { rerender } = render(controls(api));
    emit({ scope: callee, frame: snapshot });
    expect(button('Answer')).toBeDefined();
    api.history.mockResolvedValue(history);
    emit({ scope: callee, frame: { ...idle, taken_at: '2026-11-19T10:08:00.000Z' } });
    emit({ scope: callee, frame: ringing });
    expect(screen.queryByRole('button', { name: 'Answer' })).toBeNull();
    expect(await screen.findByRole('list', { name: 'Missed calls' })).toBeDefined();
    expect(api.session).not.toHaveBeenCalled();

    rerender(controls(api, caller));
    expect(screen.queryByRole('list', { name: 'Missed calls' })).toBeNull();
    emit({ scope: caller, frame: { ...active, call: failed } });
    expect(await screen.findByText('Call failed')).toBeDefined();
  });

  it('retires a stop-ringing attempt on reconnect before a delayed Answer reply can revive it', async () => {
    const { api, emit } = fakeAPI();
    const answer = deferred<CallReply>();
    const finished = deferred<typeof history>();
    api.action.mockReturnValue(answer.promise);
    api.history.mockReturnValue(finished.promise);
    const connect = vi.fn<PrivateCallMedia['connect']>();
    render(
      <PrivateCallProvider scope={callee} options={{ api, media: { connect } }} channelVoice={noVoice}>
        <PrivateCallBanner />
      </PrivateCallProvider>,
    );
    emit({ scope: callee, frame: ringing });
    click('Answer');
    emit({ scope: callee, frame: stopped });
    expect(screen.queryByRole('button', { name: 'Answer' })).toBeNull();
    emit({ scope: callee, frame: { ...idle, taken_at: '2026-11-19T10:08:00.000Z' } });
    expect(api.history).toHaveBeenCalledExactlyOnceWith(callee, conversationId, expect.any(AbortSignal));
    await act(async () => { answer.resolve(ok(accepted)); await answer.promise; });
    expect(screen.queryByRole('button', { name: 'Join call' })).toBeNull();
    expect(api.session).not.toHaveBeenCalled();
    await act(async () => { finished.resolve(history); await finished.promise; });
    expect(screen.getByRole('status').textContent).toBe('Call ended');
  });

  it('ignores an unseen late ring after an idle snapshot and announces a new missed call outside the DM', async () => {
    const { api, emit } = fakeAPI();
    render(
      <PrivateCallProvider scope={callee} options={{ api }} channelVoice={noVoice}>
        <PrivateCallBanner />
      </PrivateCallProvider>,
    );
    emit({ scope: callee, frame: { ...idle, taken_at: '2026-11-19T10:00:01.000Z' } });
    emit({ scope: callee, frame: ringing });
    expect(screen.queryByRole('button', { name: 'Answer' })).toBeNull();

    const fresh = { ...missed, id: 'new-attempt', created_at: '2026-11-19T10:01:00.000Z', ended_at: '2026-11-19T10:01:40.000Z' };
    emit({ scope: callee, frame: { ...active, call: fresh } });
    expect(screen.getByRole('status').textContent).toBe('Missed call');
    expect(screen.queryByRole('button', { name: 'Answer' })).toBeNull();
    click('Dismiss');
    emit({ scope: callee, frame: { ...active, call: fresh } });
    expect(screen.getByRole('status').textContent).toBe('');
    await act(async () => {});
  });

  it('rejects foreign events and ignores completions after permission loss or a scope change', async () => {
    const { api, emit, subscribers } = fakeAPI();
    const reply = deferred<CallReply>();
    api.action.mockReturnValue(reply.promise);
    const { rerender } = render(controls(api));

    emit({ scope: { ...callee, origin: 'https://other.example.test' }, frame: ringing });
    emit({ scope: caller, frame: ringing });
    emit({ scope: callee, frame: { ...ringing, call: { ...ringing.call, callee_id: 99 } } });
    expect(screen.queryByRole('button', { name: 'Answer' })).toBeNull();
    emit({ scope: callee, frame: ringing });
    click('Answer');
    emit({ scope: callee, accessLost: true });
    expect(screen.queryByRole('region', { name: 'Private call' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Call' })).toBeNull();

    const next = { ...callee, origin: 'https://other.example.test' };
    rerender(controls(api, next));
    await act(async () => { reply.resolve(ok(accepted)); await reply.promise; });
    act(() => { subscribers[0]?.({ scope: next, frame: ringing }); });
    expect(screen.queryByRole('button', { name: 'Answer' })).toBeNull();
    expect(button('Call')).toBeDefined();
    expect(api.session).not.toHaveBeenCalled();
  });

  it.each([
    { revision: 3 },
    { identity: 'p99' },
    { call_id: 'another-attempt' },
    { expires_at: '2000-01-01T00:00:00.000Z' },
  ])('rejects a grant with a mismatched binding (%o)', async (change) => {
    const { api, emit } = fakeAPI();
    const connect = vi.fn<PrivateCallMedia['connect']>().mockResolvedValue(undefined);
    api.session.mockResolvedValue({ ...validGrant(), ...change });
    render(controls(api, callee, { connect }));
    emit({ scope: callee, frame: ringing });
    click('Answer');
    fireEvent.click(await screen.findByRole('button', { name: 'Join call' }));
    await screen.findByRole('alert');
    expect(screen.getByRole('alert').textContent).toBe('Could not join call audio. Try again.');
    expect(connect).not.toHaveBeenCalled();
  });

  it('recovers from denied capture and aborts joined audio when call access is lost', async () => {
    const { api, emit } = fakeAPI();
    const connect = vi.fn<PrivateCallMedia['connect']>()
      .mockRejectedValueOnce(new DOMException('Permission denied', 'NotAllowedError'))
      .mockResolvedValueOnce(undefined);
    render(controls(api, callee, { connect }));
    emit({ scope: callee, frame: ringing });
    click('Answer');
    fireEvent.click(await screen.findByRole('button', { name: 'Join call' }));
    await screen.findByRole('alert');
    expect(connect.mock.calls[0]?.[2].aborted).toBe(true);
    click('Join call');
    await waitFor(() => { expect(screen.queryByRole('button', { name: 'Joining…' })).toBeNull(); });
    const signal = connect.mock.calls[1]?.[2];
    expect(signal?.aborted).toBe(false);
    emit({ scope: callee, accessLost: true });
    expect(signal?.aborted).toBe(true);
    expect(screen.queryByRole('button', { name: 'Hang up' })).toBeNull();
  });

  it('never forwards a late grant after a revision advance or revoked entitlement', async () => {
    const { api, emit } = fakeAPI();
    const connect = vi.fn<PrivateCallMedia['connect']>().mockResolvedValue(undefined);
    const grant = deferred<ReturnType<typeof validGrant>>();
    api.session.mockReturnValueOnce(grant.promise);
    render(controls(api, callee, { connect }));
    emit({ scope: callee, frame: ringing });
    click('Answer');
    fireEvent.click(await screen.findByRole('button', { name: 'Join call' }));
    emit({ scope: callee, frame: active });
    await act(async () => { grant.resolve(validGrant()); await grant.promise; });
    expect(connect).not.toHaveBeenCalled();

    const revokedGrant = deferred<ReturnType<typeof validGrant>>();
    api.session.mockReturnValueOnce(revokedGrant.promise);
    click('Join call');
    emit({ scope: callee, accessLost: true });
    await act(async () => { revokedGrant.resolve({ ...validGrant(), revision: 3 }); await revokedGrant.promise; });
    expect(connect).not.toHaveBeenCalled();
    expect(api.session.mock.calls[1]?.[2].aborted).toBe(true);
  });
});
