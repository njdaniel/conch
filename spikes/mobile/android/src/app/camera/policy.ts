// Which remote cameras the phone receives, and from whom (webcam-video.md
// §7.3, S1; #1109 M-C8). SDK-free.
//
// A camera is received only while the Voice view is open and shown (the app is
// in the foreground, no lifecycle reason holds), and only from a person whose
// presence says they hold a camera claim. Every camera is let go, explicitly,
// when the Voice view closes or the app leaves the foreground, so no picture
// is downloaded behind the person's back.
import { wanted, type TrackKind, type TrackSource } from '../../logic/subscribe';

export interface CameraWants {
  // The Voice view is open (the person's choice).
  readonly viewing: boolean;
  // The app is in the foreground and nothing holds it off.
  readonly shown: boolean;
  // Principals whose presence shows a camera claim.
  readonly claimed: ReadonlySet<number>;
}

export const noCameras: CameraWants = { viewing: false, shown: false, claimed: new Set() };

// principalOf reads the person from a participant identity, `p<id>`. The
// phone's own identity and any other form (a companion `p<id>.view`) is not a
// person to show: null.
export function principalOf(identity: string): number | null {
  const match = /^p(\d+)$/.exec(identity);
  if (match?.[1] === undefined) return null;
  const id = Number(match[1]);
  return Number.isSafeInteger(id) ? id : null;
}

// wantTrack is the subscription choice for one remote track, by its source,
// its kind and its publisher together.
export function wantTrack(source: TrackSource, kind: TrackKind | undefined, identity: string, w: CameraWants): boolean {
  const principal = principalOf(identity);
  return wanted(source, kind, { viewing: w.viewing, shown: w.shown, claimed: principal !== null && w.claimed.has(principal) });
}

// claimedIn is who presence shows holding a camera claim.
export function claimedIn(participants: readonly { readonly principal_id: number; readonly camera?: boolean }[]): ReadonlySet<number> {
  return new Set(participants.filter((p) => p.camera === true).map((p) => p.principal_id));
}
