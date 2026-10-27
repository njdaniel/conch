// The private microphone test (issue #846): check the microphone before, or
// outside, a call. The capture goes into a local audio graph on this page, a
// level meter reads it, and the person's own voice is played back only if they
// ask. Nothing connects to a room, nothing is sent, and nothing is recorded or
// kept: the graph reads the audio as it passes and drops it.
//
// The test is mounted only while not in voice, so joining voice takes it out
// of the tree and its cleanup releases the microphone before the call asks for
// it. Every way the test ends (Stop, unmounting, a change of microphone, the
// microphone going) runs the same cleanup: each track stopped, the source
// disconnected, the audio context closed.
import { useEffect, useId, useRef, useState } from 'react';
import type { Microphone } from './connection';
import { captureTest, testMicrophones, watchMicrophones, type TestCapture } from './sdk';

// What the test needs from the browser, so a unit test needs none of it.
export interface MicTestDeps {
  readonly capture: (choice: string | null) => Promise<TestCapture>;
  readonly microphones: () => Promise<readonly Microphone[] | null>;
  readonly watch: (onChange: () => void) => () => void;
  readonly audioContext: () => AudioContext | null;
}

function newContext(): AudioContext | null {
  return typeof AudioContext === 'function' ? new AudioContext() : null;
}

const browser: MicTestDeps = {
  capture: (choice) => captureTest(choice),
  microphones: testMicrophones,
  watch: watchMicrophones,
  audioContext: newContext,
};

export const deniedNote = 'Microphone access was denied. Allow it for this site in your browser, then start the test again.';
export const noMicrophoneNote = 'No microphone was found. Connect one, then start the test again.';
export const lostNote = 'Your microphone was disconnected, so the test stopped. Choose a microphone and start it again.';
export const failedNote = 'The microphone could not be opened. Start the test again.';
export const unsupportedNote = 'This browser cannot test the microphone.';
export const headphonesNote = 'Use headphones: played through speakers, your voice feeds back into the microphone.';
export const privateNote = 'Private: nothing is sent or recorded, and you are not in voice.';

// systemDefault is the value of the entry for the browser's default microphone.
const systemDefault = '';

// How often the meter reads the level.
export const meterEvery = 50;

type Status = 'off' | 'starting' | 'on';
type Problem = 'denied' | 'missing' | 'lost' | 'failed' | 'unsupported';

const problemNote: Record<Problem, string> = {
  denied: deniedNote,
  missing: noMicrophoneNote,
  lost: lostNote,
  failed: failedNote,
  unsupported: unsupportedNote,
};

// problemOf says what a refused capture was.
function problemOf(err: unknown): Problem {
  const name = err instanceof Error ? err.name : '';
  if (name === 'NotAllowedError' || name === 'SecurityError' || name === 'PermissionDeniedError') return 'denied';
  if (name === 'NotFoundError' || name === 'OverconstrainedError' || name === 'DevicesNotFoundError') return 'missing';
  return 'failed';
}

// levelOf is the loudness of a block of samples, 0 to 1: the root mean square,
// lifted so that speech at a normal distance moves most of the meter.
export function levelOf(samples: Float32Array): number {
  if (samples.length === 0) return 0;
  let sum = 0;
  for (const s of samples) sum += s * s;
  return Math.min(1, Math.sqrt(sum / samples.length) * 3);
}

export function MicTest({ deps = browser }: { readonly deps?: MicTestDeps }) {
  const [wanted, setWanted] = useState(false);
  const [choice, setChoice] = useState<string | null>(null);
  const [status, setStatus] = useState<Status>('off');
  const [problem, setProblem] = useState<Problem | null>(null);
  const [level, setLevel] = useState(0);
  const [loopback, setLoopback] = useState(false);
  const [list, setList] = useState<readonly Microphone[]>([]);
  const gain = useRef<GainNode | null>(null);
  const listId = useId();

  // The test itself: one capture per run of this effect. A change of
  // microphone runs the cleanup of the old one before the new one opens, and
  // Stop, the microphone going and unmounting run it too.
  useEffect(() => {
    if (!wanted) return;
    let cancelled = false as boolean;
    let capture: TestCapture | null = null;
    let context: AudioContext | null = null;
    let source: MediaStreamAudioSourceNode | null = null;
    let timer: number | undefined;
    let unwatch: (() => void) | null = null;
    let ended: (() => void) | null = null;
    let endedOn: MediaStreamTrack | null = null;

    const release = () => {
      if (timer !== undefined) window.clearInterval(timer);
      timer = undefined;
      unwatch?.();
      unwatch = null;
      source?.disconnect();
      source = null;
      gain.current = null;
      if (ended !== null) endedOn?.removeEventListener('ended', ended);
      ended = null;
      endedOn = null;
      capture?.stop();
      capture = null;
      if (context !== null) void context.close().catch(() => undefined);
      context = null;
    };

    // The microphone went: end the test and say so. A chosen device that is
    // gone is not chosen any more.
    const lose = (why: Problem) => {
      if (cancelled) return;
      if (why === 'lost' || why === 'missing') setChoice(null);
      setProblem(why);
      setWanted(false);
    };

    const refresh = async (opened: TestCapture) => {
      const inputs = await deps.microphones().catch(() => null);
      if (cancelled || capture !== opened || inputs === null) return;
      setList(inputs);
      // A chosen device missing from the list is gone, whether or not its
      // track has ended: a sound server may move the stream to another device.
      if (choice !== null && !inputs.some((m) => m.id === choice)) lose('lost');
    };

    setStatus('starting');
    setProblem(null);
    setLevel(0);
    void (async () => {
      const ctx = deps.audioContext();
      if (ctx === null) {
        lose('unsupported');
        return;
      }
      context = ctx;
      let opened: TestCapture;
      try {
        opened = await deps.capture(choice);
      } catch (err) {
        lose(problemOf(err));
        return;
      }
      if (cancelled) {
        // Stopped while the browser was still opening the device.
        opened.stop();
        return;
      }
      capture = opened;
      try {
        const stream = new MediaStream([opened.track]);
        source = ctx.createMediaStreamSource(stream);
        const analyser = ctx.createAnalyser();
        analyser.fftSize = 1024;
        const out = ctx.createGain();
        // Silent until the person asks to hear themselves.
        out.gain.value = 0;
        gain.current = out;
        source.connect(analyser);
        source.connect(out);
        out.connect(ctx.destination);
        const samples = new Float32Array(analyser.fftSize);
        timer = window.setInterval(() => {
          analyser.getFloatTimeDomainData(samples);
          setLevel(levelOf(samples));
        }, meterEvery);
        void ctx.resume().catch(() => undefined);
        ended = () => {
          lose('lost');
        };
        endedOn = opened.track;
        endedOn.addEventListener('ended', ended);
        unwatch = deps.watch(() => {
          void refresh(opened);
        });
      } catch {
        lose('failed');
        return;
      }
      setStatus('on');
      // The list has names now that the microphone is allowed.
      void refresh(opened);
    })();

    return () => {
      cancelled = true;
      release();
      setStatus('off');
      setLevel(0);
    };
    // The loopback is applied by its own effect, so a switch does not restart.
  }, [wanted, choice, deps]);

  useEffect(() => {
    if (gain.current !== null) gain.current.gain.value = loopback ? 1 : 0;
  }, [loopback, status]);

  const running = status !== 'off';
  const note = problem === null ? null : problemNote[problem];
  return (
    <div className="mic-test" role="group" aria-label="Test your microphone">
      <div className="mic-test-row">
        <button
          type="button"
          aria-pressed={running}
          onClick={() => {
            setWanted(!wanted);
          }}
        >
          {running ? 'Stop test' : 'Test microphone'}
        </button>
        <label htmlFor={listId} className="mic-test-label">
          Microphone
        </label>
        <select
          id={listId}
          value={choice ?? systemDefault}
          onChange={(e) => {
            setChoice(e.target.value === systemDefault ? null : e.target.value);
          }}
        >
          <option value={systemDefault}>System default</option>
          {list.map((m) => (
            <option key={m.id} value={m.id}>
              {m.label}
            </option>
          ))}
        </select>
      </div>
      {running ? (
        <div className="mic-test-row">
          <meter className="mic-meter" min={0} max={1} value={level} aria-label="Microphone test level" />
          <label className="mic-test-label">
            <input
              type="checkbox"
              checked={loopback}
              onChange={(e) => {
                setLoopback(e.target.checked);
              }}
            />{' '}
            Hear myself
          </label>
        </div>
      ) : null}
      {running && loopback ? (
        <p className="hint">
          {headphonesNote}
        </p>
      ) : null}
      {note === null ? null : (
        <p role="status" className="hint">
          {note}
        </p>
      )}
      <p className="hint">{status === 'starting' ? 'Waiting for the microphone.' : privateNote}</p>
    </div>
  );
}
