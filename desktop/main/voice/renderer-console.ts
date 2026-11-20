// Forward voice diagnostics from the existing Chromium renderer-to-main console
// event. Only fixed descriptions reach the main log. SDK context, arbitrary
// error text and source URLs can contain tokens, room names or SDP credentials.
import type { WebContents } from 'electron';

const voiceMessages = new Set([
  'Screen capture stopped delivering frames (muted for 3 seconds).',
  'Screen share encoder is CPU-limited.',
  'LiveKit retried negotiation with unmodified SDP.',
  'LiveKit could not apply an SDP description.',
  'LiveKit SDP description rejected due to a BUNDLE codec collision.',
  'LiveKit SDK warning.',
  'LiveKit SDK error.',
]);

function describe(message: string): string | undefined {
  if (voiceMessages.has(message)) return message;
  if (message.includes('Bundled payload type collision') || message.includes('A BUNDLE group contains a codec collision')) {
    return 'WebRTC reported a BUNDLE collision.';
  }
  if (/\b(?:RTCPeerConnection|RTCRtpSender|RTCRtpReceiver|RTCDataChannel|RTCError|WebRTC)\b/.test(message)) {
    return 'WebRTC reported a renderer warning or error.';
  }
  return undefined;
}

export function forwardVoiceConsole(
  contents: Pick<WebContents, 'on' | 'mainFrame'>,
  log: (line: string) => void,
  now = () => performance.now(),
): void {
  const last = new Map<string, number>();
  contents.on('console-message', ({ frame, level, message }) => {
    if ((level !== 'warning' && level !== 'error') || frame !== contents.mainFrame) return;
    // Chromium's own diagnostic and SDK console calls have different source
    // IDs. Neither source IDs nor renderer-controlled text go into the log.
    const diagnostic = describe(message);
    if (diagnostic === undefined) return;
    const at = now();
    const previous = last.get(diagnostic);
    if (previous !== undefined && at - previous < 10_000) return;
    last.set(diagnostic, at);
    log(`conch-desktop: voice renderer ${level}: ${diagnostic}`);
  });
}
