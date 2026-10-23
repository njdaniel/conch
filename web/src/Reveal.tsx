// What a page that shows a secret once needs (issue #714, design §5.3,
// §6.2): Add a device's link and an operator's authenticator key are hidden
// until Reveal, and Reveal is refused while this tab shares its screen.
import { useSyncExternalStore } from 'react';
import { onTabSharing, tabSharing } from './voice/share';

// useTabSharing follows whether this tab is sharing its screen.
export function useTabSharing(): boolean {
  return useSyncExternalStore(onTabSharing, tabSharing);
}

// Said beside every such secret: the page knows of this tab's own share,
// and of nothing else that may be watching the screen.
export const revealLimits =
  'Reveal it only where nobody else can see your screen. This page knows when this tab is sharing its screen, and will not show it then; it cannot know about another tab, another app, a screen recorder or a remote session. A copy can leak too: clipboard history, clipboard sync between devices and a remote desktop’s shared clipboard may keep it, so copy over something else once it is used.';

// Said in place of the secret while this tab shares.
export const sharingLine = 'This tab is sharing its screen, so this is not shown. Stop sharing, then reveal it.';
