// Where voice would appear. The browser has no voice (it stays out of the
// extension, #354), so it says so whatever the server's voice configuration,
// and never asks for a microphone: nothing here reaches for a device.
export function VoiceStatus() {
  return (
    <section aria-label="Voice" className="voice">
      <p>Not in voice</p>
      <p className="hint">Voice is not available in the browser yet</p>
    </section>
  );
}
