import { privateCallSessionV1, type PrivateCallV1 } from '../schema/generated';
import type { CallAlert } from './alerts';
import type { CallAction, CallEvent, CallReply, CallScope, PrivateCallAPI, PrivateCallMedia } from './api';

export function live(call: PrivateCallV1): boolean {
  return call.state === 'ringing' || joinable(call);
}

function joinable(call: PrivateCallV1): boolean {
  return call.state === 'accepted' || call.state === 'active';
}

export function incoming(call: PrivateCallV1 | null): boolean {
  return call?.state === 'ringing' && call.role === 'callee';
}

interface View {
  readonly current: PrivateCallV1 | null;
  readonly finished: readonly PrivateCallV1[];
  readonly notice: PrivateCallV1 | null;
  readonly owned: boolean;
  readonly available: boolean;
  readonly pending: boolean;
  readonly media: 'idle' | 'joining' | 'joined';
  readonly problem: string | null;
}

const idle: View = { current: null, finished: [], notice: null, owned: false, available: false, pending: false, media: 'idle', problem: null };
// Matches PrivateCallActionWindowSeconds in pkg/schema/private_call.go.
const inviteRetryWindowMs = 60 * 60 * 1000;

export class PrivateCalls {
  private view = idle;
  private readonly listeners = new Set<() => void>();
  private readonly calls = new Map<string, PrivateCallV1>();
  private readonly stopped = new Map<string, number>();
  private readonly retired = new Set<string>();
  private readonly invites = new Map<number, { id: string; startedAt: number }>();
  private binding: AbortController | undefined;
  private capture: AbortController | undefined;
  private closeAlert: (() => void) | undefined;
  private owner: string | undefined;
  private snapshotAt = -Infinity;

  constructor(
    private readonly scope: CallScope,
    private readonly api: PrivateCallAPI,
    private readonly media: PrivateCallMedia | undefined,
    private readonly alert: CallAlert,
  ) {}

  snapshot = (): View => this.view;
  audioBusy = (): boolean => this.capture !== undefined;
  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  };

  private update(update: Partial<View>): void {
    this.view = { ...this.view, ...update };
    for (const listener of this.listeners) listener();
  }

  start(): () => void {
    const binding = new AbortController();
    this.binding = binding;
    this.update({ available: true });
    this.api.subscribe(this.scope, (event) => {
      if (!binding.signal.aborted) this.receive(event);
    }, binding.signal);
    return () => { this.clear(); };
  }

  private clear(): void {
    this.binding?.abort();
    this.binding = undefined;
    this.stopCapture();
    this.closeAlert?.();
    this.closeAlert = undefined;
    this.owner = undefined;
    this.snapshotAt = -Infinity;
    this.calls.clear();
    this.stopped.clear();
    this.retired.clear();
    this.invites.clear();
    this.update(idle);
  }

  private stopCapture(): void {
    this.capture?.abort();
    this.capture = undefined;
  }

  private setCurrent(call: PrivateCallV1 | null): void {
    const previous = this.view.current;
    if (previous?.id !== call?.id || !incoming(call)) {
      this.closeAlert?.();
      this.closeAlert = undefined;
    }
    if (previous?.id !== call?.id || call?.selected_here !== true) this.stopCapture();
    this.update({
      current: call,
      owned: call !== null && this.owner === call.id && call.selected_here,
      media: this.capture === undefined ? 'idle' : this.view.media,
      problem: previous?.id === call?.id ? this.view.problem : null,
    });
  }

  private belongs(call: PrivateCallV1): boolean {
    return (call.role === 'caller' ? call.caller_id : call.callee_id) === this.scope.account &&
      call.caller_id !== call.callee_id;
  }

  private record(call: PrivateCallV1, source: 'stream' | 'snapshot' | 'reply' | 'history'): void {
    if (!this.belongs(call)) return;
    const previous = this.calls.get(call.id);
    if (previous !== undefined && (previous.revision >= call.revision || !live(previous) ||
      previous.conversation_id !== call.conversation_id || previous.caller_id !== call.caller_id ||
      previous.callee_id !== call.callee_id || previous.role !== call.role)) return;
    if (call.revision < (this.stopped.get(call.id) ?? 0)) return;

    const current = this.view.current;
    if (live(call)) {
      if (this.retired.has(call.id)) return;
      if (previous === undefined && source !== 'snapshot' && Date.parse(call.created_at) < this.snapshotAt) return;
      if (current !== null && current.id !== call.id) {
        if (Date.parse(call.created_at) < Date.parse(current.created_at)) return;
        this.retired.add(current.id);
      }
      this.calls.set(call.id, call);
      this.setCurrent(call);
      if (source === 'stream' && incoming(call) && previous === undefined) this.closeAlert = this.alert(this.scope);
    } else {
      this.calls.set(call.id, call);
      if (current?.id === call.id) this.setCurrent(null);
      const lastNotice = this.view.notice;
      const showNotice = (previous !== undefined || source === 'reply' ||
        (source === 'stream' && call.role === 'callee' && call.state === 'missed')) &&
        (lastNotice === null || Date.parse(call.created_at) >= Date.parse(lastNotice.created_at));
      // The caller can see that ringing went unanswered. Only the callee
      // gets a missed-call history entry.
      const callerMissed = call.state === 'missed' && call.role === 'caller';
      this.update({
        ...(showNotice ? { notice: call } : {}),
        ...(callerMissed ? {} : { finished: [call, ...this.view.finished] }),
      });
    }
  }

  private receive(event: CallEvent): void {
    if (event.scope.origin !== this.scope.origin || event.scope.account !== this.scope.account) return;
    if ('accessLost' in event) {
      this.clear();
      return;
    }
    const { frame } = event;
    switch (frame.schema) {
      case 'conch.private_call_ringing.v1':
      case 'conch.private_call_state.v1':
        this.record(frame.call, 'stream');
        break;
      case 'conch.private_call_stop_ringing.v1': {
        if (frame.revision <= (this.stopped.get(frame.call_id) ?? 0)) return;
        this.stopped.set(frame.call_id, frame.revision);
        const call = this.view.current;
        if (incoming(call) && call?.id === frame.call_id && call.revision < frame.revision) this.setCurrent(null);
        break;
      }
      case 'conch.private_call_snapshot.v1': {
        if (frame.call !== undefined && (!this.belongs(frame.call) || !live(frame.call))) return;
        const takenAt = Date.parse(frame.taken_at);
        if (takenAt < this.snapshotAt) return;
        this.snapshotAt = takenAt;
        const current = this.view.current;
        // A reconnect snapshot can account for a terminal transition this
        // tab missed, even if a stop-ringing hint already cleared the UI.
        const reconcile = new Set<number>();
        for (const call of this.calls.values()) {
          if (!live(call) || call.id === frame.call?.id || this.retired.has(call.id) || Date.parse(call.created_at) > takenAt) continue;
          this.retired.add(call.id);
          if (this.view.current?.id === call.id) this.setCurrent(null);
          reconcile.add(call.conversation_id);
        }
        for (const conversation of reconcile) void this.loadHistory(conversation);
        if (frame.call !== undefined && (current === null || current.id === frame.call.id || Date.parse(current.created_at) <= takenAt)) {
          this.record(frame.call, 'snapshot');
        }
        break;
      }
    }
  }

  async loadHistory(conversation: number): Promise<void> {
    const signal = this.binding?.signal;
    if (signal === undefined) return;
    try {
      const page = await this.api.history(this.scope, conversation, signal);
      if (signal.aborted) return;
      for (const call of page.calls) {
        if (call.conversation_id === conversation && !live(call) && call.state !== 'unavailable') this.record(call, 'history');
      }
    } catch {
      // History is supplementary. The current call remains actionable.
    }
  }

  async invite(conversation: number): Promise<void> {
    if (this.binding === undefined || this.view.current !== null || this.view.pending) return;
    const now = Date.now();
    const invite = this.invites.get(conversation) ?? { id: crypto.randomUUID(), startedAt: now };
    if (now - invite.startedAt >= inviteRetryWindowMs) {
      this.invites.delete(conversation);
      this.update({ problem: 'The call retry window expired. Select Call to start a new attempt.' });
      return;
    }
    this.invites.set(conversation, invite);
    await this.request(conversation, undefined, (signal) =>
      this.api.invite(this.scope, conversation, { action_id: invite.id }, signal), 'caller');
  }

  async act(call: PrivateCallV1, action: CallAction, leaveVoice?: () => Promise<void>): Promise<void> {
    if (!this.isCurrent(call)) return;
    const ringing = call.state === 'ringing';
    const allowed = action === 'hangup' ? joinable(call) : ringing && (
      action === 'cancel' ? call.role === 'caller' : call.role === 'callee'
    );
    if (!allowed) return;

    if (action === 'hangup' && this.view.media === 'joining') {
      this.stopCapture();
      this.update({ media: 'idle' });
    }
    await this.request(call.conversation_id, call.id, async (signal) => {
      if (leaveVoice !== undefined) await leaveVoice();
      if (signal.aborted || !this.isCurrent(call)) return undefined;
      return this.api.action(this.scope, call.id, action, { action_id: crypto.randomUUID(), revision: call.revision }, signal);
    }, action === 'accept' ? 'callee' : undefined);
  }

  private async request(
    conversation: number,
    attempt: string | undefined,
    send: (signal: AbortSignal) => Promise<CallReply | undefined>,
    select?: PrivateCallV1['role'],
  ): Promise<void> {
    const signal = this.binding?.signal;
    if (signal === undefined || this.view.pending) return;
    this.update({ pending: true, problem: null });
    try {
      const reply = await send(signal);
      if (signal.aborted || reply === undefined) return;
      if (reply.kind === 'access-lost') {
        this.clear();
        return;
      }
      if (reply.kind === 'failed') throw new Error('Call request failed');

      const call = reply.value.call;
      if (!this.belongs(call) || call.conversation_id !== conversation || (attempt !== undefined && call.id !== attempt)) {
        throw new Error('Call response did not match the request');
      }
      if (attempt === undefined) this.invites.delete(conversation);
      this.record(call, 'reply');
      // A state frame can arrive before this window's HTTP success.
      const current = this.view.current;
      if (reply.kind === 'ok' && reply.value.replayed !== true && call.role === select &&
        call.selected_here && live(call) && current?.id === call.id && current.selected_here) {
        this.owner = call.id;
        this.update({ owned: true });
      }
    } catch {
      if (!signal.aborted) this.update({ problem: 'Could not complete the call action. Try again.' });
    } finally {
      if (!signal.aborted) this.update({ pending: false });
    }
  }

  private isCurrent(call: PrivateCallV1): boolean {
    return this.view.current?.id === call.id && this.view.current.revision === call.revision;
  }

  async join(call: PrivateCallV1, channelVoiceBusy: () => boolean, leaveVoice?: () => Promise<void>): Promise<void> {
    const { media, api } = this;
    const binding = this.binding;
    if (media === undefined || api.session === undefined || binding === undefined ||
      !this.isCurrent(call) || !joinable(call) || !this.view.owned || this.capture !== undefined || this.view.pending) return;

    const capture = new AbortController();
    this.capture = capture;
    this.update({ media: 'joining', problem: null });
    const stillBound = () => !binding.signal.aborted && !capture.signal.aborted &&
      this.isCurrent(call) && this.view.owned && !this.view.pending && !channelVoiceBusy();

    try {
      if (leaveVoice !== undefined) await leaveVoice();
      if (!stillBound()) return;
      const grant = await api.session(this.scope, call.id, capture.signal);
      if (!stillBound()) return;
      if (privateCallSessionV1(grant).length !== 0 || grant.call_id !== call.id ||
        grant.revision !== call.revision || grant.identity !== `p${String(this.scope.account)}` ||
        Date.parse(grant.expires_at) <= Date.now()) throw new Error('Call session binding changed');
      await media.connect(this.scope, grant, capture.signal);
      if (!capture.signal.aborted) this.update({ media: 'joined' });
    } catch {
      if (!capture.signal.aborted) this.update({ problem: 'Could not join call audio. Try again.' });
    } finally {
      if (this.capture === capture && this.view.media !== 'joined') {
        this.stopCapture();
        this.update({ media: 'idle' });
      }
    }
  }
}
