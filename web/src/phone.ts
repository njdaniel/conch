import { createContext, useEffect, useState } from 'react';

// How the voice section tells the lobby whether this page is in voice (joined,
// connecting or reconnecting): the lobby marks its Voice button with it and,
// on a phone, shows the compact strip under Chat and Channels only then. Only
// a report of what the section already knows; the lobby never starts or stops
// anything with it.
export const VoiceLive = createContext<(live: boolean) => void>(() => undefined);

// Whether the voice section draws the private microphone test. On a phone the
// section is hidden outside the Voice view (and the strip only while in voice),
// so a test left running there would capture with no indicator and no Stop. The
// lobby says no outside the Voice view: the test is unmounted, and its cleanup
// (the same as Stop's) releases the microphone. It is a private check, not call
// capture, so nothing about voice changes. True everywhere else.
export const MicTestShown = createContext(true);

// The phone layout (issue #929): below this width the lobby shows one of
// three views at a time, with a bottom navigation. The same width is in
// app.css; at and above it nothing here applies.
const phone = '(max-width: 699px)';

// The views of the phone layout. Chat is the open channel's conversation,
// Channels the list of channels, Voice the voice section given the page.
export type PhoneView = 'chat' | 'channels' | 'voice';

export const phoneViews: readonly { readonly view: PhoneView; readonly label: string }[] = [
  { view: 'chat', label: 'Chat' },
  { view: 'channels', label: 'Channels' },
  { view: 'voice', label: 'Voice' },
];

function isPhone(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia(phone).matches;
}

// usePhone follows the window across the breakpoint. Only classes follow it:
// nothing is mounted or unmounted, so a call or a share carries on through a
// rotation or a resize. Where there is no matchMedia (jsdom) it is false, and
// the lobby is the wide one.
export function usePhone(): boolean {
  const [now, setNow] = useState(isPhone);
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const query = window.matchMedia(phone);
    const change = () => {
      setNow(query.matches);
    };
    query.addEventListener('change', change);
    return () => {
      query.removeEventListener('change', change);
    };
  }, []);
  return now;
}

// useAppHeight keeps --app-height, on the root element, at the height of the
// visual viewport while the phone layout is on. Browsers that resize the layout
// viewport for the on-screen keyboard (the viewport meta asks for it) need
// nothing more; one that only shrinks the visual viewport (iOS Safari) would
// otherwise leave the composer and the strip under the keyboard. Set through
// the CSSOM, which the page's style-src 'self' allows; the stylesheet falls
// back to 100dvh without it.
export function useAppHeight(on: boolean): void {
  useEffect(() => {
    // Absent in older browsers, and in jsdom.
    if (!on || !('visualViewport' in window)) return;
    const viewport = window.visualViewport;
    if (viewport === null) return;
    const root = document.documentElement;
    // Pinch zoom shrinks the visual viewport in CSS pixels (at 2x it is half the
    // layout viewport) without the layout viewport changing: the height is
    // scaled back, so a zoomed page keeps its layout and the keyboard (which
    // shrinks the viewport at whatever scale) still shortens it. Skipping the
    // update while zoomed would instead leave a stale keyboard height.
    const set = () => {
      root.style.setProperty('--app-height', `${String(Math.round(viewport.height * viewport.scale))}px`);
    };
    set();
    viewport.addEventListener('resize', set);
    return () => {
      viewport.removeEventListener('resize', set);
      root.style.removeProperty('--app-height');
    };
  }, [on]);
}
