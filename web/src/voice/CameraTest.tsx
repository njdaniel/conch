// The private camera test (ADR-015, design §7.1), in Voice options: a local
// preview of the camera chosen, "Preview: not sent". It captures, and nothing
// more: nothing is claimed, published or recorded. It is drawn only while
// Voice options is open, so closing them takes it out of the tree, and its
// cleanup stops the capture. MicTest.tsx is its pattern.
import { useEffect, useRef, useState } from 'react';
import { Glyph } from '../Icons';
import { captureFailure, failureNote } from './camera';
import { captureCameraTest, type CameraTestCapture } from './sdk';

// What the test needs from the browser, so a unit test needs none of it.
export interface CameraTestDeps {
  readonly capture: (choice: string | null) => Promise<CameraTestCapture>;
}

const browser: CameraTestDeps = { capture: captureCameraTest };

export const previewNote = 'Preview: not sent';

export function CameraTest({ choose, chosen, onCaptured, deps = browser }: {
  // The camera to capture, asked for when the test starts: the one chosen,
  // found again by its name (N7); null for the browser's own choice; false
  // for a choice that is not connected, when nothing is captured.
  readonly choose: () => Promise<string | null | false>;
  // Which camera is chosen now, by id ('' for none): a running test moves to
  // a new choice, the old capture stopped first.
  readonly chosen: string;
  // Told once the browser gave a capture: it names its cameras from then.
  readonly onCaptured?: () => void;
  readonly deps?: CameraTestDeps;
}) {
  const [on, setOn] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const preview = useRef<HTMLVideoElement>(null);
  const told = useRef(onCaptured);
  told.current = onCaptured;
  const chooser = useRef(choose);
  chooser.current = choose;

  useEffect(() => {
    if (!on) return;
    // Read afresh after each await: the cleanup ends the run.
    let live = true;
    const over = () => !live;
    let held: CameraTestCapture | null = null;
    void (async () => {
      const choice = await chooser.current();
      if (over()) return;
      if (choice === false) {
        setProblem(failureNote('not_connected'));
        setOn(false);
        return;
      }
      let capture: CameraTestCapture;
      try {
        capture = await deps.capture(choice);
      } catch (err: unknown) {
        if (over()) return;
        setProblem(failureNote(captureFailure(err)));
        setOn(false);
        return;
      }
      // Stopped, or the options closed, while the browser was asked.
      if (over()) {
        capture.stop();
        return;
      }
      held = capture;
      capture.show(preview.current);
      told.current?.();
    })();
    return () => {
      live = false;
      held?.stop();
    };
  }, [on, chosen, deps]);

  return (
    <div className="camera-test">
      <button
        type="button"
        aria-pressed={on}
        onClick={() => {
          setProblem(null);
          setOn(!on);
        }}
      >
        <Glyph name="camera" />
        <span className="label">{on ? 'Stop camera test' : 'Test camera'}</span>
      </button>
      {on ? (
        <figure className="camera-preview">
          <video ref={preview} className="mirrored" aria-label="Camera preview" muted playsInline autoPlay />
          <figcaption>{previewNote}</figcaption>
        </figure>
      ) : null}
      {problem === null ? null : (
        <p role="status" className="hint">
          {problem}
        </p>
      )}
    </div>
  );
}
