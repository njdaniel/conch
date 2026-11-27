// The optional diagnostic is checked both at the helper and at the renderer boundary.
import {
  VOICE_CONTROL_MAX_SHARE_AUDIO_LABEL_BYTES,
  VOICE_CONTROL_MAX_SHARE_AUDIO_STREAMS,
  voiceControlShareAudioStreamV3,
  type VoiceControlShareAudioStreamV3,
} from '../../web/src/schema/generated.ts';

const encoder = new TextEncoder();

function validLabel(label: string): boolean {
  if (label.length > VOICE_CONTROL_MAX_SHARE_AUDIO_LABEL_BYTES
      || encoder.encode(label).length > VOICE_CONTROL_MAX_SHARE_AUDIO_LABEL_BYTES) return false;
  for (const c of label) {
    if (c < ' ' || (c >= '\u007f' && c <= '\u009f')
      || (c >= '\u202a' && c <= '\u202e') || (c >= '\u2066' && c <= '\u2069')
      || (c.length === 1 && c >= '\ud800' && c <= '\udfff') || c === '/' || c === '\\') return false;
  }
  return true;
}

export function isShareAudioStreams(value: unknown): value is VoiceControlShareAudioStreamV3[] {
  if (!Array.isArray(value) || value.length === 0 || value.length > VOICE_CONTROL_MAX_SHARE_AUDIO_STREAMS) return false;
  for (const row of value) {
    if (voiceControlShareAudioStreamV3(row).length !== 0) return false;
    const { application, media } = row as VoiceControlShareAudioStreamV3;
    if (!validLabel(application) || !validLabel(media)) return false;
  }
  return true;
}
