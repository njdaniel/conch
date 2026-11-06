// The result shapes the logic reads from the app's API client (M4-C writes the
// client). These are not wire shapes: the wire types are schema/generated.ts.
// They mirror web/src/api.ts at ed4a68f9.
import type { VoiceCameraV1, VoiceSessionResponseV1 } from './schema/generated';

export type ApiFailure = 'unauthorized' | 'network' | 'invalid';

export type VoiceSessionResult =
  | { readonly ok: true; readonly value: VoiceSessionResponseV1 }
  | { readonly ok: false; readonly failure: ApiFailure; readonly status?: number; readonly code?: string };

export type CameraClaimResult =
  | { readonly ok: true; readonly value: VoiceCameraV1 }
  | { readonly ok: false; readonly failure: ApiFailure; readonly status?: number; readonly code?: string };

// The answer to one transmit report: ok; no_session is 409; rate_limited is
// 429; failed is no answer or a 5xx, which may be retried.
export type TransmitAnswer = 'ok' | 'no_session' | 'rate_limited' | 'unauthorized' | 'failed';
