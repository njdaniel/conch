// The talk-key setup of the desktop app (issue #1382, D3-11 of #771). In this
// order: what a read grant on a keyboard exposes (the person accepts it before
// any device is listed); the devices and whether each is readable; for one that
// is not, the exact udev rule to copy and run themselves (the app runs and
// grants nothing); then the key, captured from the next key or Mouse 4 pressed
// in this window while capture is on, or picked from a list; then Save.
//
// Capture: listeners exist only between "Set talk key" and the first accepted
// key, Escape or Cancel. The event's code is turned at once into an evdev
// number and a name; the event itself is not kept, logged or passed on. Only
// the number and the name outlive the capture. Rule text is rendered as text
// and copied with services.copyText; nothing here executes it.
import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { keyboardKeys, keyForCode, keyForMouseButton, mouseKeys, type EvdevKey } from '../../platform/desktop/evdev';
import { updatePendingWords } from './words';
import type { VoiceKeyDevice, VoiceKeyDevicesResult, VoiceOneShotError, VoiceTalkBindingResult } from '../../platform/desktop/bridge';
import { platform } from '../../platform';
import type { NativeVoice } from '../../platform/types';

const oneShotWords: Readonly<Record<VoiceOneShotError, string>> = {
  'not-signed-in': 'Sign in first.',
  'not-installed': 'The voice helper is not installed.',
  untrusted: 'The voice helper is not trusted.',
  busy: 'The voice helper is busy; try again in a moment.',
  invalid: 'That was refused as invalid.',
  'spawn-failed': 'The voice helper could not be started.',
  'timed-out': 'The voice helper did not answer in time.',
  'too-much-output': 'The voice helper said too much.',
  unsupported: 'This is not available here.',
  failed: 'The voice helper failed.',
  'bad-output': 'The voice helper gave an answer that could not be read.',
  cancelled: 'It was cancelled.',
  'update-pending': updatePendingWords,
};

export const takeEffect = 'It takes effect when you next join.';
export const cantUse = "That key can't be used as a talk key.";

// failedWords says which step did not save, and why.
export function failedWords(result: Extract<VoiceTalkBindingResult, { ok: false }>): string {
  const step = result.failed === 'device' ? 'The device was not saved. ' : result.failed === 'talk' ? 'The key was not saved. ' : 'Nothing was saved. ';
  return step + oneShotWords[result.error];
}

// revokeSteps: how to take the grant back, which the app does not do.
const revokeSteps = [
  'Delete that line from the file.',
  'Run the same reload commands again.',
  'Unplug and replug the device.',
];

function Rule({ device }: { readonly device: VoiceKeyDevice }) {
  const [copied, setCopied] = useState<string | null>(null);
  const rule = device.rule;
  if (rule === undefined) return <p className="hint">This device cannot be read, and no rule is known for it.</p>;
  const copy = (what: string, text: string) => {
    setCopied(null);
    void platform.services.copyText(text).then(
      () => { setCopied(`Copied ${what}.`); },
      () => { setCopied('Could not copy; select the text and copy it yourself.'); },
    );
  };
  return (
    <div className="talk-rule">
      <p>
        To let Conch read <bdi>{device.name}</bdi>, you can add this rule yourself. Conch does not run it and grants nothing.
      </p>
      <p>
        File, as root: <code>{rule.file}</code>{' '}
        <button type="button" onClick={() => { copy('the file name', rule.file); }}>Copy file name</button>
      </p>
      <pre className="talk-rule-line" tabIndex={0}>{rule.line}</pre>
      <button type="button" onClick={() => { copy('the rule', rule.line); }}>Copy rule</button>
      <p>Then, run:</p>
      <ol>
        {rule.reload.map((step) => (
          <li key={step}>
            <code>{step}</code>{' '}
            <button type="button" onClick={() => { copy('the step', step); }}>Copy</button>
          </li>
        ))}
      </ol>
      <p>To take the grant back:</p>
      <ol>
        {revokeSteps.map((step) => (
          <li key={step}>{step}</li>
        ))}
      </ol>
      <p role="status" className="hint">{copied}</p>
    </div>
  );
}

export function TalkKeySetup({
  source,
  joined,
  onSaved,
  onClose,
}: {
  readonly source: NativeVoice;
  // Whether a voice session is on: a saved key then waits for the next join.
  readonly joined: boolean;
  // The chosen key's name, once saved.
  readonly onSaved: (label: string) => void;
  readonly onClose: () => void;
}) {
  const [listing, setListing] = useState<VoiceKeyDevicesResult | null>(null);
  const [accepted, setAccepted] = useState(false);
  const [deviceId, setDeviceId] = useState<string | null>(null);
  const [key, setKey] = useState<EvdevKey | null>(null);
  const [capturing, setCapturing] = useState(false);
  const [refused, setRefused] = useState(false);
  const [saving, setSaving] = useState(false);
  const [outcome, setOutcome] = useState<string | null>(null);
  const live = useRef(true);
  const heading = useId();

  const load = useCallback(() => {
    setListing(null);
    const call = source.keyDevices;
    if (call === undefined) {
      setListing({ ok: false, error: 'unsupported' });
      return;
    }
    call().then(
      (r) => { if (live.current) setListing(r); },
      () => { if (live.current) setListing({ ok: false, error: 'failed' }); },
    );
  }, [source]);

  useEffect(() => {
    live.current = true;
    load();
    return () => {
      live.current = false;
    };
  }, [load]);

  // The capture: listeners exist only while capturing.
  useEffect(() => {
    if (!capturing) return;
    const take = (next: EvdevKey) => {
      setKey(next);
      setRefused(false);
      setCapturing(false);
    };
    const onKey = (e: KeyboardEvent) => {
      e.preventDefault();
      e.stopPropagation();
      if (e.repeat) return;
      if (e.code === 'Escape') {
        setCapturing(false);
        return;
      }
      const found = keyForCode(e.code);
      if (found === null) setRefused(true);
      else take(found);
    };
    const onMouse = (e: MouseEvent) => {
      // The first three buttons are the click itself and stay a click.
      const found = keyForMouseButton(e.button);
      if (found === null) return;
      e.preventDefault();
      e.stopPropagation();
      take(found);
    };
    window.addEventListener('keydown', onKey, true);
    window.addEventListener('mousedown', onMouse, true);
    return () => {
      window.removeEventListener('keydown', onKey, true);
      window.removeEventListener('mousedown', onMouse, true);
    };
  }, [capturing]);

  const devices = listing !== null && listing.ok ? listing.devices : [];
  const device = devices.find((d) => d.id === deviceId && d.readable) ?? null;
  const mismatch = device !== null && key !== null && device.kind !== key.kind;
  const ready = device !== null && key !== null && !mismatch && !saving;

  const save = () => {
    if (device === null || key === null || mismatch) return;
    const call = source.saveTalkBinding;
    setSaving(true);
    setOutcome(null);
    const done = (r: VoiceTalkBindingResult) => {
      if (!live.current) return;
      setSaving(false);
      if (r.ok) {
        onSaved(key.label);
        setOutcome(`Saved: hold ${key.label} to talk. ${joined ? takeEffect : ''}`.trim());
      } else {
        setOutcome(failedWords(r));
      }
    };
    if (call === undefined) {
      done({ ok: false, error: 'unsupported' });
      return;
    }
    call({ deviceId: device.id, code: key.code }).then(done, () => { done({ ok: false, error: 'failed' }); });
  };

  const pick = (value: string) => {
    setRefused(false);
    if (value === '') {
      setKey(null);
      return;
    }
    const m = mouseKeys().find((k) => k.label === value);
    const k = m ?? keyboardKeys().find(([, row]) => row.label === value)?.[1];
    if (k !== undefined) setKey(k);
  };

  return (
    <section className="talk-setup" aria-labelledby={heading}>
      <h3 id={heading}>Talk key</h3>

      {listing === null ? <p role="status" className="hint">Reading what is available…</p> : null}
      {listing !== null && !listing.ok ? (
        <>
          <p role="status" className="hint">The talk key setup is not available: {oneShotWords[listing.error]}</p>
          <button type="button" onClick={load}>Check again</button>
        </>
      ) : null}

      {listing !== null && listing.ok ? (
        <div className="talk-disclosure">
          {listing.disclosure.map((line) => (
            <p key={line}>{line}</p>
          ))}
          {accepted ? null : (
            <button type="button" className="primary" onClick={() => { setAccepted(true); }}>
              I understand
            </button>
          )}
        </div>
      ) : null}

      {accepted && listing !== null && listing.ok ? (
        <>
          <fieldset className="talk-devices">
            <legend>Device</legend>
            {devices.length === 0 ? <p className="hint">No keyboard or mouse was found.</p> : null}
            {devices.map((d) => (
              <div key={d.id} className="talk-device">
                <label>
                  <input
                    type="radio"
                    name="talk-device"
                    checked={deviceId === d.id}
                    disabled={!d.readable}
                    onChange={() => { setDeviceId(d.id); }}
                  />{' '}
                  <bdi>{d.name}</bdi> ({d.kind}, {d.readable ? 'readable' : 'not readable'})
                </label>
                {d.readable ? null : <Rule device={d} />}
              </div>
            ))}
            <button type="button" onClick={load}>Check again</button>
          </fieldset>

          <div className="talk-binding">
            <p>
              Talk key: <strong>{key === null ? 'none chosen' : <bdi>{key.label}</bdi>}</strong>
            </p>
            {capturing ? (
              <>
                <p role="status">Press the key or Mouse 4 now. Escape cancels.</p>
                <button type="button" onClick={() => { setCapturing(false); }}>Cancel</button>
              </>
            ) : (
              <button type="button" onClick={() => { setRefused(false); setCapturing(true); }}>Set talk key</button>
            )}
            {refused ? <p role="status" className="hint">{cantUse}</p> : null}
            <label>
              Or pick one{' '}
              <select value={key?.label ?? ''} onChange={(e) => { pick(e.target.value); }}>
                <option value="">Choose a key…</option>
                {mouseKeys().map((k) => (
                  <option key={k.label} value={k.label}>{k.label}</option>
                ))}
                {keyboardKeys().map(([dom, k]) => (
                  <option key={dom} value={k.label}>{k.label}</option>
                ))}
              </select>
            </label>
            {mismatch ? (
              <p role="status" className="hint">
                {key.kind === 'mouse' ? `${key.label} needs a mouse device; choose a mouse.` : `${key.label} needs a keyboard device; choose a keyboard.`}
              </p>
            ) : null}
            <button type="button" className="primary" disabled={!ready} onClick={save}>
              {saving ? 'Saving…' : 'Save'}
            </button>
          </div>
        </>
      ) : null}

      {outcome === null ? null : <p role="status">{outcome}</p>}
      <button type="button" onClick={onClose}>Close</button>
    </section>
  );
}
