// The voice/share renderer sends a fixed code over the owned document's bridge.
// Main supplies the severity and message without reading the renderer console.
import type { GeometricSampler, MainLog } from '../diagnostics.ts';
import type { VoiceDiagnostic } from '../../../web/src/platform/desktop/bridge.ts';

const lines = {
  'capture-stalled': 'warning: Screen capture stopped delivering frames (muted for 3 seconds).',
  'encoder-cpu': 'warning: Screen share encoder is CPU-limited.',
  'sdp-retry': 'warning: LiveKit retried negotiation with unmodified SDP.',
  'sdp-rejected': 'error: LiveKit could not apply an SDP description.',
  'sdp-bundle-collision': 'error: LiveKit SDP description rejected due to a BUNDLE codec collision.',
  'sdk-warning': 'warning: LiveKit SDK warning.',
  'sdk-error': 'error: LiveKit SDK error.',
} satisfies Record<VoiceDiagnostic, string>;

export function isVoiceDiagnostic(value: unknown): value is VoiceDiagnostic {
  return typeof value === 'string' && Object.hasOwn(lines, value);
}

export function voiceDiagnosticLog(log: MainLog, now: () => number, samples: GeometricSampler<VoiceDiagnostic>): (diagnostic: VoiceDiagnostic) => void {
  // The IPC registry lives for the whole app, across profile/window changes.
  const last = new Map<VoiceDiagnostic, number>();
  return (diagnostic) => {
    const at = now();
    const previous = last.get(diagnostic);
    if (previous !== undefined && at - previous < 10_000) {
      // Count validated reports even when the local throttle keeps them silent.
      samples.sample(diagnostic, false);
      return;
    }
    last.set(diagnostic, at);
    // Keep the local throttle and sample each code separately in the export.
    if (samples.sample(diagnostic)) {
      log(`conch-desktop: voice renderer ${lines[diagnostic]}`, { ring: true });
    } else {
      log(`conch-desktop: voice renderer ${lines[diagnostic]}`);
    }
  };
}
