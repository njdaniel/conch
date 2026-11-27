import { useCallback, useContext, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { ChannelList } from './ChannelList';
import { ChannelNav } from './ChannelNav';
import { GroupsPanel } from './ChannelGroups';
import { visualOrder } from './arrangement';
import { useArrangement } from './useArrangement';
import type { SettingsChange } from './ChannelSettingsAsk';
import type { InVoice } from './voice/useOccupancy';
import { ChannelJoin, type ChannelJoinHandler } from './voice/channelJoin';
import { headingId } from './Conversation';
import { Avatar, Glyph } from './Icons';
import { ServerVersion, TemporaryBanner } from './ServerVersion';
import { AddPerson, JoiningSettings, SettingsOpen, type JoiningRequest } from './joiningSettings';
import { DrawerOver, LeaveDrawer, OverlayReport, setPageModal } from './pageModal';
import { MicTestShown, phoneViews, ShowChat, useAppHeight, usePhone, VoiceLive, VoiceSheet, VoiceStrip, type PhoneView } from './phone';
import { DesktopOS } from './platform/os';
import type { ChannelV0 } from './schema/channel';
import { StageHost } from './stage';
import type { ChannelsState } from './state/channels';
import { connectionLabel, type ConnectionState } from './state/connection';

// At this width and wider the members panel is a column beside the
// conversation; below it, a panel drawn over the page (issue #536). The same
// width is in app.css.
const wide = '(min-width: 1100px)';

function isWide(): boolean {
  return typeof window.matchMedia !== 'function' || window.matchMedia(wide).matches;
}

// useWide follows the window across the breakpoint. Only classes and focus
// follow it: nothing is mounted or unmounted, so a call or a share carries on.
function useWide(): boolean {
  const [wideNow, setWide] = useState(isWide);
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const query = window.matchMedia(wide);
    const change = () => {
      setWide(query.matches);
    };
    query.addEventListener('change', change);
    return () => {
      query.removeEventListener('change', change);
    };
  }, []);
  return wideNow;
}

// What signing out costs, said before it is done (issue #493). A person
// with no password (one who joined before passwords, or was made by an
// operator) can only be let back into this account by an operator; one with
// a password signs in again with it (issue #713).
export const signOutWarning = 'To come back into this account, you will need a new link from an operator.';
export const passwordSignOutWarning = 'To come back into this account, sign in again with your name and password.';
function signOutQuestion(which: 'here' | 'everywhere', passwordSet: boolean, desktop: boolean): string {
  const warning = passwordSet ? passwordSignOutWarning : signOutWarning;
  const place = desktop ? 'this app' : 'this browser';
  return which === 'here' ? `Sign out of ${place}? ${warning}` : `Sign out on every device, ${place} included? ${warning}`;
}

// The lobby: a top bar (who, which server, the connection, Invite, Members,
// Settings), channels on the left, the open channel in the middle, the
// members panel on the right, and the voice strip across the bottom. The
// middle is the conversation the caller gives it; the panel is the caller's
// too (who is here and what is running).
export function Lobby({
  origin,
  name,
  me = 0,
  temporary,
  channels,
  unread,
  occupancy,
  voiceNames,
  notificationCenter,
  connection,
  conversation,
  firstRun,
  panel,
  voice,
  invites,
  newChannel,
  canRename,
  renderRename,
  canSetTopic,
  renderTopic,
  canManage,
  renderSettings,
  onInviteTo,
  onLeft,
  onUnauthorized,
  onAddPerson,
  account,
  appearance,
  signIns,
  report,
  onOpen,
  onBrowse,
  directoryTick = 0,
  onRetry,
  onSignOut,
  onSignOutEverywhere,
  signOutProblem,
  passwordSet = false,
  banner,
  band,
  visit,
  admin,
  onSettingsOpen,
  overlay,
}: {
  readonly origin: string;
  readonly name: string;
  // The signed-in principal's id, for the colour of their initial.
  readonly me?: number;
  // Whoami said this is a temporary server (ADR-011): the banner shows.
  readonly temporary?: boolean;
  readonly channels: ChannelsState;
  readonly unread: ReadonlyMap<string, number>;
  // Who is in each channel's voice (issues #596, #1103), shown in the list,
  // and the names the page has for them by principal id.
  readonly occupancy?: InVoice;
  readonly voiceNames?: ReadonlyMap<number, string>;
  readonly notificationCenter?: ReactNode;
  readonly connection: ConnectionState;
  readonly conversation: ReactNode;
  // The first-run panel for an operator with no channels (issue #1032). It
  // stands where the conversation would; on a phone, where that is not shown
  // with nothing open, it is drawn in the Channels view instead. Never both.
  readonly firstRun?: ReactNode;
  readonly panel: ReactNode;
  // The voice section: kept by the caller, which owns the voice client. It is
  // rendered in one place only, the strip, whatever the window's width.
  readonly voice: ReactNode;
  // "Invite someone" (issue #440), or null for a person who may not invite.
  readonly invites: ReactNode;
  // "New channel" (issue #695), under the channel list, or null for a person
  // who may not make one.
  readonly newChannel?: ReactNode;
  // Per-channel actions in each row's ⋯ menu (issues #1135, #1095): who may
  // rename or set the topic of which channel, their forms, the opener of the
  // invite panel, and the channel-removal handler after a Leave succeeds.
  readonly canRename?: (channel: ChannelV0) => boolean;
  readonly renderRename?: (channel: ChannelV0, close: () => void) => ReactNode;
  // Whether this viewer may set that channel's topic (an operator, or its
  // manager); absent, nobody. The server still decides.
  readonly canSetTopic?: (channel: ChannelV0) => boolean;
  // The set-topic form for a channel, shown in place under its row; `close`
  // hides it and returns focus to the row's ⋯.
  readonly renderTopic?: (channel: ChannelV0, close: () => void) => ReactNode;
  readonly canManage?: boolean;
  readonly renderSettings?: (channel: ChannelV0, change: SettingsChange, close: () => void) => ReactNode;
  readonly onInviteTo?: (channel: ChannelV0) => void;
  readonly onLeft?: (name: string) => void;
  readonly onUnauthorized?: () => void;
  // The console's Members section asks for the member-link panel (issue
  // #1020, "Add a person"): Settings closes, then the page opens it.
  readonly onAddPerson?: () => void;
  // The caller's standing and own account settings, in Settings.
  readonly account?: ReactNode;
  readonly appearance?: ReactNode;
  // "Your sign-ins" (issue #496), in Settings. Ending this browser's own
  // sign-in is signing out, so the section is handed endThis, which asks the
  // sign-out's question.
  readonly signIns?: (endThis: () => void) => ReactNode;
  // "Report a problem" (issue #446): in Settings beside the sign-outs, and
  // again where the page says it cannot reach the server.
  readonly report?: ReactNode;
  readonly onOpen: (channel: string) => void;
  // The quick switcher's "Join" rows: show that public channel's directory row.
  // Absent for a viewer with no directory.
  readonly onBrowse?: (channel: string) => void;
  // Changes when the directory or the viewer's permissions did (issue #1492).
  readonly directoryTick?: number;
  readonly onRetry: () => void;
  readonly onSignOut: () => void;
  // Sign out everywhere (issue #439), and why it last failed, if it did.
  readonly onSignOutEverywhere: () => void;
  readonly signOutProblem: string | null;
  // Whether the person has a password to sign in again with (whoami's
  // auth.password_set), which changes what signing out is said to cost.
  readonly passwordSet?: boolean;
  // Shown above the conversation: a guest's visit, or what signing in from
  // an invite did not do (issue #713).
  readonly banner?: ReactNode;
  // A band across the page under the top bar, on every phone view (Preview as
  // member, issue #1517); the page has none otherwise.
  readonly band?: ReactNode;
  // For a temporary guest, what Settings says in place of the sign-outs:
  // a visit ends with End visit, in the banner.
  readonly visit?: ReactNode;
  // The server's own settings, for an operator (issue #714): joining and
  // guests, in Settings.
  readonly admin?: ReactNode;
  // Called each time Settings opens, so what it shows from whoami is fresh.
  readonly onSettingsOpen?: () => void;
  // A question over the whole page, Settings included (issue #714: a recent
  // proof of the password). While it is shown everything else is inert and
  // stays as it was, so what was being typed is kept.
  readonly overlay?: ReactNode;
}) {
  const desktop = useContext(DesktopOS) !== null;
  const joinVoice = useRef<ChannelJoinHandler | null>(null);
  const [panelOpen, setPanelOpen] = useState(isWide);
  const wideNow = useWide();
  // A column left open would cover the page once the window narrows: it
  // closes, and Members opens it again.
  const [wasWide, setWasWide] = useState(wideNow);
  if (wasWide !== wideNow) {
    setWasWide(wideNow);
    if (!wideNow) setPanelOpen(false);
  }
  // The phone layout (issue #929): below 700 px one of three views is drawn at
  // a time. All of them stay mounted, so a switch only changes what is shown:
  // the draft, the log's place and the call are untouched.
  const phone = usePhone();
  // The person's own favorites, groups and order (issue #1493).
  const arranged = useArrangement(origin, me, channels.channels, channels.loaded);
  const inVisualOrder = useMemo(() => visualOrder(arranged.arrangement, channels.channels), [arranged.arrangement, channels.channels]);
  useAppHeight(phone);
  // Until a view is chosen: Chat once a channel is open, Channels while the
  // list is loading, failed to load, or has nothing open to chat in.
  const [chosen, setView] = useState<PhoneView | null>(null);
  // Chat with nothing open (the channel was lost or closed on this tab) is
  // Channels.
  const chatOpen = channels.loaded && channels.selected !== null;
  const wanted: PhoneView = chosen ?? 'chat';
  const view: PhoneView = wanted === 'chat' && !chatOpen ? 'channels' : wanted;
  const [live, setLive] = useState(false);
  // Leaving voice under Chat or Channels takes the strip off the screen
  // (Join voice was focused in it): focus goes to the Voice button, where Join
  // voice now is, rather than to the page's top. Only focus that was in the
  // strip, or nowhere.
  const dock = useRef<HTMLDivElement>(null);
  const voiceTab = useRef<HTMLButtonElement>(null);
  const channelsHeading = useRef<HTMLHeadingElement>(null);
  // The strip's state button opens the Voice view (issue #1028). It is not
  // drawn there, so focus goes to the Voice button, now the current view's.
  const openVoice = useCallback(() => {
    voiceTab.current?.focus();
    setView('voice');
  }, []);
  // The conversation's height while a view that shows it (or leaves it laid out
  // at the same size) is current, kept as --chat-height on the lobby for the
  // Voice view, which lays it out hidden behind. Resizing it there would clamp
  // the log's scroll position, and a reader a few pixels above the end would
  // come back to somewhere else. Set through the CSSOM (no inline style).
  const lobbyEl = useRef<HTMLDivElement>(null);
  const conversationEl = useRef<HTMLElement>(null);
  useEffect(() => {
    const box = lobbyEl.current;
    const el = conversationEl.current;
    if (!phone || view === 'voice' || box === null || el === null || typeof ResizeObserver !== 'function') return;
    const watch = new ResizeObserver(() => {
      box.style.setProperty('--chat-height', `${String(el.getBoundingClientRect().height)}px`);
    });
    watch.observe(el);
    return () => {
      watch.disconnect();
    };
  }, [phone, view]);
  const wasLive = useRef(false);
  useEffect(() => {
    const left = wasLive.current && !live;
    wasLive.current = live;
    if (!left || !phone || view === 'voice') return;
    const at = document.activeElement;
    if (at === null || at === document.body || dock.current?.contains(at) === true) voiceTab.current?.focus();
  }, [phone, live, view]);
  // Choosing a channel on a phone returns to Chat, and focus follows the page
  // to the conversation's heading (the list it was on is no longer shown).
  const toHeading = useRef(false);
  useEffect(() => {
    if (view !== 'chat' || !toHeading.current) return;
    toHeading.current = false;
    document.getElementById(headingId)?.focus();
  }, [view]);
  const [settingsOpen, setSettingsOpen] = useState(false);
  // Settings with Joining expanded, asked for from the invite panel
  // (issue #909).
  const [joiningAsks, setJoiningAsks] = useState(0);
  // Opened from there, the section takes focus, not the heading, so the
  // switch is in view.
  const toJoining = useRef(false);
  const joining = useMemo<JoiningRequest>(
    () => ({
      count: joiningAsks,
      open: () => {
        toJoining.current = true;
        setJoiningAsks((n) => n + 1);
        setSettingsOpen(true);
      },
    }),
    [joiningAsks],
  );
  const asking = overlay !== undefined && overlay !== null;
  // The members panel is drawn over the page below the wide breakpoint, with a
  // backdrop, and then it is a modal drawer like Settings: focus goes into it,
  // the rest of the page is inert, and focus goes back to Members after. As a
  // side column (wide) it is none of that.
  const membersOver = panelOpen && !wideNow;
  // The voice options sheet is open (VoiceSheet): the page behind it is inert
  // with the same attribute as Settings and re-authentication, in one
  // expression each. Re-authentication outranks it: the sheet is told it is
  // blocked and closes; the dock, the nav and the overlay are never made inert
  // for the sheet.
  const [sheetOpen, setSheetOpen] = useState(false);
  const voiceSheet = sheetOpen && !asking;
  // Modals drawn outside the lobby's own (the quick switcher, in the body) say
  // they are open through OverlayReport; they are counted here.
  const [overlays, setOverlays] = useState(0);
  const reportOverlay = useCallback((open: boolean) => {
    setOverlays((n) => Math.max(0, n + (open ? 1 : -1)));
  }, []);
  // Settings, re-authentication or one of those is up over the sheet: told to
  // it as behind, so it takes focus back when the modal over it goes.
  const behind = settingsOpen || asking || membersOver || overlays > 0;
  const sheetLink = useMemo(() => ({ report: setSheetOpen, blocked: asking, behind }), [asking, behind]);
  // What the lobby does not draw but the page does (the desktop strip, the
  // update notices) is inert for the same modals: said here, once, to them.
  const modalNow = settingsOpen || asking || voiceSheet || membersOver;
  useEffect(() => {
    setPageModal(modalNow);
    return () => {
      setPageModal(false);
    };
  }, [modalNow]);
  // Whether the sheet is the modal that is left when Settings closes.
  const sheetModal = useRef(false);
  useEffect(() => {
    sheetModal.current = voiceSheet;
  }, [voiceSheet]);
  // Which sign-out is waiting for its yes, if one is.
  const [confirming, setConfirming] = useState<'here' | 'everywhere' | null>(null);
  const [stage, setStage] = useState<HTMLElement | null>(null);
  const cancel = useRef<HTMLButtonElement>(null);
  const membersButton = useRef<HTMLButtonElement>(null);
  const membersBody = useRef<HTMLDivElement>(null);
  const settingsButton = useRef<HTMLButtonElement>(null);
  const settingsHeading = useRef<HTMLHeadingElement>(null);
  // The question takes focus on its safe answer, so Enter keeps the account.
  useEffect(() => {
    if (confirming !== null) cancel.current?.focus();
  }, [confirming]);
  // Focus in the members column when the window narrows would be lost as the
  // column closes: it goes to Members first.
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const query = window.matchMedia(wide);
    const narrowed = () => {
      if (!query.matches && membersBody.current?.contains(document.activeElement) === true) membersButton.current?.focus();
    };
    query.addEventListener('change', narrowed);
    return () => {
      query.removeEventListener('change', narrowed);
    };
  }, []);
  // Focus goes back to Settings once the panel has closed: until then the
  // button is inert behind it, and cannot take focus. Unless the voice options
  // sheet is open: it is modal, the button stays inert behind it, and focus
  // goes back into the sheet. Re-authentication is above Settings and makes it
  // inert: while it is up nothing here moves focus (the restore waits), and
  // when it clears focus goes to what is next, the Settings panel if it is
  // still open, else the button Settings was opened from.
  const settingsClosed = useRef(false);
  useEffect(() => {
    if (settingsOpen) onSettingsOpen?.();
  }, [settingsOpen, onSettingsOpen]);
  useEffect(() => {
    if (asking) return;
    if (settingsOpen) {
      if (toJoining.current) toJoining.current = false;
      else settingsHeading.current?.focus();
    } else if (settingsClosed.current) {
      settingsClosed.current = false;
      // The button is inert behind the voice options sheet: focus belongs in
      // the sheet, which takes it (VoicePanel) now that nothing is over it.
      // And focus the person already has somewhere usable is left alone: it
      // moves only from the body, nothing, the closed panel or an inert subtree.
      const at = document.activeElement;
      const stranded = at === null || at === document.body || at.closest('[inert]') !== null || at.closest('#settings-panel') !== null;
      if (!sheetModal.current && stranded) settingsButton.current?.focus();
    }
  }, [settingsOpen, asking]);
  // Focus goes back to Members once the panel has closed: until then the
  // button is inert behind it, and cannot take focus.
  const membersClosed = useRef(false);
  // The drawer was closed by an action that shows something in the centre
  // (LeaveDrawer): focus goes there, not to Members.
  const toCentre = useRef(false);
  const centre = useRef<HTMLElement>(null);
  // What the focus recovery below reads, as of the last render.
  const over = useRef({ over: false, blocked: false });
  over.current = { over: membersOver, blocked: settingsOpen || asking || overlays > 0 };
  // While the drawer is modal and nothing is over it, focus that ends up
  // nowhere usable (the body, nothing, a subtree that is inert) comes back to
  // the drawer: a row that remounted under focus, re-authentication that went
  // away. Usable focus, and a modal of another kind, are left alone.
  const recoverFocus = useCallback(() => {
    if (!over.current.over || over.current.blocked) return;
    const at = document.activeElement;
    if (at !== null && at !== document.body && at.closest('[inert]') === null) return;
    membersBody.current?.focus();
  }, []);
  // The channel the drawer was opened on, by id: a rename keeps it. A channel
  // shown that the list does not hold (yet) is no change; none shown is null.
  const shownChannel = channels.selected === null ? null : (channels.channels.find((c) => c.name === channels.selected)?.id ?? undefined);
  const openedOn = useRef<number | null>(null);
  const wasOver = useRef(false);
  useEffect(() => {
    const entered = membersOver && !wasOver.current;
    wasOver.current = membersOver;
    if (entered) {
      openedOn.current = shownChannel ?? null;
      membersBody.current?.focus();
    } else if (membersOver) recoverFocus();
    else if (membersClosed.current) {
      membersClosed.current = false;
      membersButton.current?.focus();
    } else if (toCentre.current) {
      toCentre.current = false;
      // The heading of what the action showed, else the conversation's.
      (centre.current?.querySelector<HTMLElement>('[data-centre-target]') ?? document.getElementById(headingId))?.focus();
    }
  }, [membersOver, asking, recoverFocus]);
  // A row that remounts under focus drops it with no event to hear.
  useEffect(() => {
    const body = membersBody.current;
    if (!membersOver || body === null || typeof MutationObserver === 'undefined') return;
    // One task later, and only if focus is still nowhere: the browser's own
    // Tab (out of a row's menu, say) moves it first, and it is not ours to take.
    let later = 0;
    const watch = new MutationObserver(() => {
      window.clearTimeout(later);
      later = window.setTimeout(recoverFocus, 0);
    });
    watch.observe(body, { childList: true, subtree: true });
    return () => {
      window.clearTimeout(later);
      watch.disconnect();
    };
  }, [membersOver, recoverFocus]);
  // Another channel (or none) is shown than the one the drawer was opened on:
  // it closes rather than stay over a page it has made inert. Only a change
  // after the opening counts: a shown channel that was still settling when the
  // drawer opened (none, then the first one) is taken as the one it opened on,
  // and showing the same channel again changes nothing. The wide side column
  // stays open across channels.
  useEffect(() => {
    if (!over.current.over) return;
    if (shownChannel === undefined) return;
    if (openedOn.current === null) {
      openedOn.current = shownChannel;
      return;
    }
    if (shownChannel === openedOn.current) return;
    const at = document.activeElement;
    if (at === null || at === document.body || membersBody.current?.contains(at) === true) membersClosed.current = true;
    setPanelOpen(false);
  }, [shownChannel]);
  const leaveDrawer = useCallback(() => {
    if (!over.current.over) return;
    toCentre.current = true;
    setPanelOpen(false);
    // On a phone the centre is the Chat view's: the other views hide it, and
    // what is shown there could not take focus.
    if (phone) setView('chat');
  }, [phone]);
  // Arriving (signing in, or a sign-in restored on reload) leaves focus on
  // the page's body, where the next Tab starts from the top: the first time a
  // conversation is shown, focus goes to its heading (issue #590). Only then:
  // opening another channel later leaves focus where it is.
  const arrived = useRef(false);
  const hasFirstRun = firstRun !== null && firstRun !== undefined;
  const showing = channels.loaded && ((conversation !== null && conversation !== undefined) || hasFirstRun);
  useEffect(() => {
    if (!showing || arrived.current) return;
    const heading = document.getElementById(headingId);
    if (heading === null) return;
    arrived.current = true;
    if (document.activeElement === null || document.activeElement === document.body) heading.focus();
  }, [showing]);

  // Safari can click a button without focusing it. On leaving Voice, wait
  // until the dock changes and the sheet releases the inert page before
  // recovering focus that fell to the body or stayed in a hidden control.
  const previousView = useRef(view);
  const viewButton = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    if (voiceSheet && view !== 'voice') return;
    const leftVoice = previousView.current === 'voice' && view !== 'voice';
    previousView.current = view;
    if (!leftVoice) return;
    const button = viewButton.current;
    viewButton.current = null;
    if (!phone || behind) return;
    const at = document.activeElement;
    if (at !== null && at !== document.body) {
      const hiddenInDock = dock.current?.contains(at) === true &&
        (at.closest('[hidden]') !== null || at.getClientRects().length === 0 || getComputedStyle(at).visibility !== 'visible');
      if (!hiddenInDock) return;
    }
    const destination = button ?? (view === 'chat' ? document.getElementById(headingId) ?? centre.current : channelsHeading.current);
    if (destination === null) return;
    // The centre takes no tabindex of its own (a press on it must not focus
    // it, issue #2196): it gets one only for this focus, and loses it on blur.
    if (destination === centre.current && !destination.hasAttribute('tabindex')) {
      destination.tabIndex = -1;
      destination.addEventListener('blur', () => { destination.removeAttribute('tabindex'); }, { once: true });
    }
    destination.focus();
  }, [phone, view, voiceSheet, behind]);

  function closeSettings() {
    setSettingsOpen(false);
    setConfirming(null);
    settingsClosed.current = true;
  }
  function addPerson() {
    closeSettings();
    onAddPerson?.();
  }
  function closeMembers() {
    setPanelOpen(false);
    membersClosed.current = true;
  }
  // Everything behind the drawn-over members panel is inert, so Tab past its
  // last control would leave the page for the browser's own chrome: it wraps
  // instead, first to last and last to first, as in any modal dialog. A key
  // something inside has already used (a menu, the profile card) is left alone.
  function onMembersKey(event: KeyboardEvent) {
    if (event.key === 'Escape') {
      onEscape(closeMembers)(event);
      return;
    }
    if (event.key !== 'Tab' || event.defaultPrevented) return;
    const body = membersBody.current;
    if (body === null) return;
    const stops = [body, ...body.querySelectorAll<HTMLElement>('a[href], button, input, select, textarea, summary, [tabindex]')].filter(
      (el) => el.tabIndex >= 0 && !el.matches(':disabled') && el.closest('[hidden], [inert]') === null,
    );
    const first = stops[0];
    const last = stops[stops.length - 1];
    if (first === undefined || last === undefined) return;
    // In an open row menu, the menu's own ⋯ button is the stop: Tab from it
    // closes the menu, and the page goes on from the button, not from an item.
    const focused = document.activeElement;
    const menu = focused?.closest('[role="menu"]') ?? null;
    const at = menu === null || !body.contains(menu) ? focused : (menu.parentElement?.querySelector('[aria-haspopup="menu"]') ?? focused);
    if (event.shiftKey ? at === first : at === last) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus();
    }
  }
  // Escape closes the panel focus is in, and the panel's button gets focus.
  const onEscape = (close: () => void) => (event: KeyboardEvent) => {
    if (event.key !== 'Escape' || event.defaultPrevented) return;
    event.preventDefault();
    close();
  };

  return (
    <JoiningSettings.Provider value={joining}>
    <SettingsOpen.Provider value={settingsOpen}>
    <OverlayReport.Provider value={reportOverlay}>
    <AddPerson.Provider value={onAddPerson === undefined ? null : addPerson}>
      <div ref={lobbyEl} className={`${phone ? `lobby phone view-${view}${live ? ' in-voice' : ''}` : 'lobby'}${band === undefined ? '' : ' has-band'}`}>
        {/* While Settings is open the rest of the page is inert: the panel is
            modal, and focus cannot wander behind its backdrop. */}
        <div className="top-group" inert={band !== undefined && (settingsOpen || asking || voiceSheet || membersOver)}>
        <header className="topbar" inert={settingsOpen || asking || voiceSheet || membersOver}>
          <h1 className="brand">Conch</h1>
          <div role="status" aria-label="Status" className="status">
            <Avatar id={me} name={name} />
            <span className="status-who">{name}</span>
            {/* A phone has no room for it (issue #1030). */}
            {phone ? null : <span className="status-server">{origin}</span>}
            <span className={`conn conn-${connection}`}>{connectionLabel(connection)}</span>
          </div>
          <div className="topbar-actions">
            {notificationCenter}
            <DrawerOver.Provider value={membersOver}>{invites}</DrawerOver.Provider>
            <button
              ref={membersButton}
              type="button"
              className="icon-button"
              aria-expanded={panelOpen}
              aria-controls="members-body"
              onClick={() => {
                setPanelOpen(!panelOpen);
              }}
            >
              <Glyph name="members" />
              <span className="label">Members</span>
            </button>
            <button
              ref={settingsButton}
              type="button"
              className="icon-button"
              aria-expanded={settingsOpen}
              aria-controls="settings-panel"
              aria-label={phone ? `Settings, signed in as ${name}` : undefined}
              onClick={() => {
                if (settingsOpen) closeSettings();
                else setSettingsOpen(true);
              }}
            >
              <Glyph name="settings" />
              <span className="label">Settings</span>
            </button>
          </div>
        </header>
        {band}
        </div>
        {/* The areas hold the members panel, which stays live while it is drawn over
            the page: every other part of them is inert then, one by one. */}
        <div className={panelOpen ? 'areas' : 'areas side-closed'} inert={settingsOpen || asking || voiceSheet}>
          {/* The switcher's entry is above the Channels navigation, not in it: that holds channel controls only. */}
          <div className="channels" inert={membersOver}>
            {channels.loaded ? (
            <ChannelNav
              channels={inVisualOrder}
              unread={unread}
              selected={channels.selected}
              me={me}
              refresh={directoryTick}
              onOpen={(name) => {
                onOpen(name);
                if (phone) {
                  toHeading.current = true;
                  setView('chat');
                }
              }}
              {...(onBrowse === undefined
                ? {}
                : {
                    onBrowse: (name: string) => {
                      onBrowse(name);
                      if (phone) setView('channels');
                    },
                  })}
            />
            ) : null}
          <nav aria-label="Channels">
            <h2 ref={channelsHeading} tabIndex={-1}>Channels</h2>
            {channels.loaded ? (
              <>
                <ChannelList
                  {...(visit === undefined ? { arrangement: arranged } : {})}
                  channels={channels.channels}
                  selected={channels.selected}
                  unread={unread}
                  occupancy={occupancy}
                  voiceNames={voiceNames}
                  viewer={me}
                  empty={newChannel === null || newChannel === undefined ? undefined : 'No channels yet'}
                  {...(canRename === undefined ? {} : { canRename })}
                  {...(renderRename === undefined ? {} : { renderRename })}
                  {...(canSetTopic === undefined ? {} : { canSetTopic })}
                  {...(renderTopic === undefined ? {} : { renderTopic })}
                  {...(canManage === undefined ? {} : { canManage })}
                  {...(renderSettings === undefined ? {} : { renderSettings })}
                  {...(onInviteTo === undefined ? {} : { onInvite: onInviteTo })}
                  {...(onLeft === undefined ? {} : { onLeft })}
                  {...(onUnauthorized === undefined ? {} : { onUnauthorized })}
                  onJoinVoice={(channel) => {
                    joinVoice.current?.(channel);
                  }}
                  onOpen={(name) => {
                    onOpen(name);
                    if (phone) {
                      toHeading.current = true;
                      setView('chat');
                    }
                  }}
                />
                <ShowChat.Provider
                  value={() => {
                    if (!phone) return;
                    toHeading.current = true;
                    setView('chat');
                  }}
                >
                  {newChannel}
                </ShowChat.Provider>
                {phone ? firstRun : null}
              </>
            ) : connection === 'reconnecting' ? (
              <div className="problem">
                <p className="empty">Cannot reach the server</p>
                <button type="button" onClick={onRetry}>
                  Try again
                </button>
                {report}
              </div>
            ) : (
              <p className="empty">Loading channels…</p>
            )}
          </nav>
            {channels.loaded && visit === undefined && channels.channels.length > 0 ? <GroupsPanel controls={arranged} /> : null}
          </div>
          <main ref={centre} className="centre" inert={membersOver}>
            <div ref={setStage} className="stage" />
            <section ref={conversationEl} aria-label="Conversation" className="conversation">
              {temporary === true ? <TemporaryBanner /> : null}
              {banner}
              {channels.lost === undefined ? null : (
                <p role="alert" className="banner">
                  You no longer have access to #{channels.lost}
                </p>
              )}
              {conversation}
              {phone ? null : firstRun}
            </section>
          </main>
          <aside aria-label="Members panel" className="panel">
            <div
              ref={membersBody}
              id="members-body"
              hidden={!panelOpen}
              tabIndex={panelOpen ? 0 : undefined}
              aria-label="Members list"
              role="region"
              onKeyDown={membersOver ? onMembersKey : undefined}
              onBlur={membersOver ? (event) => {
                if (event.relatedTarget === null) window.setTimeout(recoverFocus, 0);
              } : undefined}
            >
              {membersOver ? (
                <button type="button" className="icon-button close" onClick={closeMembers}>
                  <Glyph name="close" />
                  <span className="label">Close</span>
                </button>
              ) : null}
              <LeaveDrawer.Provider value={leaveDrawer}>{panel}</LeaveDrawer.Provider>
            </div>
          </aside>
          {membersOver ? <div className="backdrop" onClick={closeMembers} /> : null}
        </div>
        <div ref={dock} className="dock" inert={settingsOpen || asking || membersOver}>
          <VoiceLive.Provider value={setLive}>
            <MicTestShown.Provider value={!phone || view === 'voice'}>
              <VoiceStrip.Provider value={phone && view !== 'voice' ? openVoice : null}>
                <StageHost.Provider value={stage}>
                  <VoiceSheet.Provider value={sheetLink}>
                    <ChannelJoin.Provider value={joinVoice}>{voice}</ChannelJoin.Provider>
                  </VoiceSheet.Provider>
                </StageHost.Provider>
              </VoiceStrip.Provider>
            </MicTestShown.Provider>
          </VoiceLive.Provider>
        </div>
        {phone ? (
          <nav aria-label="Main navigation" className="phone-nav" inert={settingsOpen || asking || membersOver}>
            {phoneViews.map(({ view: v, label }) => (
              <button
                key={v}
                ref={v === 'voice' ? voiceTab : undefined}
                type="button"
                aria-current={view === v ? 'page' : undefined}
                aria-describedby={v === 'voice' && live ? 'nav-voice-live' : undefined}
                onClick={(event) => {
                  if (view === 'voice' && v !== 'voice') viewButton.current = event.currentTarget;
                  setView(v);
                }}
              >
                <Glyph name={v === 'chat' ? 'chat' : v === 'channels' ? 'channels' : 'headphones'} />
                <span className="nav-label">{label}</span>
                {v === 'voice' && live ? <span className="nav-live" aria-hidden="true" /> : null}
              </button>
            ))}
            {/* Outside the button, so the button's name stays Voice. */}
            {live ? (
              <span id="nav-voice-live" className="vh">
                You are in voice
              </span>
            ) : null}
          </nav>
        ) : null}
        {settingsOpen && !asking ? <div className="backdrop" onClick={closeSettings} /> : null}
        <div
          id="settings-panel"
          role="dialog"
          aria-modal="true"
          aria-labelledby="settings-heading"
          className="drawer" hidden={!settingsOpen} inert={asking} onKeyDown={onEscape(closeSettings)}>
          <div className="drawer-head">
            <h2 id="settings-heading" ref={settingsHeading} tabIndex={-1}>
              Settings
            </h2>
            <button type="button" className="icon-button close" onClick={closeSettings}>
              <Glyph name="close" />
              <span className="label">Close</span>
            </button>
          </div>
          <section aria-label="Account" className="drawer-section">
            <p className="who">
              <Avatar id={me} name={name} />
              <span>
                Signed in as <strong>{name}</strong>
                <br />
                <span className="hint">{origin}</span>
              </span>
            </p>
            {account}
            {signIns?.(() => {
              setConfirming('here');
            })}
          </section>
          {appearance}
          {admin === undefined || admin === null ? null : (
            <section aria-label="Administration" className="drawer-section">
              {admin}
            </section>
          )}
          <section aria-label="Sign out" className="drawer-section signout">
            {visit !== undefined ? (
              visit
            ) : confirming === null ? (
              <>
                <div className="actions">
                  <button
                    type="button"
                    onClick={() => {
                      setConfirming('here');
                    }}
                    aria-describedby="signout-note"
                  >
                    Sign out
                  </button>
                  <button
                    type="button"
                    onClick={() => {
                      setConfirming('everywhere');
                    }}
                    aria-describedby="signout-note"
                  >
                    Sign out everywhere
                  </button>
                </div>
                <p id="signout-note" className="hint">
                  Sign out ends this sign-in {desktop ? 'in this app' : 'in every tab of this browser'}. Sign out everywhere ends all of yours, on every device.
                  {passwordSet
                    ? ' Either way, you sign in again with your name and password.'
                    : ' Either way, you will need a new link from an operator to come back into this account.'}
                </p>
              </>
            ) : (
              <div role="group" aria-labelledby="signout-question" className="signout-confirm">
                <p id="signout-question">{signOutQuestion(confirming, passwordSet, desktop)}</p>
                <div className="actions">
                  <button
                    type="button"
                    className="danger"
                    onClick={() => {
                      const which = confirming;
                      setConfirming(null);
                      if (which === 'here') onSignOut();
                      else onSignOutEverywhere();
                    }}
                  >
                    {confirming === 'here' ? 'Yes, sign out' : 'Yes, sign out everywhere'}
                  </button>
                  <button
                    ref={cancel}
                    type="button"
                    onClick={() => {
                      setConfirming(null);
                    }}
                  >
                    Cancel
                  </button>
                </div>
              </div>
            )}
            {signOutProblem === null ? null : (
              <p role="alert" className="notice">
                {signOutProblem}
              </p>
            )}
          </section>
          <section aria-label="Help" className="drawer-section">
            {report}
            <ServerVersion className="hint" quiet />
          </section>
        </div>
        {overlay}
      </div>
    </AddPerson.Provider>
    </OverlayReport.Provider>
    </SettingsOpen.Provider>
    </JoiningSettings.Provider>
  );
}
