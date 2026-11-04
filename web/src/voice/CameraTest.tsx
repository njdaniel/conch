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

export function CameraTest({ choice, onCaptured, deps = browser }: {
  // The camera chosen, exactly, or null for the browser's own choice.
  readonly choice: string | null;
  // Told once the browser gave a capture: it names its cameras from then.
  readonly onCaptured?: () => void;
  readonly deps?: CameraTestDeps;
}) {
  const [on, setOn] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const preview = useRef<HTMLVideoElement>(null);
  const told = useRef(onCaptured);
  told.current = onCaptured;

  useEffect(() => {
    if (!on) return;
    let live = true;
    let held: CameraTestCapture | null = null;
    deps.capture(choice).then(
      (capture) => {
        // Stopped, or the options closed, while the browser was asked.
        if (!live) {
          capture.stop();
          return;
        }
        held = capture;
        capture.show(preview.current);
        told.current?.();
      },
      (err: unknown) => {
        if (!live) return;
        setProblem(failureNote(captureFailure(err)));
        setOn(false);
      },
    );
    return () => {
      live = false;
      held?.stop();
    };
  }, [on, choice, deps]);

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
