// The browser client's top level: sign-in, the operator warning, then the
// lobby. The auth, connection and channels reducers hold the state; src/api.ts
// does every call and is the only code that sees the credential.
import { useCallback, useEffect, useReducer, useRef, useState } from 'react';
import * as api from './api';
import { Conversation } from './Conversation';
import { Lobby } from './Lobby';
import { OperatorWarning } from './OperatorWarning';
import { channelFromHash, forgetChannel, hashForChannel, rememberChannel, rememberedChannel } from './route';
import { SignIn } from './SignIn';
import { authReducer, initialAuth } from './state/auth';
import { channelsReducer, initialChannels } from './state/channels';
import { connectionReducer } from './state/connection';
import { whisperNotice } from './Composer';
import { draftFor, draftsReducer, initialDrafts, initialTargets, targetFor, targetsReducer } from './state/drafts';
import type { Audience } from './schema/message';
import { canLoadOlder, chatFor, chatReducer, initialChat, lastId, oldestId, ordered, type ChatAction } from './state/messages';
import { useLiveChannel } from './useLiveChannel';
import { useReadState } from './useReadState';
import { MembersPanel } from './agents/MembersPanel';
import { RunDetail } from './agents/RunDetail';
import { useAgents } from './agents/useAgents';
import { useNow } from './agents/useNow';

export function App({ origin }: { readonly origin: string }) {
  const [auth, dispatchAuth] = useReducer(authReducer, undefined, () => initialAuth(api.hasToken()));
  const [connection, dispatchConnection] = useReducer(connectionReducer, 'signedOut');
  const [channels, dispatchChannels] = useReducer(channelsReducer, initialChannels);
  const [chat, dispatchRaw] = useReducer(chatReducer, initialChat);
  const [drafts, dispatchDrafts] = useReducer(draftsReducer, initialDrafts);
  const [targets, dispatchTargets] = useReducer(targetsReducer, initialTargets);
  // Whether this tab has shown the audit-log notice: once per tab.
  const whisperNoted = useRef(false);
  const [attempt, setAttempt] = useState(0);
  // The newest chat state, for code that outlives a render (the socket's resume point).
  // It is moved by the same reducer in the same call as the state, so it is
  // never behind: the history is loaded and the socket opened in one tick.
  const chatNow = useRef(chat);
  const dispatchChat = useCallback((action: ChatAction) => {
    chatNow.current = chatReducer(chatNow.current, action);
    dispatchRaw(action);
  }, []);

  // A credential left by an earlier load of this tab is checked once.
  const restoring = auth.phase === 'restoring';
  useEffect(() => {
    if (!restoring) return;
    let live = true;
    void api.restore().then((result) => {
      if (!live) return;
      if (result.ok) dispatchAuth({ type: 'verified', principal: result.value });
      else dispatchAuth({ type: 'failed', failure: result.failure });
    });
    return () => {
      live = false;
    };
  }, [restoring]);

  const signedIn = auth.phase === 'signedIn';

  // The lobby loads the caller's channels. A 401 here, or anywhere later,
  // sends the page back to sign-in.
  useEffect(() => {
    if (!signedIn) return;
    let live = true;
    if (attempt === 0) dispatchConnection({ type: 'started' });
    void api.listChannels().then((result) => {
      if (!live) return;
      if (result.ok) {
        dispatchConnection({ type: 'reached' });
        dispatchChannels({
          type: 'loaded',
          channels: result.value,
          preferred: channelFromHash(window.location.hash),
          remembered: rememberedChannel(),
        });
      } else if (result.failure === 'unauthorized') {
        dispatchAuth({ type: 'unauthorized' });
      } else {
        dispatchConnection({ type: 'lost' });
      }
    });
    return () => {
      live = false;
    };
  }, [signedIn, attempt]);

  useEffect(() => {
    if (auth.phase === 'signedOut') {
      dispatchConnection({ type: 'closed' });
      dispatchChannels({ type: 'reset' });
      dispatchChat({ type: 'reset' });
      dispatchDrafts({ type: 'reset' });
      dispatchTargets({ type: 'reset' });
      setAttempt(0);
    }
  }, [auth.phase]);

  // The hash names the open channel, so back, forward and a pasted link work.
  useEffect(() => {
    if (!signedIn) return;
    function onHash() {
      const name = channelFromHash(window.location.hash);
      if (name !== null) dispatchChannels({ type: 'select', name });
    }
    window.addEventListener('hashchange', onHash);
    return () => {
      window.removeEventListener('hashchange', onHash);
    };
  }, [signedIn]);

  useEffect(() => {
    if (channels.selected === null) return;
    rememberChannel(channels.selected);
    const hash = hashForChannel(channels.selected);
    if (window.location.hash !== hash) window.location.hash = hash;
  }, [channels.selected]);

  const onUnauthorized = useCallback(() => {
    dispatchAuth({ type: 'unauthorized' });
  }, []);
  const onLost = useCallback((name: string) => {
    dispatchChannels({ type: 'lost', name });
  }, []);
  const getAfter = useCallback((name: string) => lastId(chatFor(chatNow.current, name)), []);
  // One older page at a time, and none past maxHeld.
  const getOldest = useCallback((name: string) => {
    const held = chatFor(chatNow.current, name);
    return held.older === 'loading' || !canLoadOlder(held) ? null : oldestId(held);
  }, []);

  const principal = auth.phase === 'signedIn' ? auth.principal : null;
  const selected = signedIn ? channels.selected : null;
  const channelId = channels.channels.find((c) => c.name === selected)?.id ?? null;
  const live = useLiveChannel({
    channel: channelId === null ? null : selected,
    channelId,
    me: principal ?? { id: 0, name: '', kind: 'human' },
    getAfter,
    getOldest,
    dispatchChat,
    dispatchConnection,
    onUnauthorized,
    onLost,
  });

  // Whether the reader is at the newest message of the open channel; a channel
  // newly opened starts at it.
  const [bottom, setBottom] = useState<{ readonly channel: string | null; readonly atBottom: boolean }>({ channel: null, atBottom: true });
  const held = selected === null ? null : chatFor(chat, selected);
  const head = held === null ? undefined : ordered(held).at(-1);
  const unread = useReadState({
    signedIn,
    channels: channels.channels,
    loaded: channels.loaded,
    open: channelId === null ? null : selected,
    ready: held?.history === 'ready',
    newest: head === undefined ? null : { id: head.id, author: head.author_id },
    atBottom: bottom.channel === selected ? bottom.atBottom : true,
    status: live.status,
    me: principal?.id ?? 0,
    onUnauthorized,
  });

  // The browser's own session, the events socket and what the panel and run
  // detail show from them.
  const agents = useAgents({
    signedIn,
    channel: channelId === null ? null : selected,
    channelId,
    onUnauthorized,
  });
  const now = useNow(signedIn);

  // Go to message: run detail closes, and once the conversation is on screen
  // again it scrolls to the message the run answers and puts focus on it. A
  // message older than the history held is said so, since there is nothing to
  // scroll to.
  const [scrollTo, setScrollTo] = useState<number | null>(null);
  const closeRun = agents.closeRun;
  const goToMessage = useCallback(
    (id: number) => {
      if (selected !== null && ordered(chatFor(chatNow.current, selected)).some((m) => m.id === id)) {
        closeRun(`Run answers message #${String(id)}`);
        setScrollTo(id);
      } else {
        closeRun(`Message #${String(id)} is older than the history held`);
      }
    },
    [selected, closeRun],
  );
  const detailOpen = agents.state.detail !== null;
  useEffect(() => {
    if (scrollTo === null || detailOpen) return;
    setScrollTo(null);
    const el = document.querySelector<HTMLElement>(`[data-message-id="${String(scrollTo)}"]`);
    if (el === null) return;
    el.scrollIntoView({ block: 'center' });
    el.focus({ preventScroll: true });
  }, [scrollTo, detailOpen]);

  // One POST per click. Nothing here sends a message a second time on its own.
  const post = useCallback(
    (name: string, localId: number, body: string, audience?: Audience) => {
      void api.postMessage(name, body, audience).then((result) => {
        if (result.ok) {
          dispatchChat({ type: 'sent', channel: name, localId, message: result.message });
          if (audience?.kind === 'principals') {
            const note = whisperNoted.current ? null : whisperNotice;
            whisperNoted.current = true;
            dispatchTargets({ type: 'whispered', channel: name, sent: audience.principal_ids, note });
          }
        } else if (result.outcome === 'unauthorized') dispatchAuth({ type: 'unauthorized' });
        else {
          // A refusal stays as a Failed line with Retry and Dismiss, so the text
          // is never lost; the server's own line is shown beside the composer.
          if (result.outcome === 'refused') dispatchTargets({ type: 'refused', channel: name, line: result.reason });
          dispatchChat({ type: 'sendFailed', channel: name, localId, outcome: result.outcome === 'refused' ? 'failed' : 'unconfirmed' });
        }
      });
    },
    [],
  );

  const releaseSession = agents.release;
  const signOut = useCallback(() => {
    // The session is ended first: the request needs the credential that
    // signing out forgets.
    releaseSession();
    api.signOut();
    forgetChannel();
    dispatchAuth({ type: 'signOut' });
  }, [releaseSession]);

  const submit = useCallback((credential: string) => {
    dispatchAuth({ type: 'submit' });
    void api.signIn(credential).then((result) => {
      if (result.ok) dispatchAuth({ type: 'verified', principal: result.value });
      else dispatchAuth({ type: 'failed', failure: result.failure });
    });
  }, []);

  switch (auth.phase) {
    case 'restoring':
      return (
        <main className="signin">
          <h1>Conch</h1>
          <p role="status">Checking your sign-in…</p>
        </main>
      );
    case 'signedOut':
    case 'checking':
      return (
        <SignIn
          origin={origin}
          notice={auth.phase === 'signedOut' ? auth.notice : null}
          checking={auth.phase === 'checking'}
          onSubmit={submit}
        />
      );
    case 'operatorWarning':
      return (
        <OperatorWarning
          name={auth.principal.name}
          onContinue={() => {
            if (api.continueAsOperator()) dispatchAuth({ type: 'continue' });
            else dispatchAuth({ type: 'failed', failure: 'invalid' });
          }}
          onSignOut={signOut}
        />
      );
    case 'signedIn':
      return (
        <Lobby
          origin={origin}
          name={auth.principal.name}
          channels={channels}
          unread={unread}
          connection={connection}
          panel={<MembersPanel agents={agents} now={now} />}
          conversation={
            selected === null || channelId === null ? null : (
              <>
                {agents.state.detail === null ? null : <RunDetail agents={agents} now={now} onGoToMessage={goToMessage} />}
                <div className="conversation-body" hidden={agents.state.detail !== null}>
              <Conversation
                key={selected}
                channel={selected}
                chat={chatFor(chat, selected)}
                live={live.status}
                people={live.people}
                nets={live.nets}
                monitored={live.monitored}
                viewer={auth.principal.id}
                draft={draftFor(drafts, selected)}
                target={targetFor(targets, selected)}
                onDraft={(text) => {
                  dispatchDrafts({ type: 'edit', channel: selected, text });
                }}
                onTarget={dispatchTargets}
                onSend={(body, audience) => {
                  const localId = chatFor(chat, selected).nextLocalId;
                  dispatchTargets({ type: 'posting', channel: selected });
                  dispatchChat({ type: 'sending', channel: selected, body, at: Date.now(), ...(audience === undefined ? {} : { audience }) });
                  dispatchDrafts({ type: 'edit', channel: selected, text: '' });
                  post(selected, localId, body, audience);
                }}
                onRetrySend={(localId) => {
                  const send = chatFor(chat, selected).pending.find((p) => p.localId === localId);
                  if (send === undefined || send.status === 'sending') return;
                  dispatchTargets({ type: 'posting', channel: selected });
                  dispatchChat({ type: 'retrying', channel: selected, localId });
                  post(selected, localId, send.body, send.audience);
                }}
                onDismissSend={(localId) => {
                  dispatchChat({ type: 'discard', channel: selected, localId });
                }}
                onLoadOlder={live.loadOlder}
                onReload={live.retry}
                onBottom={(atBottom) => {
                  setBottom({ channel: selected, atBottom });
                }}
              />
                </div>
              </>
            )
          }
          onOpen={(name) => {
            dispatchChannels({ type: 'select', name });
          }}
          onRetry={() => {
            setAttempt((n) => n + 1);
          }}
          onSignOut={signOut}
        />
      );
  }
}
