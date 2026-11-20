import { createContext, useContext, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore, type ReactNode } from 'react';
import type { PrivateCallV1 } from '../schema/generated';
import { alertIncomingCall, type CallAlert } from './alerts';
import type { CallScope, PrivateCallAPI, PrivateCallMedia } from './api';
import { incoming, PrivateCalls } from './controller';

export interface PrivateCallOptions {
  readonly api: PrivateCallAPI;
  readonly media?: PrivateCallMedia;
  readonly alert?: CallAlert;
}

export interface ChannelVoice {
  readonly isBusy: () => boolean;
  readonly leave: () => Promise<void>;
}

interface CallContext {
  readonly calls: PrivateCalls;
  readonly view: ReturnType<PrivateCalls['snapshot']>;
  readonly mediaAvailable: boolean;
  readonly channelVoice: ChannelVoice;
}

const Calls = createContext<CallContext | null>(null);

export function usePrivateCalls() {
  return useContext(Calls)?.calls;
}

export function PrivateCallProvider({ scope, options, channelVoice, children }: {
  readonly scope: CallScope;
  readonly options: PrivateCallOptions;
  readonly channelVoice: ChannelVoice;
  readonly children: ReactNode;
}) {
  const { origin, account } = scope;
  const { api, media, alert = alertIncomingCall } = options;
  const calls = useMemo(() => new PrivateCalls({ origin, account }, api, media, alert), [origin, account, api, media, alert]);
  const view = useSyncExternalStore(calls.subscribe, calls.snapshot);
  useLayoutEffect(() => calls.start(), [calls]);

  return (
    <Calls.Provider value={{ calls, view, mediaAvailable: media !== undefined && api.session !== undefined, channelVoice }}>
      {children}
    </Calls.Provider>
  );
}

const outcome: Record<PrivateCallV1['state'], string> = {
  ringing: 'Calling…',
  accepted: 'Call accepted',
  active: 'Call in progress',
  unavailable: 'Call unavailable',
  declined: 'Call declined',
  cancelled: 'Call cancelled',
  missed: 'Missed call',
  ended: 'Call ended',
  failed: 'Call failed',
};

function describeOutcome(call: PrivateCallV1): string {
  return call.state === 'missed' && call.role === 'caller' ? 'Call went unanswered' : outcome[call.state];
}

// The DM route supplies its durable conversation id. Incoming invitations
// stay in the app banner so there is only one Answer prompt per window.
export function DMCallControls({ conversationId }: { readonly conversationId: number }) {
  const context = useContext(Calls);
  const calls = context?.calls;
  useEffect(() => { void calls?.loadHistory(conversationId); }, [calls, conversationId]);
  if (context === null || !context.view.available) return null;
  const { view } = context;
  const current = view.current?.conversation_id === conversationId ? view.current : null;
  const finished = view.finished.filter((call) => call.conversation_id === conversationId)
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
  const notice = view.notice?.conversation_id === conversationId ? view.notice : null;
  const lastFinished = finished[0];
  const latest = notice !== null && (lastFinished === undefined || Date.parse(notice.created_at) >= Date.parse(lastFinished.created_at))
    ? notice : lastFinished;
  const missed = finished.filter((call) => call.role === 'callee' && call.state === 'missed');
  return (
    <section aria-label="Private call controls">
      {current === null ? (
        <>
          <button type="button" disabled={view.pending || view.current !== null} onClick={() => { void context.calls.invite(conversationId); }}>Call</button>
          <p role="status">{latest === undefined || (latest.state === 'missed' && latest.role === 'callee') ? '' : describeOutcome(latest)}</p>
          {view.current === null && view.problem !== null ? <p role="alert">{view.problem}</p> : null}
        </>
      ) : incoming(current) ? null : (
        <AttemptControls key={current.id} context={context} call={current} />
      )}
      {missed.length === 0 ? null : (
        <ul aria-label="Missed calls">
          {missed.map((call) => {
            const at = call.ended_at ?? call.created_at;
            return <li key={call.id}>Missed call <time dateTime={at}>{new Date(at).toLocaleString()}</time></li>;
          })}
        </ul>
      )}
    </section>
  );
}

export function PrivateCallBanner({ conversationId, nameOf }: {
  readonly conversationId?: number;
  readonly nameOf?: (principal: number) => string | undefined;
}) {
  const context = useContext(Calls);
  const [dismissed, setDismissed] = useState<PrivateCallV1 | null>(null);
  if (context === null || !context.view.available) return null;

  const { current, notice, owned } = context.view;
  const isIncoming = incoming(current);
  const show = current !== null && (isIncoming || (owned && current.conversation_id !== conversationId));
  const showNotice = current === null && notice !== null && notice !== dismissed && notice.conversation_id !== conversationId;
  const peer = isIncoming && current !== null ? nameOf?.(current.caller_id) : undefined;
  const invitation = isIncoming ? (peer === undefined ? 'Incoming private call' : `Incoming call from ${peer}`) : undefined;

  return (
    <>
      {/* Keep the live region mounted before a ring and across DM navigation. */}
      <p className="vh" role="status">{invitation ?? (showNotice ? describeOutcome(notice) : '')}</p>
      {show ? (
        <section className="banner" aria-label="Private call">
          <AttemptControls key={current.id} context={context} call={current} invitation={invitation} />
        </section>
      ) : null}
      {showNotice ? (
        <section className="banner" aria-label="Private call">
          <p>{describeOutcome(notice)}</p>
          <button type="button" onClick={() => { setDismissed(notice); }}>Dismiss</button>
        </section>
      ) : null}
    </>
  );
}

function AttemptControls({ context, call, invitation }: {
  readonly context: CallContext;
  readonly call: PrivateCallV1;
  readonly invitation?: string;
}) {
  const { calls, view, channelVoice, mediaAvailable } = context;
  const [confirm, setConfirm] = useState<{ action: 'accept' | 'join'; revision: number } | null>(null);
  const answer = useRef<HTMLButtonElement>(null);
  const keepVoice = useRef<HTMLButtonElement>(null);
  const confirming = confirm?.revision === call.revision ? confirm.action : null;
  const restoreFocus = useRef(false);
  useLayoutEffect(() => {
    if (confirming !== null) keepVoice.current?.focus();
    else if (restoreFocus.current) answer.current?.focus();
    restoreFocus.current = false;
  }, [confirming]);
  const question = useId();
  const proceed = (action: 'accept' | 'join', leave?: () => Promise<void>) => {
    setConfirm(null);
    if (action === 'accept') void calls.act(call, 'accept', leave);
    else void calls.join(call, channelVoice.isBusy, leave);
  };
  const ask = (action: 'accept' | 'join') => {
    if (channelVoice.isBusy()) setConfirm({ action, revision: call.revision });
    else proceed(action);
  };
  const keep = () => {
    restoreFocus.current = true;
    setConfirm(null);
  };

  return (
    <>
      {invitation === undefined ? <p role="status">{outcome[call.state]}</p> : <p aria-hidden="true">{invitation}</p>}
      {confirming !== null ? (
        <div role="group" aria-labelledby={question} onKeyDown={(event) => {
          if (event.key !== 'Escape') return;
          event.preventDefault();
          keep();
        }}>
          <p id={question}>Leave channel voice to {confirming === 'accept' ? 'answer' : 'join'} this call?</p>
          <div className="actions">
            <button ref={keepVoice} type="button" onClick={keep}>Keep channel voice</button>
            <button type="button" onClick={() => { proceed(confirming, channelVoice.leave); }}>
              {confirming === 'accept' ? 'Leave and answer' : 'Leave and join'}
            </button>
          </div>
        </div>
      ) : (
        <div className="actions">
          {call.state === 'ringing' ? (
            call.role === 'callee' ? (
              <>
                <button ref={answer} type="button" disabled={view.pending} onClick={() => { ask('accept'); }}>Answer</button>
                <button type="button" disabled={view.pending} onClick={() => { void calls.act(call, 'decline'); }}>Decline</button>
              </>
            ) : (
              <button type="button" disabled={view.pending} onClick={() => { void calls.act(call, 'cancel'); }}>Cancel</button>
            )
          ) : (
            <>
              {view.owned && mediaAvailable && view.media !== 'joined' ? (
                <button ref={answer} type="button" disabled={view.pending || view.media === 'joining'} onClick={() => { ask('join'); }}>
                  {view.media === 'joining' ? 'Joining…' : 'Join call'}
                </button>
              ) : null}
              <button type="button" disabled={view.pending} onClick={() => { void calls.act(call, 'hangup'); }}>Hang up</button>
            </>
          )}
        </div>
      )}
      {view.owned && call.state === 'accepted' && !mediaAvailable ? <p>Call audio is unavailable.</p> : null}
      {view.problem === null ? null : <p role="alert">{view.problem}</p>}
    </>
  );
}
