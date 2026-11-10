// The way into Preview as member (issue #1517): a person's ⋯ menu asks to
// preview them, and the page that holds the preview does it. A context rather
// than a prop through the console, as "Add a person" is (joiningSettings.ts).
// null where the page offers no preview (the desktop's bridge has no header
// for it), so the item is not drawn. blocked, when set, is why the item is
// drawn disabled.
import { createContext } from 'react';
import type { ServerMemberV1 } from '../schema/principal';

export interface PreviewEntryValue {
  readonly start: (person: ServerMemberV1) => void;
  readonly blocked: string | null;
}

export const leaveVoiceFirst = 'Leave voice to start a preview.';

export const PreviewEntry = createContext<PreviewEntryValue | null>(null);
