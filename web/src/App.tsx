// The browser client's top level: sign-in, the operator warning, then the
// lobby. The auth, connection and channels reducers hold the state; src/api.ts
// does every call and is the only code that sees the credential.
import { useCallback, useEffect, useLayoutEffect, useReducer, useRef, useState } from 'react';
import * as api from './api';
import { ChangeName } from './ChangeName';
import { CreateChannel } from './CreateChannel';
import { Conversation } from './Conversation';
import { Invite } from './Invite';
import { InvitePanel } from './InvitePanel';
import { Lobby } from './Lobby';
import { ReportProblem } from './Report';
import { OperatorWarning } from './OperatorWarning';
import { channelFromHash, forgetChannel, hashForChannel, rememberChannel, rememberedChannel, takeLinkFromAddress } from './route';
import type { WhoAmIResponseV1 } from './schema/principal';
import { SignIn } from './SignIn';
import { YourSignIns } from './SignIns';
import { SignInLink } from './SignInLink';
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
import { VoiceStatus } from './VoiceStatus';
import { useOccupancy } from './voice/useOccupancy';

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
  // Why the last sign out everywhere did not happen, shown in the lobby.
  const [everywhereFailed, setEverywhereFailed] = useState<string | null>(null);
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
      if (!result.ok) dispatchAuth({ type: 'failed', failure: result.failure });
      // An operator credential found where only a member's may last is held
      // back for the warning, as at sign-in.
      else if (result.value.warn) dispatchAuth({ type: 'held', principal: result.value.principal });
      else dispatchAuth({ type: 'verified', principal: result.value.principal });
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
      setEverywhereFailed(null);
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
  // The one way a channel is opened: from the list, or from the voice strip.
  const onOpen = useCallback((name: string) => {
    dispatchChannels({ type: 'select', name });
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
  // The browser's own session, the events socket and what the panel and run
  // detail show from them. The member list it keeps current also names the
  // authors in the conversation.
  const agents = useAgents({
    signedIn,
    channel: channelId === null ? null : selected,
    channelId,
    onUnauthorized,
  });
  const live = useLiveChannel({
    channel: channelId === null ? null : selected,
    channelId,
    me: principal ?? { id: 0, name: '', kind: 'human' },
    members: agents.members,
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
  // A channel opened again starts at the bottom, whatever it was left at.
  const [bottomFor, setBottomFor] = useState(selected);
  if (bottomFor !== selected) {
    setBottomFor(selected);
    setBottom({ channel: selected, atBottom: true });
  }
  const held = selected === null ? null : chatFor(chat, selected);
  const headId = held === null ? 0 : lastId(held);
  const head = held?.byId.get(headId);
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

  const now = useNow(signedIn);

  // A server that predates voice_allowed says nothing: voice, as the invite
  // panel reads it. Agents have no voice (ADR-004).
  const voiceAllowed = principal !== null && principal.voice_allowed !== false && principal.kind === 'human';
  // How many are in each channel's voice (issue #596): one socket for the
  // page, for the list and the strip. Never opened for someone without voice.
  const occupancy = useOccupancy({ signedIn, voiceAllowed, reachable: connection === 'connected', onUnauthorized });

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
  // One sign-out at a time: a second click while the first is with the server
  // does nothing.
  const signingOut = useRef(false);
  const signOut = useCallback(() => {
    if (signingOut.current) return;
    signingOut.current = true;
    // The session is ended first, and waited for: the request needs the
    // credential that signing out revokes, and the revoke ends the session.
    void releaseSession()
      .then(() => api.signOut())
      .then((told) => {
        signingOut.current = false;
        forgetChannel();
        dispatchAuth({ type: 'signOut', notice: told ? null : 'untold' });
      });
  }, [releaseSession]);

  // Sign out everywhere revokes every credential of the person. If the server
  // does not do it, the person stays signed in here and is told, so they can
  // try again: dropping the credential would leave the others alive with no
  // way left to end them from this browser.
  const principalId = principal?.id ?? null;
  const signOutEverywhere = useCallback(() => {
    if (signingOut.current || principalId === null) return;
    signingOut.current = true;
    setEverywhereFailed(null);
    void api.signOutEverywhere(principalId).then((result) => {
      signingOut.current = false;
      if (result.ok) {
        forgetChannel();
        dispatchAuth({ type: 'signOut', notice: 'everywhere' });
      } else if (result.failure === 'unauthorized') {
        dispatchAuth({ type: 'unauthorized' });
      } else {
        setEverywhereFailed(everywhereFailure(result));
      }
    });
  }, [principalId]);

  // Another tab of this browser signing the shared credential out signs this
  // one out too.
  useEffect(() => {
    if (!signedIn) return;
    return api.onSignedOutElsewhere(() => {
      forgetChannel();
      dispatchAuth({ type: 'signOut', notice: 'elsewhere' });
    });
  }, [signedIn]);

  // The code of an invite link (issue #440) or a sign-in link (issue #493),
  // held here in memory only: never stored, and dropped once it is used or
  // the person goes back. It is taken out of the address before the first
  // paint, and again when a link is pasted into a tab that is already open (a
  // hashchange, no reload). The newest link wins.
  const [inviteCode, setInviteCode] = useState<string | null>(null);
  const [signInCode, setSignInCode] = useState<string | null>(null);
  useLayoutEffect(() => {
    function take() {
      const found = takeLinkFromAddress();
      if (found?.kind === 'invite') {
        setSignInCode(null);
        setInviteCode(found.inviteCode);
      } else if (found?.kind === 'signin') {
        setInviteCode(null);
        setSignInCode(found.signInCode);
      }
    }
    take();
    window.addEventListener('hashchange', take);
    return () => {
      window.removeEventListener('hashchange', take);
    };
  }, []);

  // Continue as a new person, or as the person a sign-in link is for:
  // whoever is signed in is signed out properly first, their credential
  // revoked, before the invite or the link signs someone in.
  const leaveForInvite = useCallback(async (): Promise<boolean> => {
    await releaseSession();
    const told = await api.signOut();
    forgetChannel();
    dispatchAuth({ type: 'signOut', notice: told ? null : 'untold' });
    return told;
  }, [releaseSession]);

  // Joined, or signed back in through a sign-in link: the code is dropped and
  // the lobby opens, on the first channel an invite granted. A redemption is
  // a sign-in, so it takes a pasted credential's two steps. When whoami did
  // not answer, it is asked again.
  const joined = useCallback((who: WhoAmIResponseV1 | null, channel: string | undefined) => {
    setInviteCode(null);
    setSignInCode(null);
    if (channel !== undefined) window.location.hash = hashForChannel(channel);
    dispatchAuth({ type: 'submit' });
    if (who !== null) {
      dispatchAuth({ type: 'verified', principal: who });
      return;
    }
    void api.whoAmI().then((result) => {
      if (result.ok) dispatchAuth({ type: 'verified', principal: result.value });
      else dispatchAuth({ type: 'failed', failure: result.failure });
    });
  }, []);

  const submit = useCallback((credential: string) => {
    dispatchAuth({ type: 'submit' });
    void api.signIn(credential).then((result) => {
      if (result.ok) dispatchAuth({ type: 'verified', principal: result.value });
      else if (result.failure === 'stored') dispatchAuth({ type: 'recheck' });
      else dispatchAuth({ type: 'failed', failure: result.failure });
    });
  }, []);

  // Forgetting a stored sign-in that could not be checked (issue #469): it is
  // revoked if the server answers, then dropped, and signing in is open again.
  const forgetStored = useCallback(() => {
    void api.forgetStored().then((told) => {
      dispatchAuth({ type: 'signOut', notice: told ? null : 'untold' });
    });
  }, []);

  // An invite link is answered before anything else, once a stored sign-in
  // has been checked, so the page can say who is signed in already.
  if (inviteCode !== null && auth.phase !== 'restoring') {
    const signedInAs = auth.phase === 'signedIn' || auth.phase === 'operatorWarning' ? auth.principal.name : null;
    return (
      <Invite
        origin={origin}
        inviteCode={inviteCode}
        signedInAs={signedInAs}
        unchecked={signedInAs === null && api.heldElsewhere()}
        onNewPerson={leaveForInvite}
        onBack={() => {
          setInviteCode(null);
        }}
        onJoined={joined}
      />
    );
  }

  // A sign-in link likewise, once a stored sign-in has been checked.
  if (signInCode !== null && auth.phase !== 'restoring') {
    const signedInAs = auth.phase === 'signedIn' || auth.phase === 'operatorWarning' ? auth.principal.name : null;
    return (
      <SignInLink
        origin={origin}
        signInCode={signInCode}
        signedInAs={signedInAs}
        unchecked={signedInAs === null && api.heldElsewhere()}
        onSignOutFirst={leaveForInvite}
        onBack={() => {
          setSignInCode(null);
        }}
        onSignedIn={(who) => {
          joined(who, undefined);
        }}
      />
    );
  }

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
          onForget={forgetStored}
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
          me={auth.principal.id}
          channels={channels}
          unread={unread}
          occupancy={occupancy}
          connection={connection}
          panel={<MembersPanel agents={agents} now={now} />}
          account={
            // A person renames themselves; an agent's name is an operator's to set.
            auth.principal.kind === 'human' ? (
              <ChangeName
                id={auth.principal.id}
                name={auth.principal.name}
                onRenamed={(p) => {
                  dispatchAuth({ type: 'renamed', name: p.name });
                }}
                onUnauthorized={onUnauthorized}
              />
            ) : null
          }
          signIns={(endThis) => <YourSignIns onEndThis={endThis} onUnauthorized={onUnauthorized} />}
          report={<ReportProblem onUnauthorized={onUnauthorized} />}
          voice={
            <VoiceStatus
              signedIn={signedIn}
              channel={selected}
              viewer={auth.principal.id}
              people={live.people}
              voiceAllowed={voiceAllowed}
              reachable={connection === 'connected'}
              channels={channels.channels}
              occupancy={occupancy}
              onUnauthorized={onUnauthorized}
              onOpen={onOpen}
            />
          }
          newChannel={
            // The server allows operators alone; anyone else is not offered it.
            auth.principal.role === 'operator' ? (
              <CreateChannel
                onCreated={(channel) => {
                  dispatchChannels({ type: 'created', channel });
                }}
                onUnauthorized={onUnauthorized}
              />
            ) : null
          }
          invites={
            auth.principal.may_invite === true ? (
              <InvitePanel origin={origin} me={auth.principal} channels={channels.channels} onUnauthorized={onUnauthorized} />
            ) : null
          }
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
                  setBottom((prev) => (prev.channel === selected && prev.atBottom === atBottom ? prev : { channel: selected, atBottom }));
                }}
              />
                </div>
              </>
            )
          }
          onOpen={onOpen}
          onRetry={() => {
            setAttempt((n) => n + 1);
          }}
          onSignOut={signOut}
          onSignOutEverywhere={signOutEverywhere}
          signOutProblem={everywhereFailed}
        />
      );
  }
}

// everywhereFailure is the line shown when sign out everywhere did not happen.
function everywhereFailure(result: { readonly failure: api.ApiFailure; readonly code?: string }): string {
  if (result.failure === 'network') return 'Could not sign out everywhere: the server cannot be reached. You are still signed in; try again.';
  if (result.code === 'last_operator') return 'Could not sign out everywhere: you are the only operator who can sign in.';
  return 'Could not sign out everywhere: the server refused. You are still signed in.';
}
