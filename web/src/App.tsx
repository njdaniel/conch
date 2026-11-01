// The browser client's top level: sign-in, the operator warning, then the
// lobby. The auth, connection and channels reducers hold the state; src/api.ts
// does every call and is the only code that sees the credential.
import { useCallback, useEffect, useLayoutEffect, useReducer, useRef, useState } from 'react';
import * as api from './api';
import { uploadRefusalLine } from './fileSize';
import { ChangeName } from './ChangeName';
import { CreateChannel } from './CreateChannel';
import { Conversation } from './Conversation';
import { GuestVisit } from './Guest';
import { displayName, visitOf } from './guestName';
import { channelWords, Invite, refusalLine, waitWords } from './Invite';
import { InvitePanel } from './InvitePanel';
import { FirstRun } from './FirstRun';
import { Lobby } from './Lobby';
import { ReportProblem } from './Report';
import { OperatorWarning } from './OperatorWarning';
import { RenameChannel } from './RenameChannel';
import { channelFromHash, forgetChannel, hashForChannel, rememberChannel, rememberedChannel, takeLinkFromAddress } from './route';
import type { ChannelDocV1 } from './schema/channel';
import type { WhoAmIResponseV1 } from './schema/principal';
import type { ReadStateV1 } from './schema/readstate';
import { SignIn } from './SignIn';
import { YourSignIns } from './SignIns';
import { SignInLink } from './SignInLink';
import { AddDevice } from './AddDevice';
import { GuestSettings } from './GuestSettings';
import { Reauth } from './Reauth';
import { PasswordSettings } from './Settings';
import { Setup } from './Setup';
import { authReducer, initialAuth, keptAcross, operatorSession } from './state/auth';
import { channelsReducer, initialChannels, type ChannelsAction } from './state/channels';
import { connectionReducer } from './state/connection';
import { ComposerAttachmentProvider, whisperNotice, type ComposerAttachmentContextValue } from './Composer';
import { clearStored, forgetChannelStored, loadStored, pruneStored, saveDrafts, storedAudience, type StoredTarget } from './state/draftStore';
import { draftFor, draftsReducer, initialDrafts, initialTargets, targetFor, targetsReducer, type Target, type TargetAction } from './state/drafts';
import type { Audience, MessageV2 } from './schema/message';
import {
  attachmentDraftsReducer,
  attachmentPayload,
  attachmentsFor,
  initialAttachmentDrafts,
  normalizeAudience,
  type AttachmentDraftAction,
  type AttachmentDrafts,
  type DraftAttachment,
  type NewDraftAttachment,
} from './state/attachments';
import { canLoadOlder, chatFor, chatReducer, initialChat, lastId, oldestId, ordered, sameSend, type ChatAction, type PendingSend } from './state/messages';
import { useLiveChannel } from './useLiveChannel';
import { useNotifications } from './useNotifications';
import { NotificationCenter } from './NotificationCenter';
import { NotificationSettings } from './NotificationSettings';
import { SearchPanel } from './SearchPanel';
import { useReadState } from './useReadState';
import { useChannelRefresh } from './useChannelRefresh';
import { MembersPanel } from './agents/MembersPanel';
import { RunDetail } from './agents/RunDetail';
import { useAgents } from './agents/useAgents';
import { useNow } from './agents/useNow';
import { VoiceStatus } from './VoiceStatus';
import { useOccupancy } from './voice/useOccupancy';
import { needsHttps } from './transport';

const checkingAudience = 'Checking who this draft goes to…';
const audienceGone = 'Who this draft was for is no longer available. Choose who it goes to before sending.';

// The attachment drafts kept across a sign-in that ended: by channel id
// instead of name (issue #1067), the audience scope of each kept as it is.
interface KeptAttachments {
  readonly byChannel: Readonly<Record<number, Readonly<Record<string, readonly DraftAttachment[]>>>>;
  readonly nextClientId: number;
}

function keepAttachments(drafts: AttachmentDrafts, channels: readonly { readonly id: number; readonly name: string }[]): KeptAttachments {
  const byChannel: Record<number, Readonly<Record<string, readonly DraftAttachment[]>>> = {};
  for (const channel of channels) {
    const scopes = drafts.channels[channel.name];
    if (scopes !== undefined) byChannel[channel.id] = scopes;
  }
  return { byChannel, nextClientId: drafts.nextClientId };
}

function restoreAttachments(kept: KeptAttachments, channels: readonly { readonly id: number; readonly name: string }[], generation: number): AttachmentDrafts {
  const named: Record<string, Readonly<Record<string, readonly DraftAttachment[]>>> = {};
  for (const [id, scopes] of Object.entries(kept.byChannel)) {
    const name = channels.find((c) => String(c.id) === id)?.name;
    if (name !== undefined) named[name] = scopes;
  }
  return { channels: named, nextClientId: kept.nextClientId, generation };
}

// App is the page. A guest who creates an account (issue #713) becomes a
// member with a new credential, and every socket, session and list the page
// held was the guest's: the page starts over, as on a reload, with the
// member's credential, rather than mend each of them in place.
export function App({ origin }: { readonly origin: string }) {
  const [generation, setGeneration] = useState(0);
  const restart = useCallback(() => {
    setGeneration((n) => n + 1);
  }, []);
  return <Session key={generation} origin={origin} onRestart={restart} />;
}

function Session({ origin, onRestart }: { readonly origin: string; readonly onRestart: () => void }) {
  const [auth, dispatchAuth] = useReducer(authReducer, undefined, () => initialAuth(api.hasToken()));
  const [connection, dispatchConnection] = useReducer(connectionReducer, 'signedOut');
  const [channels, dispatchChannelsRaw] = useReducer(channelsReducer, initialChannels);
  const [chat, dispatchRaw] = useReducer(chatReducer, initialChat);
  const [drafts, dispatchDrafts] = useReducer(draftsReducer, initialDrafts);
  const [targets, dispatchTargets] = useReducer(targetsReducer, initialTargets);
  const [attachmentDrafts, dispatchAttachmentsRaw] = useReducer(attachmentDraftsReducer, initialAttachmentDrafts);
  const attachmentsNow = useRef(attachmentDrafts);
  const dispatchAttachments = useCallback((action: AttachmentDraftAction) => {
    attachmentsNow.current = attachmentDraftsReducer(attachmentsNow.current, action);
    dispatchAttachmentsRaw(action);
  }, []);
  const uploadControllers = useRef(new Map<string, { readonly controller: AbortController; readonly channelId: number }>());
  const channelEpochs = useRef(new Map<number, number>());
  const originNow = useRef(origin);
  originNow.current = origin;
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      for (const upload of uploadControllers.current.values()) upload.controller.abort();
      uploadControllers.current.clear();
    };
  }, []);
  // Whether this tab has shown the audit-log notice: once per tab.
  const whisperNoted = useRef(false);
  const [attempt, setAttempt] = useState(0);
  // Why the last sign out everywhere did not happen, shown in the lobby.
  const [everywhereFailed, setEverywhereFailed] = useState<string | null>(null);
  // The invite a person chose to sign in instead of using (issue #713): its
  // channels and who sent it, so the lobby can say if the account they
  // signed in to is not in them. Signing in uses nothing of an invite.
  const [destination, setDestination] = useState<{ readonly channels: readonly string[]; readonly inviter: string } | null>(null);
  // The newest chat state, for code that outlives a render (the socket's resume point).
  // It is moved by the same reducer in the same call as the state, so it is
  // never behind: the history is loaded and the socket opened in one tick.
  const chatNow = useRef(chat);
  const dispatchChat = useCallback((action: ChatAction) => {
    chatNow.current = chatReducer(chatNow.current, action);
    dispatchRaw(action);
  }, []);
  // The newest channel list, kept the same way: a channel renamed (issue #728)
  // is called by its new name from the moment the rename is applied, so a
  // socket frame or an answer arriving before the next render goes under it.
  const channelsNow = useRef(channels);
  const dispatchChannels = useCallback((action: ChannelsAction) => {
    channelsNow.current = channelsReducer(channelsNow.current, action);
    dispatchChannelsRaw(action);
  }, []);
  // A channel's name now, by its id.
  const nameOf = useCallback((id: number) => channelsNow.current.channels.find((c) => c.id === id)?.name ?? null, []);

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
  }, [signedIn, attempt, dispatchChannels]);

  // What was being typed when a sign-in ended by itself (issue #714: an
  // operator's session runs out after an hour without use, or any
  // credential is revoked mid-sentence), kept in memory for the same person
  // signing back in, and for nobody else. A sign-out the person chose
  // forgets it.
  const kept = useRef<{
    readonly principal: number;
    // By channel id, as the audiences are (issue #1039): a channel renamed,
    // or a new one given the old name, while signed out never gets the text.
    readonly drafts: Readonly<Record<number, string>>;
    // Images by channel id too, each under the audience scope it was added to
    // (issue #1067): they come back to the same channel and the same
    // audience, which is held until checked like the text's.
    readonly attachments: KeptAttachments;
    readonly pending: Readonly<Record<string, readonly PendingSend[]>>;
    // Who each channel's draft was for, by channel id (issue #1039): text
    // kept without it would come back addressed to the whole channel.
    readonly targets: Readonly<Record<number, StoredTarget>>;
  } | null>(null);
  // Whether the drafts kept in this tab for a reload (issue #1039) have been
  // given back; until then nothing is written, so the empty start cannot
  // overwrite them.
  const [draftsReady, setDraftsReady] = useState(false);
  // The audiences kept across a reload that are not yet checked against the
  // channel's nets and members, by channel id. Until then the draft cannot
  // be sent, and what was kept is what stays stored.
  //
  // The rule for every kept draft, across a reload and across a sign-in that
  // ended by itself: it never reaches a wider audience than the one it was
  // written for. The audience is kept through one function (storedAudience);
  // it comes back held, unsendable, until it is known to stand, and then
  // whole (the 'restore' action replaces, never merges); a choice the person
  // makes meanwhile wins and the kept one is dropped (chooseTarget). Whatever
  // comes back, from storage, from the drafts kept across a sign-in or from
  // a send a 401 gave back, is protected: Escape never changes it, and only
  // choosing the channel or a net does. When in doubt, the draft stays
  // unsendable.
  const pendingTargets = useRef(new Map<number, StoredTarget>());
  // The audience of a send whose text a 401 gave back to the composer, by
  // channel id; null is the whole channel. The text goes with the audience
  // it was sent to, never with whatever the composer shows by then.
  const returnedAudiences = useRef(new Map<number, StoredTarget | null>());
  const draftsNow = useRef(drafts);
  useEffect(() => {
    draftsNow.current = drafts;
  }, [drafts]);
  const targetsNow = useRef(targets);
  useEffect(() => {
    targetsNow.current = targets;
  }, [targets]);
  const lastSignedIn = useRef<number | null>(null);
  const endedNotice = auth.phase === 'signedOut' ? auth.notice : null;

  useEffect(() => {
    if (auth.phase === 'signedOut') {
      // Once, as the sign-in ends; the notice is the one it ended with.
      if (lastSignedIn.current !== null) {
        const typed: Record<number, string> = {};
        for (const channel of channelsNow.current.channels) {
          const text = draftsNow.current[channel.name];
          if (text !== undefined && text !== '') typed[channel.id] = text;
        }
        const files = keepAttachments(attachmentsNow.current, channelsNow.current.channels);
        const pending: Record<string, readonly PendingSend[]> = {};
        if (keptAcross(endedNotice)) {
          for (const [channel, chat] of Object.entries(chatNow.current.channels)) {
            if (chat.pending.length === 0) continue;
            pending[channel] = chat.pending.map((p): PendingSend =>
              p.phase === 'posting' || p.phase === 'reconciling' ? { ...p, status: 'unconfirmed', phase: 'unconfirmed' } : p,
            );
          }
        }
        // Each channel's audience goes with its text: one still being checked
        // is kept as it was, never as the placeholder.
        const audiences: Record<number, StoredTarget> = {};
        for (const channel of channelsNow.current.channels) {
          const returned = returnedAudiences.current.get(channel.id);
          const audience = returned !== undefined ? returned : (pendingTargets.current.get(channel.id) ?? storedAudience(targetFor(targetsNow.current, channel.name)));
          if (audience !== null) audiences[channel.id] = audience;
        }
        kept.current = keptAcross(endedNotice) && (Object.keys(typed).length > 0 || Object.keys(files.byChannel).length > 0 || Object.keys(pending).length > 0)
          ? { principal: lastSignedIn.current, drafts: typed, attachments: files, pending, targets: audiences }
          : null;
        lastSignedIn.current = null;
      }
      for (const upload of uploadControllers.current.values()) upload.controller.abort();
      uploadControllers.current.clear();
      dispatchConnection({ type: 'closed' });
      dispatchChannels({ type: 'reset' });
      dispatchChat({ type: 'reset' });
      dispatchDrafts({ type: 'reset' });
      dispatchTargets({ type: 'reset' });
      dispatchAttachments({ type: 'reset' });
      // Nothing of this person's stays readable in the tab (issue #1039),
      // unless the sign-in only could not be checked: a reload with the
      // server away leaves the drafts for when it is back.
      if (endedNotice !== 'network') clearStored();
      pendingTargets.current.clear();
      returnedAudiences.current.clear();
      setDraftsReady(false);
      setAttempt(0);
      setEverywhereFailed(null);
    }
  }, [auth.phase, dispatchChannels, dispatchChat, dispatchAttachments]);

  // Signing out forgets the invite a person signed in instead of using: the
  // next person here did not open it.
  const lastPhase = useRef(auth.phase);
  useEffect(() => {
    if (lastPhase.current === 'signedIn' && auth.phase !== 'signedIn') setDestination(null);
    lastPhase.current = auth.phase;
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
  }, [signedIn, dispatchChannels]);

  // The open channel is remembered and in the address, under its new name
  // once it is renamed. The hash is set as when another channel is opened:
  // the one way the page writes the address bar (hygiene.test.ts).
  useEffect(() => {
    if (channels.selected === null) return;
    rememberChannel(channels.selected);
    const hash = hashForChannel(channels.selected);
    if (window.location.hash !== hash) window.location.hash = hash;
  }, [channels.selected]);

  // A channel renamed (issue #728), however the page learned it: the
  // operator's own answer, the events socket, or the list read again. The
  // list, the open channel and everything kept by its name move to the new
  // one; nothing is reloaded and no socket is reopened.
  const invalidateChannel = useCallback((id: number) => {
    channelEpochs.current.set(id, (channelEpochs.current.get(id) ?? 0) + 1);
    for (const [key, upload] of uploadControllers.current) {
      if (upload.channelId !== id) continue;
      upload.controller.abort();
      uploadControllers.current.delete(key);
    }
  }, []);

  const renamed = useCallback(
    (id: number, name: string) => {
      const was = channelsNow.current.channels.find((c) => c.id === id);
      if (was === undefined || was.name === name) return;
      const pending = chatFor(chatNow.current, was.name).pending.filter((p) => p.phase === 'posting' || p.phase === 'reconciling');
      const uploading = Object.entries(attachmentsNow.current.channels[was.name] ?? {}).flatMap(([scope, attachments]) =>
        attachments.filter((attachment) => attachment.status === 'uploading').map((attachment) => ({ scope, attachment })),
      );
      invalidateChannel(id);
      dispatchChannels({ type: 'renamed', id, name });
      dispatchChat({ type: 'renamed', from: was.name, to: name });
      for (const send of pending) {
        dispatchChat({
          type: 'sendFailed',
          channel: name,
          localId: send.localId,
          outcome: 'unconfirmed',
          ...(send.requestId === undefined ? {} : { requestId: send.requestId }),
        });
      }
      dispatchDrafts({ type: 'renamed', from: was.name, to: name });
      dispatchTargets({ type: 'renamed', from: was.name, to: name });
      dispatchAttachments({ type: 'renamed', from: was.name, to: name });
      for (const { scope, attachment } of uploading) {
        dispatchAttachments({
          type: 'cancelled',
          channel: name,
          audience: audienceForScope(scope),
          clientId: attachment.clientId,
          generation: attachmentsNow.current.generation,
          attempt: attachment.attempt,
        });
      }
    },
    [invalidateChannel, dispatchChannels, dispatchChat, dispatchAttachments],
  );

  // Everything the page keeps of a channel it can no longer open: its
  // messages, draft and audience go too, so a later channel given the same
  // name starts empty (issue #1027).
  const dropChannel = useCallback((id: number, name: string) => {
    invalidateChannel(id);
    for (const scope of Object.keys(attachmentsNow.current.channels[name] ?? {})) {
      dispatchAttachments({ type: 'clear', channel: name, audience: audienceForScope(scope) });
    }
    dispatchChat({ type: 'forget', channel: name });
    dispatchDrafts({ type: 'forget', channel: name });
    dispatchTargets({ type: 'forget', channel: name });
    // The draft and any open edit kept for a reload go with it (issue #1039).
    pendingTargets.current.delete(id);
    if (principalNow.current !== null) forgetChannelStored(principalNow.current, id);
  }, [invalidateChannel, dispatchAttachments, dispatchChat]);

  const loseChannel = useCallback((id: number, name: string) => {
    dropChannel(id, name);
    dispatchChannels({ type: 'lost', name });
  }, [dispatchChannels, dropChannel]);

  // The slow way (issue #728): a call for a channel answered
  // channel_not_found. The list is read again and the channel found by its
  // id: under a new name, the page applies the rename; not there at all, it
  // is lost, as after a 1008. It resolves to the channel's name now, or null.
  // One read at a time for a channel: calls that fail together share it.
  const refinding = useRef(new Map<number, Promise<string | null>>());
  const refind = useCallback(
    (id: number): Promise<string | null> => {
      const asked = refinding.current.get(id);
      if (asked !== undefined) return asked;
      const reading = api.listChannels().then((result) => {
        refinding.current.delete(id);
        if (!result.ok) {
          if (result.failure === 'unauthorized') dispatchAuth({ type: 'unauthorized' });
          return null;
        }
        const found = result.value.find((c) => c.id === id);
        const was = channelsNow.current.channels.find((c) => c.id === id);
        if (found === undefined) {
          if (was !== undefined) loseChannel(id, was.name);
          return null;
        }
        renamed(id, found.name);
        return found.name;
      });
      refinding.current.set(id, reading);
      return reading;
    },
    [loseChannel, renamed],
  );
  const onMissing = useCallback(
    (id: number) => {
      void refind(id);
    },
    [refind],
  );
  const onChannel = useCallback(
    (doc: ChannelDocV1) => {
      renamed(doc.channel_id, doc.name);
    },
    [renamed],
  );
  const onManages = useCallback(
    (id: number, manages: boolean) => {
      dispatchChannels({ type: 'managed', id, manages });
    },
    [dispatchChannels],
  );

  const onUnauthorized = useCallback(() => {
    dispatchAuth({ type: 'unauthorized' });
  }, []);
  const onLost = useCallback(
    (name: string) => {
      const channel = channelsNow.current.channels.find((c) => c.name === name);
      if (channel !== undefined) loseChannel(channel.id, name);
    },
    [loseChannel],
  );
  const [searchOpen, setSearchOpen] = useState(false);
  // Asks from the first-run panel for the New channel form (issue #1032).
  const [createAsks, setCreateAsks] = useState(0);
  const restoreSearchFocus = useRef(false);
  useEffect(() => {
    if (searchOpen) {
      restoreSearchFocus.current = true;
      return;
    }
    if (!restoreSearchFocus.current) return;
    restoreSearchFocus.current = false;
    document.getElementById('channel-search-open')?.focus();
  }, [searchOpen]);
  // The one way a channel is opened: from the list, or from the voice strip.
  const onOpen = useCallback(
    (name: string) => {
      setSearchOpen(false);
      dispatchChannels({ type: 'select', name });
    },
    [dispatchChannels],
  );
  const getAfter = useCallback((name: string) => lastId(chatFor(chatNow.current, name)), []);
  // One older page at a time, and none past maxHeld.
  const getOldest = useCallback((name: string) => {
    const held = chatFor(chatNow.current, name);
    return held.older === 'loading' || !canLoadOlder(held) ? null : oldestId(held);
  }, []);

  const principal = auth.phase === 'signedIn' ? auth.principal : null;
  // A temporary guest's live visit (issue #713), and how the page names the
  // person: a guest by its attribution, never by the text of its handle.
  const visit = principal === null ? null : visitOf(principal);
  const shownName = principal === null ? '' : displayName(principal);
  const selected = signedIn ? channels.selected : null;
  const currentChannel = channels.channels.find((c) => c.name === selected) ?? null;
  const channelId = currentChannel?.id ?? null;
  // The browser's own session, the events socket and what the panel and run
  // detail show from them. The member list it keeps current also names the
  // authors in the conversation.
  // The list read again and replaced (issue #1027), on the events socket's word
  // and on focus; the counts are then read again too.
  const reloadReadState = useRef<(() => void) | null>(null);
  const reloadCounts = useCallback(() => {
    reloadReadState.current?.();
  }, []);
  const refreshChannels = useChannelRefresh({
    signedIn,
    channelsNow,
    empty: channels.loaded && channels.channels.length === 0,
    dispatchChannels,
    renamed,
    forget: dropChannel,
    onUnauthorized,
    reloadReadState: reloadCounts,
  });
  const [readStateEvent, setReadStateEvent] = useState<ReadStateV1 | null>(null);
  const [messageEvent, setMessageEvent] = useState<MessageV2 | null>(null);
  const [editEvent, setEditEvent] = useState<MessageV2 | null>(null);
  useEffect(() => {
    setReadStateEvent(null);
    setMessageEvent(null);
    setEditEvent(null);
  }, [signedIn, principal?.id]);
  // An edited message (issue #845): the copy goes to whatever the page holds
  // of it, the open conversation and a notification alike, and is dropped
  // where nothing holds it. It is never a new message.
  const edited = useCallback((message: MessageV2) => {
    const name = nameOf(message.channel_id);
    if (name !== null) dispatchChat({ type: 'edited', channel: name, message });
  }, [nameOf, dispatchChat]);
  const editFinal = useCallback((id: number) => {
    if (selected !== null) dispatchChat({ type: 'editFinal', channel: selected, id });
  }, [selected, dispatchChat]);
  const onMessageEdited = useCallback((message: MessageV2) => {
    setEditEvent(message);
    edited(message);
  }, [edited]);
  const agents = useAgents({
    signedIn,
    channel: channelId === null ? null : selected,
    channelId,
    nameOf,
    viewer: principal?.id ?? 0,
    onUnauthorized,
    onChannel,
    onManages,
    onMissing,
    onReadState: setReadStateEvent,
    onMessage: setMessageEvent,
    onMessageEdited,
    onChannelsChanged: refreshChannels,
  });
  const live = useLiveChannel({
    channel: channelId === null ? null : selected,
    channelId,
    nameOf,
    me: principal === null ? { id: 0, name: '', kind: 'human' } : { id: principal.id, name: shownName, kind: principal.kind },
    members: agents.members,
    getAfter,
    getOldest,
    dispatchChat,
    dispatchConnection,
    onUnauthorized,
    onLost,
    onRenamed: renamed,
    onMissing,
  });

  // Whether the reader is at the newest message of the open channel; a channel
  // newly opened starts at it. By id, so a rename is not another channel.
  const [bottom, setBottom] = useState<{ readonly channel: number | null; readonly atBottom: boolean }>({ channel: null, atBottom: true });
  // A channel opened again starts at the bottom, whatever it was left at.
  const [bottomFor, setBottomFor] = useState(channelId);
  if (bottomFor !== channelId) {
    setBottomFor(channelId);
    setBottom({ channel: channelId, atBottom: true });
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
    atBottom: bottom.channel === channelId ? bottom.atBottom : true,
    status: live.status,
    eventsLive: agents.state.link === 'live',
    event: readStateEvent,
    me: principal?.id ?? 0,
    onUnauthorized,
    onMissing,
    reload: reloadReadState,
  });
  const notifications = useNotifications({
    signedIn,
    origin,
    principal: principal === null ? null : { id: principal.id, name: shownName },
    channels: channels.channels,
    loaded: channels.loaded,
    eventsLive: agents.state.link === 'live',
    event: messageEvent,
    editEvent,
    readStateEvent,
    onOpen,
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
      setSearchOpen(false);
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
    if (scrollTo === null || detailOpen || searchOpen) return;
    setScrollTo(null);
    const el = document.querySelector<HTMLElement>(`.conversation-body [data-message-id="${String(scrollTo)}"]`);
    if (el === null) return;
    el.scrollIntoView({ block: 'center' });
    el.focus({ preventScroll: true });
  }, [scrollTo, detailOpen, searchOpen]);

  const principalId = principal?.id ?? null;
  const principalNow = useRef(principalId);
  principalNow.current = principalId;
  const nextRequest = useRef(1);

  const activeSend = useCallback((send: FrozenSend): boolean => {
    return mounted.current && originNow.current === send.origin && principalNow.current === send.principal &&
      attachmentsNow.current.generation === send.generation &&
      (channelEpochs.current.get(send.channelId) ?? 0) === send.channelEpoch &&
      channelsNow.current.channels.some((channel) => channel.id === send.channelId && channel.name === send.channel);
  }, []);

  const freezeSend = useCallback((channelId: number, channel: string, body: string, audience: Audience | undefined, attachments: readonly DraftAttachment[], threadRootId?: number): FrozenSend | null => {
    if (principalId === null) return null;
    const normalized = normalizeAudience(audience);
    const frozen = [...attachments];
    return {
      origin,
      principal: principalId,
      channelId,
      channel,
      channelEpoch: channelEpochs.current.get(channelId) ?? 0,
      generation: attachmentsNow.current.generation,
      requestId: `browser-send-${String(nextRequest.current++)}`,
      body,
      ...(normalized === undefined ? {} : { audience: normalized }),
      ...(threadRootId === undefined ? {} : { threadRootId }),
      attachments: frozen,
      ...(frozen.length === 0 ? {} : { payload: attachmentPayload(frozen) }),
    };
  }, [principalId, origin]);

  const markSent = useCallback((send: FrozenSend, localId: number, message: MessageV2) => {
    const name = nameOf(send.channelId) ?? send.channel;
    dispatchChat({ type: 'sent', channel: name, localId, message, requestId: send.requestId });
    if (send.threadRootId === undefined && send.audience?.kind === 'principals') {
      const note = whisperNoted.current ? null : whisperNotice;
      whisperNoted.current = true;
      // By the send's own request id, never by who it went to (issue #1066):
      // the reducer ends the whisper only if the composer still holds the
      // draft this send took.
      dispatchTargets({ type: 'whispered', channel: name, requestId: send.requestId, note });
    }
  }, [nameOf, dispatchChat]);

  // One POST per click. Nothing here sends a message a second time on its own.
  const post = useCallback((send: FrozenSend, localId: number) => {
    if (!activeSend(send)) return;
    void api.postMessage(send.channel, send.body, send.threadRootId === undefined ? send.audience : undefined, send.payload, send.threadRootId).then((result) => {
      if (!activeSend(send)) return;
      const name = nameOf(send.channelId) ?? send.channel;
      if (result.ok) {
        markSent(send, localId, result.message);
      } else if (result.outcome === 'unauthorized') {
        if (send.attachments.length === 0 && send.threadRootId === undefined) {
          // Text alone returns to the composer; attachments stay in the pending
          // line so their bytes and uploaded file ids survive the sign-in.
          if ((draftsNow.current[name] ?? '') === '') {
            dispatchDrafts({ type: 'edit', channel: name, text: send.body });
            dispatchTargets({ type: 'edited', channel: name });
            returnedAudiences.current.set(send.channelId, storedAudienceOf(send.audience, send.netUnder));
          }
          dispatchChat({ type: 'discard', channel: name, localId, requestId: send.requestId });
        } else {
          dispatchChat({ type: 'sendFailed', channel: name, localId, outcome: 'failed', requestId: send.requestId });
        }
        dispatchAuth({ type: 'unauthorized' });
      } else {
        // A refusal stays as a Failed line with Retry and Dismiss, so the text
        // is never lost; the server's own line is shown beside the composer.
        if (result.outcome === 'refused') dispatchTargets({ type: 'refused', channel: name, line: result.reason });
        if (result.outcome === 'refused' && result.code === api.channelNotFound) onMissing(send.channelId);
        const uncertainAttachment = result.outcome === 'refused' && result.code === 'invalid_file_attachment';
        dispatchChat({
          type: 'sendFailed',
          channel: name,
          localId,
          outcome: result.outcome === 'refused' && !uncertainAttachment ? 'failed' : 'unconfirmed',
          requestId: send.requestId,
        });
      }
    });
  }, [activeSend, nameOf, markSent, dispatchChat, onMissing]);

  const uploadOne = useCallback(async (send: FrozenSend, attachment: DraftAttachment): Promise<DraftAttachment> => {
    if (attachment.status === 'ready') return attachment;
    if (!activeSend(send)) return { ...attachment, status: 'cancelled' };
    const channel = nameOf(send.channelId) ?? send.channel;
    dispatchAttachments({ type: 'uploading', channel, ...(send.audience === undefined ? {} : { audience: send.audience }), clientId: attachment.clientId, generation: send.generation });
    const attempt = attachment.attempt + 1;
    const controller = new AbortController();
    const key = `${String(send.generation)}:${String(attachment.clientId)}`;
    uploadControllers.current.set(key, { controller, channelId: send.channelId });
    const result = await api.uploadFile(send.channel, attachment.filename, attachment.mediaType, attachment.bytes, controller.signal);
    if (uploadControllers.current.get(key)?.controller === controller) uploadControllers.current.delete(key);
    if (!activeSend(send)) return { ...attachment, status: 'cancelled', attempt };
    const currentChannel = nameOf(send.channelId) ?? send.channel;
    const action = { channel: currentChannel, ...(send.audience === undefined ? {} : { audience: send.audience }), clientId: attachment.clientId, generation: send.generation, attempt };
    if (result.ok) {
      dispatchAttachments({ ...action, type: 'uploaded', fileId: result.file.id });
      return { ...attachment, status: 'ready', attempt, fileId: result.file.id, error: undefined };
    }
    if (result.outcome === 'cancelled') {
      dispatchAttachments({ ...action, type: 'cancelled' });
      return { ...attachment, status: 'cancelled', attempt, error: undefined };
    }
    if (result.outcome === 'unauthorized') {
      dispatchAuth({ type: 'unauthorized' });
      return { ...attachment, status: 'failed', attempt, error: 'Your sign-in ended before the upload finished.' };
    }
    if (result.outcome === 'network') {
      const error = 'Cannot reach the server. The file is still here; try again.';
      dispatchAttachments({ ...action, type: 'uploadFailed', error });
      return { ...attachment, status: 'failed', attempt, error };
    }
    const error = 'reason' in result ? uploadRefusalLine(attachment.filename, result.code, result.reason) : 'The server refused the upload';
    dispatchAttachments({ ...action, type: 'uploadFailed', error });
    return { ...attachment, status: 'failed', attempt, error };
  }, [activeSend, nameOf, dispatchAttachments]);

  const beginSend = useCallback((send: FrozenSend) => {
    const name = nameOf(send.channelId) ?? send.channel;
    const localId = chatFor(chatNow.current, name).nextLocalId;
    // A send of the composer's draft takes it by request id (issue #1066); a
    // reply in a thread leaves the composer's draft as it is.
    dispatchTargets(send.threadRootId === undefined ? { type: 'posting', channel: name, requestId: send.requestId } : { type: 'posting', channel: name });
    dispatchChat({
      type: 'sending',
      channel: name,
      channelId: send.channelId,
      authorId: send.principal,
      body: send.body,
      at: Date.now(),
      attachments: send.attachments,
      requestId: send.requestId,
      ...(send.audience === undefined ? {} : { audience: send.audience }),
      ...(send.threadRootId === undefined ? {} : { threadRootId: send.threadRootId }),
    });
    if (send.threadRootId === undefined) dispatchDrafts({ type: 'edit', channel: name, text: '' });
    post(send, localId);
  }, [nameOf, dispatchChat, post]);

  const send = useCallback((channelId: number, channel: string, body: string, audience: Audience | undefined, files: readonly DraftAttachment[], threadRootId?: number) => {
    const plain = freezeSend(channelId, channel, body, audience, files, threadRootId);
    if (plain === null) return;
    // The net under a whisper, as the composer shows it at the press: a 401
    // gives the text back with it (issue #1039).
    const under = audience?.kind === 'principals' && threadRootId === undefined ? targetFor(targetsNow.current, channel).net : null;
    const frozen: FrozenSend = under === null ? plain : { ...plain, netUnder: under };
    if (files.length === 0) {
      beginSend(frozen);
      return;
    }
    void (async () => {
      let attachments = [...frozen.attachments];
      for (let index = 0; index < attachments.length; index++) {
        const attachment = attachments[index];
        if (attachment === undefined || attachment.status === 'ready') continue;
        const uploaded = await uploadOne(frozen, attachment);
        attachments = [...attachments.slice(0, index), uploaded, ...attachments.slice(index + 1)];
        if (uploaded.status !== 'ready') return;
      }
      if (!activeSend(frozen) || attachments.some((attachment) => attachment.status !== 'ready')) return;
      const ready = { ...frozen, attachments, payload: attachmentPayload(attachments) };
      const name = nameOf(channelId) ?? channel;
      dispatchAttachments({ type: 'clear', channel: name, ...(frozen.audience === undefined ? {} : { audience: frozen.audience }) });
      beginSend(ready);
    })();
  }, [freezeSend, beginSend, uploadOne, activeSend, nameOf, dispatchAttachments]);

  const reconcile = useCallback((send: FrozenSend, localId: number, after: number) => {
    if (!activeSend(send)) return;
    const name = nameOf(send.channelId) ?? send.channel;
    dispatchChat({ type: 'reconciling', channel: name, localId, requestId: send.requestId });
    void api.loadMessagesAscending(send.channel, after).then((result) => {
      if (!activeSend(send)) return;
      const currentName = nameOf(send.channelId) ?? send.channel;
      const held = chatFor(chatNow.current, currentName);
      const pending = held.pending.find((p) => p.localId === localId && p.requestId === send.requestId);
      if (pending === undefined || pending.channelId !== send.channelId) return;
      if (!result.ok) {
        if (result.failure === 'unauthorized') dispatchAuth({ type: 'unauthorized' });
        else dispatchChat({ type: 'reconciliationFailed', channel: currentName, localId, requestId: send.requestId });
        return;
      }
      const found = result.value.find((message) =>
        !held.sentIds.has(message.id) && message.channel_id === send.channelId && message.author_id === send.principal && message.id > after && sameSend(pending, message),
      );
      if (found !== undefined) {
        markSent(send, localId, found);
        return;
      }
      if (!result.complete) {
        dispatchChat({ type: 'reconciliationFailed', channel: currentName, localId, requestId: send.requestId });
        return;
      }
      dispatchChat({ type: 'retrying', channel: currentName, localId, requestId: send.requestId });
      post(send, localId);
    });
  }, [activeSend, nameOf, dispatchChat, markSent, post]);

  const retrySend = useCallback((channelId: number, channel: string, localId: number) => {
    const pending = chatFor(chatNow.current, channel).pending.find((p) => p.localId === localId);
    if (pending === undefined || pending.status === 'sending' || pending.channelId !== channelId) return;
    const current = channelsNow.current.channels.find((c) => c.id === pending.channelId);
    if (current === undefined || current.name !== channel) return;
    const frozen = freezeSend(pending.channelId, current.name, pending.body, pending.audience, pending.attachments, pending.threadRootId);
    if (frozen === null) return;
    const send = { ...frozen, requestId: pending.requestId ?? frozen.requestId, ...(pending.payload === undefined ? {} : { payload: pending.payload }) };
    // A Retry is not the composer's draft: it never takes it (issue #1066).
    dispatchTargets({ type: 'posting', channel: current.name });
    if (pending.phase === 'unconfirmed') reconcile(send, localId, pending.reconcileAfter);
    else {
      dispatchChat({ type: 'retrying', channel: current.name, localId, requestId: send.requestId });
      post(send, localId);
    }
  }, [freezeSend, reconcile, dispatchChat, post]);

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

  // A visit's hard end (issue #713): when it comes, the page asks who it is,
  // and the server's 401 signs the guest out with its own words. The server
  // ends the visit whether or not the page is open; this only stops the page
  // from showing a visit that is over until its next request.
  const visitEnds = visit?.expires_at ?? null;
  useEffect(() => {
    if (visitEnds === null) return;
    const wait = Math.min(Math.max(0, Date.parse(visitEnds) - Date.now()) + 1_000, 2 ** 31 - 1);
    const timer = window.setTimeout(() => {
      void api.whoAmI().then((result) => {
        if (!result.ok && result.failure === 'unauthorized') dispatchAuth({ type: 'unauthorized' });
      });
    }, wait);
    return () => {
      window.clearTimeout(timer);
    };
  }, [visitEnds]);

  // Signed in (issue #714): drafts and pending sends kept from this person's
  // last sign-in come back; anyone else's are dropped. An interrupted POST is
  // unconfirmed, so its first retry reconciles before posting again. Pending
  // sends wait for the channel list and follow their frozen ids across renames.
  useEffect(() => {
    if (principalId === null || !channels.loaded) return;
    lastSignedIn.current = principalId;
    const was = kept.current;
    kept.current = null;
    if (was !== null && was.principal === principalId) {
      const pending: Record<string, PendingSend[]> = {};
      for (const [channel, sends] of Object.entries(was.pending)) {
        for (const send of sends) {
          const name = channelsNow.current.channels.find((c) => c.id === send.channelId)?.name ?? channel;
          pending[name] = [...(pending[name] ?? []), send];
        }
      }
      // Text goes to the channel it was typed in, by id, under its name now;
      // a channel that is gone takes its text with it.
      const typed: Record<string, string> = {};
      for (const [id, text] of Object.entries(was.drafts)) {
        const name = channelsNow.current.channels.find((c) => String(c.id) === id)?.name;
        if (name !== undefined) typed[name] = text;
      }
      dispatchDrafts({ type: 'restore', drafts: typed });
      // Images go to the channel they were added in, by id, under its name
      // now; a channel that is gone takes its images with it (issue #1067).
      dispatchAttachments({ type: 'restore', drafts: restoreAttachments(was.attachments, channelsNow.current.channels, attachmentsNow.current.generation) });
      dispatchChat({ type: 'restored', pending });
    }
    // An audience is never guessed: the box starts as a whisper to nobody,
    // which cannot be sent, until the channel's nets and members say the
    // audience still stands (below).
    const hold = (id: number, target: StoredTarget) => {
      const name = channelsNow.current.channels.find((c) => c.id === id)?.name;
      if (name === undefined) return;
      pendingTargets.current.set(id, target);
      dispatchTargets({ type: 'held', channel: name, line: checkingAudience });
    };
    if (was !== null && was.principal === principalId) {
      for (const [id, target] of Object.entries(was.targets)) hold(Number(id), target);
    }
    // The drafts this tab kept across a reload (issue #1039), by channel id so
    // a rename since does not matter. What was typed since wins. What belongs
    // to a channel the person can no longer open is dropped first.
    pruneStored(principalId, (id) => channelsNow.current.channels.some((c) => c.id === id));
    const stored = loadStored(principalId);
    const back: Record<string, string> = {};
    for (const [id, text] of Object.entries(stored.drafts)) {
      const name = channelsNow.current.channels.find((c) => String(c.id) === id)?.name;
      if (name !== undefined) back[name] = text;
    }
    if (Object.keys(back).length > 0) dispatchDrafts({ type: 'restore', drafts: back });
    for (const [id, target] of Object.entries(stored.targets)) hold(Number(id), target);
    setDraftsReady(true);
  }, [principalId, channels.loaded, dispatchAttachments, dispatchChat]);

  // Keep the drafts and audiences for a reload: only text and ids, only in
  // this tab (issue #1039).
  useEffect(() => {
    if (!draftsReady || principalId === null) return;
    const byId: Record<number, string> = {};
    const audiences: Record<number, StoredTarget> = {};
    for (const channel of channels.channels) {
      const text = drafts[channel.name];
      if (text !== undefined) byId[channel.id] = text;
      const audience = pendingTargets.current.get(channel.id) ?? storedAudience(targetFor(targets, channel.name));
      if (audience !== null) audiences[channel.id] = audience;
    }
    saveDrafts(principalId, byId, audiences);
  }, [drafts, targets, channels.channels, draftsReady, principalId]);

  // An audience kept across a reload is given back once the open channel's
  // nets and members are here and it still stands: the net is one the person
  // may transmit to, every recipient is still a member. Otherwise the text
  // stays, unsendable, until they choose who it goes to.
  useEffect(() => {
    if (channelId === null || selected === null) return;
    const kept = pendingTargets.current.get(channelId);
    if (kept === undefined || live.netsFor !== channelId || agents.membersFor !== channelId) return;
    pendingTargets.current.delete(channelId);
    // The whisper first, as the composer sends it. A whisper to nobody never
    // stands: it would leave the draft one Escape from the whole channel.
    const netStands = (net: number) => live.nets.has(net) && !live.monitored.has(net);
    if (kept.whisper === null) {
      if (kept.net !== null && netStands(kept.net)) dispatchTargets({ type: 'restore', channel: selected, net: kept.net, whisper: null });
      else dispatchTargets({ type: 'held', channel: selected, line: audienceGone });
      return;
    }
    if (!(kept.whisper.length > 0 && kept.whisper.every((id) => id !== principalId && live.people.has(id)))) {
      dispatchTargets({ type: 'held', channel: selected, line: audienceGone });
      return;
    }
    // The net under the whisper is checked on its own and kept only if it
    // stands, for the To: list and Send. Either way the draft comes back
    // protected, so Escape cannot take it anywhere.
    const under = kept.net !== null && netStands(kept.net) ? kept.net : null;
    dispatchTargets({ type: 'restore', channel: selected, net: under, whisper: kept.whisper });
  }, [channelId, selected, live.netsFor, live.nets, live.monitored, live.people, agents.membersFor, principalId]);

  // The audience controls (issue #1039): the person's own choice, in a channel
  // whose kept audience is still being checked, wins, and the kept one is
  // dropped, so the check can never add to what they chose. Escape is not a
  // choice: it cannot end a hold (the reducer keeps it), so it leaves the
  // check running too.
  const chooseTarget = useCallback((action: TargetAction) => {
    if (action.type === 'channel' || action.type === 'net' || action.type === 'whisper' || action.type === 'toggle') {
      const id = channelsNow.current.channels.find((c) => c.name === action.channel)?.id;
      if (id !== undefined) pendingTargets.current.delete(id);
    }
    dispatchTargets(action);
  }, []);

  // An operator's session ends at its twelve hours (issue #714, design
  // §5.3): when it comes, the page asks who it is, and the server's 401
  // signs out with the operator's own words, as a visit's hard end does. An
  // hour without use ends it too, which the next request learns.
  const sessionEnds = principal !== null && operatorSession(principal) ? (principal.auth?.expires_at ?? null) : null;
  useEffect(() => {
    if (sessionEnds === null) return;
    const wait = Math.min(Math.max(0, Date.parse(sessionEnds) - Date.now()) + 1_000, 2 ** 31 - 1);
    const timer = window.setTimeout(() => {
      void api.whoAmI().then((result) => {
        if (!result.ok && result.failure === 'unauthorized') dispatchAuth({ type: 'unauthorized' });
      });
    }, wait);
    return () => {
      window.clearTimeout(timer);
    };
  }, [sessionEnds]);

  // The recent proof (issue #714): a call answered reauth_required asks
  // here, over the page, and api.ts sends it again once the proof stands.
  // Whatever form asked stays as it was underneath. Leaving the lobby
  // answers a question still open with no.
  const [asking, setAsking] = useState(false);
  const answer = useRef<((proved: boolean) => void) | null>(null);
  const finishReauth = useCallback((proved: boolean) => {
    const resolve = answer.current;
    answer.current = null;
    setAsking(false);
    resolve?.(proved);
  }, []);
  useEffect(() => {
    if (!signedIn) return;
    api.onReauthRequired(
      () =>
        new Promise<boolean>((resolve) => {
          answer.current = resolve;
          setAsking(true);
        }),
    );
    return () => {
      api.onReauthRequired(null);
      finishReauth(false);
    };
  }, [signedIn, finishReauth]);

  // whoami read again after a change to the person's own account (a
  // password set or changed).
  const refreshWho = useCallback(() => {
    void api.whoAmI().then((result) => {
      if (result.ok) dispatchAuth({ type: 'refreshed', principal: result.value });
      else if (result.failure === 'unauthorized') dispatchAuth({ type: 'unauthorized' });
    });
  }, []);

  // End visit: the session is ended first, as for a sign-out, then the
  // visit. Only the server's word that it ended signs the guest out here.
  const endVisit = useCallback(async (): Promise<'ended' | 'unconfirmed'> => {
    if (signingOut.current) return 'unconfirmed';
    signingOut.current = true;
    await releaseSession();
    const outcome = await api.endVisit();
    signingOut.current = false;
    if (outcome === 'unconfirmed') return outcome;
    // unsent: this tab no longer held the credential, so it told the server
    // nothing, and says so.
    forgetChannel();
    dispatchAuth({ type: 'signOut', notice: outcome === 'ended' ? 'visitEnded' : 'visitUntold' });
    return 'ended';
  }, [releaseSession]);

  // Forget the visit in this tab after an End visit the server did not
  // confirm: the sign-out tries once more to tell the server.
  const forgetVisit = useCallback(() => {
    if (signingOut.current) return;
    signingOut.current = true;
    void api.signOut().then((told) => {
      signingOut.current = false;
      forgetChannel();
      dispatchAuth({ type: 'signOut', notice: told ? 'visitEnded' : 'visitUntold' });
    });
  }, []);

  // Create an account during the visit. Made, the page starts over as the
  // member; a visit that is over signs out with the way back in; anything
  // else is the guest panel's to say, and the guest is as it was.
  const convert = useCallback(
    async (name: string, password: string): Promise<api.ConvertResult> => {
      const result = await api.convertGuest(name, password);
      if (result.ok) {
        onRestart();
      } else if (result.failure === 'gone' || result.failure === 'unkept') {
        forgetChannel();
        dispatchAuth({ type: 'signOut', notice: result.failure === 'gone' ? 'convertGone' : 'convertUnkept' });
      }
      return result;
    },
    [onRestart],
  );

  // What of the invite a person signed in instead of using is not theirs.
  const missing =
    destination === null || !signedIn || !channels.loaded ? [] : destination.channels.filter((name) => !channels.channels.some((c) => c.name === name));

  // The code of an invite link (issue #440) or a sign-in link (issue #493),
  // held here in memory only: never stored, and dropped once it is used or
  // the person goes back. It is taken out of the address before the first
  // paint, and again when a link is pasted into a tab that is already open (a
  // hashchange, no reload). The newest link wins.
  const [inviteCode, setInviteCode] = useState<string | null>(null);
  const [signInCode, setSignInCode] = useState<string | null>(null);
  // An operator's setup from a host-made token (issue #714) is a page of its
  // own, reached from the sign-in page, or from a setup link (ADR-011), whose
  // token waits in a ref (never in state) until Setup puts it in its field.
  const [settingUp, setSettingUp] = useState(false);
  const setupLink = useRef<string | null>(null);
  // Counts the setup links taken (a number, not a secret): Setup is keyed by
  // it, so each link gets a fresh page with its own token in the field.
  const [setupGeneration, setSetupGeneration] = useState(0);
  // Whether someone is signed in, for take() below, which outlives renders.
  const signedInNow = useRef(false);
  signedInNow.current = auth.phase === 'signedIn' || auth.phase === 'operatorWarning';
  // A setup link is for a signed-out browser. One that waits while someone
  // is signed in (or that sign-in restored under) is dropped, so a stale
  // token is never pre-filled after they sign out.
  useEffect(() => {
    if (auth.phase !== 'signedIn' && auth.phase !== 'operatorWarning') return;
    setupLink.current = null;
    setSettingUp(false);
  }, [auth.phase]);
  useLayoutEffect(() => {
    function take() {
      const found = takeLinkFromAddress();
      if (found?.kind === 'invite') {
        setSignInCode(null);
        setInviteCode(found.inviteCode);
      } else if (found?.kind === 'signin') {
        setInviteCode(null);
        setSignInCode(found.signInCode);
      } else if (found?.kind === 'setup') {
        setInviteCode(null);
        setSignInCode(null);
        if (signedInNow.current) {
          // Not used: sign out first, then open the link again.
          setupLink.current = null;
          return;
        }
        setupLink.current = found.setupToken;
        setSetupGeneration((n) => n + 1);
        setSettingUp(true);
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

  // A returning member signs in with a name and a password (issue #713).
  const submitPassword = useCallback((name: string, password: string) => {
    dispatchAuth({ type: 'submit' });
    void api.signInWithPassword(name, password).then((result) => {
      if (result.ok) {
        dispatchAuth({ type: 'verified', principal: result.value, own: true });
        return;
      }
      switch (result.failure) {
        case 'stored':
          dispatchAuth({ type: 'recheck' });
          return;
        case 'held':
          dispatchAuth({ type: 'failed', failure: 'held' });
          return;
        case 'unchecked':
          void api.whoAmI().then((who) => {
            if (who.ok) dispatchAuth({ type: 'verified', principal: who.value, own: true });
            else dispatchAuth({ type: 'failed', failure: who.failure });
          });
          return;
        case 'unkept':
          dispatchAuth({ type: 'refused', line: 'This browser will not let the page keep your sign-in. Allow this site to keep data, then sign in again.' });
          return;
        case 'totp':
          // A right password for an account that needs a code: nobody is
          // signed in yet (issue #714).
          dispatchAuth({ type: 'challenged', name, until: result.expiresAt });
          return;
        default:
          dispatchAuth({ type: 'refused', line: passwordRefusal(result) });
      }
    });
  }, []);

  // An operator's code, the second step (issue #714). A code that is not
  // right leaves the step open for the next; a step out of time or of tries
  // goes back to the password.
  const submitCode = useCallback((code: string) => {
    dispatchAuth({ type: 'codeSent' });
    void api.signInWithCode(code).then((result) => {
      if (result.ok) {
        dispatchAuth({ type: 'verified', principal: result.value });
        return;
      }
      switch (result.failure) {
        case 'over':
          dispatchAuth({ type: 'signOut', notice: 'codeOver' });
          return;
        case 'held':
          dispatchAuth({ type: 'failed', failure: 'held' });
          return;
        case 'unchecked':
          void api.whoAmI().then((who) => {
            if (who.ok) dispatchAuth({ type: 'verified', principal: who.value, own: true });
            else dispatchAuth({ type: 'failed', failure: who.failure });
          });
          return;
        case 'unkept':
          dispatchAuth({ type: 'refused', line: 'This browser will not let the page keep your sign-in. Allow this site to keep data, then sign in again.' });
          return;
        default:
          dispatchAuth({ type: 'codeRefused', line: codeRefusal(result) });
      }
    });
  }, []);

  const codeOver = useCallback((over: boolean) => {
    api.cancelCodeSignIn();
    dispatchAuth({ type: 'signOut', notice: over ? 'codeOver' : null });
  }, []);

  // Forgetting a stored sign-in that could not be checked (issue #469): it is
  // revoked if the server answers, then dropped, and signing in is open again.
  const forgetStored = useCallback(() => {
    void api.forgetStored().then((told) => {
      dispatchAuth({ type: 'signOut', notice: told ? null : 'untold' });
    });
  }, []);

  const previousOrigin = useRef(origin);
  useEffect(() => {
    if (previousOrigin.current === origin) return;
    previousOrigin.current = origin;
    for (const upload of uploadControllers.current.values()) upload.controller.abort();
    uploadControllers.current.clear();
    dispatchAttachments({ type: 'reset' });
  }, [origin, dispatchAttachments]);

  const composerTarget: Target | null = selected === null ? null : targetFor(targets, selected);
  const composerAudience = composerTarget === null ? undefined : audienceForTarget(composerTarget);
  // Every change to the draft's images makes it a newer draft than any send
  // on its way (issue #1066), from the moment one is chosen, pasted or dropped.
  const editedImages = () => {
    if (selected !== null) dispatchTargets({ type: 'edited', channel: selected });
  };
  const composerAttachments: ComposerAttachmentContextValue = {
    attachments: selected === null ? [] : attachmentsFor(attachmentDrafts, selected, composerAudience),
    editing: editedImages,
    add: (files: readonly NewDraftAttachment[]) => {
      if (selected === null || channelId === null) return;
      editedImages();
      dispatchAttachments({ type: 'add', channel: selected, ...(composerAudience === undefined ? {} : { audience: composerAudience }), files });
    },
    remove: (clientId: number) => {
      if (selected === null) return;
      editedImages();
      dispatchAttachments({ type: 'remove', channel: selected, ...(composerAudience === undefined ? {} : { audience: composerAudience }), clientId });
    },
    cancel: (clientId: number) => {
      if (selected === null) return;
      const attachment = attachmentsFor(attachmentsNow.current, selected, composerAudience).find((a) => a.clientId === clientId);
      if (attachment === undefined) return;
      editedImages();
      uploadControllers.current.get(`${String(attachmentsNow.current.generation)}:${String(clientId)}`)?.controller.abort();
      dispatchAttachments({
        type: 'cancelled',
        channel: selected,
        ...(composerAudience === undefined ? {} : { audience: composerAudience }),
        clientId,
        generation: attachmentsNow.current.generation,
        attempt: attachment.attempt,
      });
    },
    retry: (clientId: number) => {
      if (selected === null || channelId === null) return;
      const attachment = attachmentsFor(attachmentsNow.current, selected, composerAudience).find((a) => a.clientId === clientId);
      if (attachment === undefined || attachment.status === 'ready' || attachment.status === 'uploading') return;
      editedImages();
      const frozen = freezeSend(channelId, selected, draftFor(draftsNow.current, selected), composerAudience, [attachment]);
      if (frozen !== null) void uploadOne(frozen, attachment);
    },
  };

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
        onSignIn={(inviteChannels, inviter) => {
          setInviteCode(null);
          setDestination({ channels: inviteChannels, inviter });
        }}
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

  if (settingUp && (auth.phase === 'signedOut' || auth.phase === 'checking')) {
    return (
      <Setup
        key={setupGeneration}
        origin={origin}
        link={setupLink}
        onDone={(skipped) => {
          setSettingUp(false);
          dispatchAuth({ type: 'signOut', notice: skipped ? 'setupSkipped' : 'setupDone' });
        }}
        onBack={() => {
          setSettingUp(false);
        }}
      />
    );
  }

  switch (auth.phase) {
    case 'code':
      return (
        <SignIn
          origin={origin}
          notice={null}
          checking={false}
          code={{ name: auth.name, until: auth.until, checking: auth.checking, ...(auth.line === undefined ? {} : { line: auth.line }) }}
          onSubmit={submit}
          onPassword={submitPassword}
          onCode={submitCode}
          onCodeOver={codeOver}
          onForget={forgetStored}
          onJoined={(who) => {
            joined(who, undefined);
          }}
        />
      );
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
          {...(auth.phase === 'signedOut' && auth.line !== undefined ? { line: auth.line } : {})}
          checking={auth.phase === 'checking'}
          onSubmit={submit}
          onPassword={submitPassword}
          onForget={forgetStored}
          onJoined={(who) => {
            joined(who, undefined);
          }}
          onSetup={() => {
            setSettingUp(true);
          }}
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
        <ComposerAttachmentProvider value={composerAttachments}>
        <Lobby
          origin={origin}
          name={shownName}
          me={auth.principal.id}
          temporary={auth.principal.server_mode === 'temporary'}
          passwordSet={auth.principal.auth?.password_set === true}
          banner={
            <>
              {visit === null ? null : <GuestVisit guest={visit} insecure={needsHttps(origin)} onConvert={convert} onEnd={endVisit} onForget={forgetVisit} />}
              {missing.length === 0 || destination === null ? null : (
                <div role="status" className="banner destination">
                  <p>
                    Signing in did not use the invite from {destination.inviter}: this account is not in {channelWords(missing)}.
                    Ask {destination.inviter} to add you.
                  </p>
                  <button
                    type="button"
                    onClick={() => {
                      setDestination(null);
                    }}
                  >
                    Dismiss
                  </button>
                </div>
              )}
            </>
          }
          {...(visit === null
            ? {}
            : {
                visit: (
                  <p className="hint">
                    You are visiting as a temporary guest. To leave, use End visit at the top of the conversation: closing the
                    tab does not end the visit.
                  </p>
                ),
              })}
          channels={channels}
          unread={unread}
          occupancy={occupancy}
          notificationCenter={<NotificationCenter alerts={notifications.alerts} onOpen={notifications.openAlert} onClear={notifications.clearAlerts} />}
          connection={connection}
          panel={
            <MembersPanel
              agents={agents}
              now={now}
              controls={
                currentChannel === null
                  ? null
                  : {
                      channel: currentChannel,
                      channels: channels.channels,
                      viewer: { role: auth.principal.role, guest: visit !== null },
                      onUnauthorized,
                    }
              }
            />
          }
          account={
            <>
              <div className="standing">
                <h3>Who can administer</h3>
                <p>{administrationLine(auth.principal, channels.channels, visit !== null)}</p>
              </div>
              <NotificationSettings
                policy={notifications.policy}
                native={notifications.native}
                permission={notifications.browserPermission}
                supported={notifications.browserSupported}
                channels={channels.channels}
                muted={notifications.muted}
                onToggleMute={notifications.toggleMute}
                onPolicy={notifications.setPolicy}
                onEnable={() => {
                  void notifications.enableNative();
                }}
              />
              {/* A person renames themselves; an agent's name is an operator's to
                  set, and a guest's is its handle (issue #713). */}
              {auth.principal.kind === 'human' && visit === null ? (
                <>
                  <ChangeName
                    id={auth.principal.id}
                    name={auth.principal.name}
                    onRenamed={(p) => {
                      dispatchAuth({ type: 'renamed', name: p.name });
                    }}
                    onUnauthorized={onUnauthorized}
                  />
                  {auth.principal.role === 'operator' && !operatorSession(auth.principal) ? (
                    <p className="hint">
                      This browser uses a script’s credential, not your sign-in: no password or code was checked, and it does
                      not end after an hour without use. Sign out, and sign in with your name, password and a code.
                    </p>
                  ) : null}
                  {/* A server that predates passwords says nothing of them. */}
                  {auth.principal.auth === undefined ? null : (
                    <PasswordSettings who={auth.principal} insecure={needsHttps(origin)} onChanged={refreshWho} onUnauthorized={onUnauthorized} />
                  )}
                </>
              ) : null}
            </>
          }
          {...(visit === null
            ? {
                signIns: (endThis: () => void) => (
                  <YourSignIns
                    onEndThis={endThis}
                    onUnauthorized={onUnauthorized}
                    // Add a device is a member's alone (design §6.2): an
                    // operator signs in elsewhere with a password and a code.
                    addDevice={
                      auth.principal.kind === 'human' && auth.principal.role === 'member' && auth.principal.auth !== undefined ? (
                        <AddDevice origin={origin} insecure={needsHttps(origin)} onUnauthorized={onUnauthorized} />
                      ) : null
                    }
                  />
                ),
              }
            : {})}
          admin={auth.principal.role === 'operator' ? <GuestSettings channels={channels.channels} onUnauthorized={onUnauthorized} /> : null}
          overlay={
            asking ? (
              <Reauth operator={auth.principal.role === 'operator' && auth.principal.auth?.totp_enrolled === true} insecure={needsHttps(origin)} onDone={finishReauth} onUnauthorized={onUnauthorized} />
            ) : null
          }
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
          renameChannel={
            // Operators and this channel's managers may rename. Keyed by id:
            // opening another channel starts the form over, and a rename of
            // this one keeps it.
            visit === null && (auth.principal.role === 'operator' || currentChannel?.manages === true) && channelId !== null && selected !== null ? (
              <RenameChannel
                key={channelId}
                channel={{ id: channelId, name: selected }}
                onRenamed={(channel) => {
                  renamed(channel.id, channel.name);
                }}
                onStale={refind}
                onUnauthorized={onUnauthorized}
              />
            ) : null
          }
          newChannel={
            // The server allows operators alone; anyone else is not offered it.
            auth.principal.role === 'operator' ? (
              <CreateChannel
                asks={createAsks}
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
          firstRun={
            // An operator on a server with no channels is shown what to do
            // first; anyone else keeps the channel list's own line.
            channels.loaded && channels.channels.length === 0 && auth.principal.role === 'operator' && visit === null ? (
              <FirstRun
                onMakeChannel={() => {
                  setCreateAsks((n) => n + 1);
                }}
              />
            ) : null
          }
          conversation={
            selected === null || channelId === null ? null : (
              <>
                {agents.state.detail === null ? null : <RunDetail agents={agents} now={now} onGoToMessage={goToMessage} />}
                {!searchOpen ? null : (
                  <SearchPanel
                    key={channelId}
                    channelId={channelId}
                    channel={selected}
                    live={live.status}
                    people={live.people}
                    nets={live.nets}
                    viewer={auth.principal.id}
                    onMissing={onMissing}
                    onUnauthorized={onUnauthorized}
                    onClose={() => {
                      setSearchOpen(false);
                    }}
                  />
                )}
                <div className="conversation-body" hidden={agents.state.detail !== null || searchOpen}>
              <Conversation
                key={channelId}
                historyBounded={agents.membersFor === channelId && (agents.members.find((m) => m.principal_id === auth.principal.id)?.history_after ?? 0) > 0}
                channelId={channelId}
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
                  // Whatever is typed or inserted now is a newer draft than
                  // any send on its way (issue #1066).
                  dispatchTargets({ type: 'edited', channel: selected });
                }}
                onTarget={chooseTarget}
                onSend={(body, audience, files?: readonly DraftAttachment[]) => {
                  send(channelId, selected, body, audience, files ?? []);
                }}
                onReplySend={(body, root, audience) => {
                  send(channelId, selected, body, audience, [], root);
                }}
                onRetrySend={(localId) => {
                  retrySend(channelId, selected, localId);
                }}
                onDismissSend={(localId) => {
                  const pending = chatFor(chatNow.current, selected).pending.find((p) => p.localId === localId);
                  dispatchChat({ type: 'discard', channel: selected, localId, ...(pending?.requestId === undefined ? {} : { requestId: pending.requestId }) });
                }}
                onEdited={edited}
                onEditFinal={editFinal}
                onLoadOlder={live.loadOlder}
                onReload={live.retry}
                onSearch={() => {
                  setSearchOpen(true);
                }}
                onUnauthorized={onUnauthorized}
                onBottom={(atBottom) => {
                  setBottom((prev) => (prev.channel === channelId && prev.atBottom === atBottom ? prev : { channel: channelId, atBottom }));
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
        </ComposerAttachmentProvider>
      );
  }
}

function audienceForTarget(target: Target): Audience | undefined {
  if (target.whisper !== null) return { kind: 'principals', principal_ids: [...target.whisper] };
  if (target.net !== null) return { kind: 'net', net_id: target.net };
  return undefined;
}

// storedAudienceOf is a send's audience as it is kept when a 401 gives its
// text back, with the net that was under its whisper: null for the whole
// channel. It came back, so it is protected.
function storedAudienceOf(audience: Audience | undefined, netUnder: number | undefined): StoredTarget | null {
  if (audience === undefined) return null;
  if (audience.kind === 'principals') return { net: netUnder ?? null, whisper: [...audience.principal_ids], protected: true };
  return { net: audience.net_id, whisper: null, protected: true };
}

function audienceForScope(scope: string): Audience | undefined {
  if (scope === 'channel') return undefined;
  if (scope.startsWith('net:')) return { kind: 'net', net_id: Number(scope.slice(4)) };
  return { kind: 'principals', principal_ids: scope.slice(11).split(',').map(Number) };
}

interface FrozenSend {
  readonly origin: string;
  readonly principal: number;
  readonly channelId: number;
  readonly channel: string;
  readonly channelEpoch: number;
  readonly generation: number;
  readonly requestId: string;
  readonly body: string;
  readonly audience?: Audience;
  // The net under a whisper when it was sent from the composer (issue #1039).
  readonly netUnder?: number;
  readonly threadRootId?: number;
  readonly attachments: readonly DraftAttachment[];
  readonly payload?: ReturnType<typeof attachmentPayload>;
}

function administrationLine(who: WhoAmIResponseV1, channels: readonly { readonly name: string; readonly manages?: boolean }[], guest: boolean): string {
  if (who.role === 'operator' && who.owner === true) return 'You are the owner.';
  if (who.role === 'operator') return 'You are an operator. Only the owner changes roles or acts on the owner.';
  if (guest) return 'You are a guest; a visit cannot hold any right.';
  const managed = channels.filter((channel) => channel.manages === true).map((channel) => channel.name);
  if (managed.length > 0) return `You manage ${channelWords(managed)}.`;
  return 'You are a member.';
}

// passwordRefusal is the line shown for a refused password sign-in. Every
// wrong name or password is the server's one answer, and the page's too.
function passwordRefusal(result: api.InviteRefusal): string {
  switch (result.code) {
    case 'auth_failed':
    case 'invalid_request':
      return 'The name or password is not right.';
    case 'rate_limited':
      return `Too many tries. Try again in ${waitWords(result.retryAfter)}.`;
    default:
      return refusalLine(result);
  }
}

// codeRefusal is the line shown for a refused code, while the step is open.
function codeRefusal(result: api.InviteRefusal): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try the code again.';
  switch (result.code) {
    case 'auth_failed':
    case 'invalid_request':
      return 'That code is not right. Type the code the app shows now.';
    case 'rate_limited':
      return `Too many tries. Try again in ${waitWords(result.retryAfter)}.`;
    default:
      return refusalLine(result);
  }
}

// everywhereFailure is the line shown when sign out everywhere did not happen.
function everywhereFailure(result: { readonly failure: api.ApiFailure; readonly code?: string }): string {
  if (result.failure === 'network') return 'Could not sign out everywhere: the server cannot be reached. You are still signed in; try again.';
  if (result.code === 'last_operator') return 'Could not sign out everywhere: you are the only operator who can sign in.';
  return 'Could not sign out everywhere: the server refused. You are still signed in.';
}
