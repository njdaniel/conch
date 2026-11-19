// Copied from spikes/mobile/android at ea00f74f (#1390, M4-B) for the iOS spike (#1394, M5-B).
// Which remote tracks the phone subscribes to (webcam-video section 7.3, S1;
// #1109 M-C8). Adapted from `wanted` in web/src/voice/sdk.ts at ed4a68f9,
// without the livekit-client enums, so it needs no SDK.
//
// Phone: the screen-share sources are never wanted. The spike never asks for
// p<id>.screen or p<id>.view (ADR-016 Decision 3), so there is no `watching`.
export type TrackSource = 'microphone' | 'camera' | 'screen_share' | 'screen_share_audio' | 'unknown';
export type TrackKind = 'audio' | 'video';

export interface Wants {
  // The Voice view is open and shown.
  readonly viewing: boolean;
  readonly shown: boolean;
  // The track's owner holds a camera claim.
  readonly claimed: boolean;
}

// wanted is the subscription choice for a remote track, by its declared source
// and its kind together: a microphone if it is sound; a camera if it is a
// picture, from someone holding a claim, while the Voice view is open and
// shown. Anything else, and any track whose kind disagrees with its source,
// never.
export function wanted(source: TrackSource, kind: TrackKind | undefined, want: Wants): boolean {
  switch (source) {
    case 'microphone':
      return kind === 'audio';
    case 'camera':
      return kind === 'video' && want.viewing && want.shown && want.claimed;
    default:
      return false;
  }
}
