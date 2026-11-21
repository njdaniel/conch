// The talk and mute key setup of the desktop app (#1382, #2056). Windows
// dispatches to WindowsTalkKeySetup (#777), with no device grant or path.
// On Linux, in this
// order: what a read grant on a keyboard exposes (the person accepts it before
// any device is shown); the key or Mouse 4 pressed in this window, or picked
// from a list; any matching device choice or access instructions; then Save.
//
// Capture: listeners exist only between "Set talk/mute key" and the first accepted
// key, Escape or Cancel. The event's code is turned at once into an evdev
// number and a name; the event itself is not kept, logged or passed on. Only
// the number and the name outlive the capture. Rule text is rendered as text
// and copied with services.copyText; nothing here executes it.
import { WindowsTalkKeySetup } from './WindowsTalkKeySetup';
import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { keyboardKeys, keyForCode, keyForMouseButton, mouseKeys, type EvdevKey } from '../../platform/desktop/evdev';
import { updatePendingWords } from './words';
import type { VoiceKeyDevice, VoiceKeyDevicesResult, VoiceOneShotError, VoiceTalkBindingResult } from '../../platform/desktop/bridge';
import { platform } from '../../platform';
import type { NativeVoice } from '../../platform/types';

const oneShotWords: Readonly<Record<VoiceOneShotError, string>> = {
  'not-signed-in': 'Sign in first.',
  'not-installed': 'The voice helper is not installed.',
  untrusted: 'Conch blocked the voice helper. Reinstall Conch, or relaunch your development build from a folder other users cannot change.',
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
  const step = result.failed === 'device' ? 'The device was not saved. ' : result.failed !== undefined ? 'The key was not saved. ' : 'Nothing was saved. ';
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
  binding = 'talk',
  onSaved,
  onClose,
}: {
  readonly source: NativeVoice;
  // Whether a voice session is on: a saved key then waits for the next join.
  readonly joined: boolean;
  readonly binding?: 'talk' | 'mute';
  // The chosen key's name, once saved; Windows says if it waits for a join.
  readonly onSaved: (label: string, nextJoinOnly?: boolean) => void;
  readonly onClose: () => void;
}) {
  const title = binding === 'mute' ? 'Mute key' : 'Talk key';
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
      setOutcome(null);
    };
    const onKey = (e: KeyboardEvent) => {
      // Leave the capture without consuming keyboard navigation.
      if (e.code === 'Tab' || e.target instanceof HTMLSelectElement) {
        setCapturing(false);
        return;
      }
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
  const matching = devices.filter((d) => d.kind === key?.kind);
  const readable = matching.filter((d) => d.readable);
  const device = (readable.length === 1 ? readable[0] : readable.find((d) => d.id === deviceId)) ?? null;
  const ready = device !== null && key !== null && !saving && !capturing;

  const save = () => {
    if (device === null || key === null || !ready) return;
    const call = source.saveTalkBinding;
    setSaving(true);
    setOutcome(null);
    const done = (r: VoiceTalkBindingResult) => {
      if (!live.current) return;
      setSaving(false);
      if (r.ok) {
        onSaved(key.label);
        const action = binding === 'mute' ? `press ${key.label} to toggle mute` : `hold ${key.label} to talk`;
        setOutcome(`Saved: ${action}. ${joined ? takeEffect : ''}`.trim());
      } else {
        setOutcome(failedWords(r));
      }
    };
    if (call === undefined) {
      done({ ok: false, error: 'unsupported' });
      return;
    }
    call({ deviceId: device.id, code: key.code, ...(binding === 'mute' ? { binding } : {}) }).then(done, () => { done({ ok: false, error: 'failed' }); });
  };

  const pick = (value: string) => {
    setRefused(false);
    setCapturing(false);
    setOutcome(null);
    if (value === '') {
      setKey(null);
      return;
    }
    const m = mouseKeys().find((k) => k.label === value);
    const k = m ?? keyboardKeys().find(([, row]) => row.label === value)?.[1];
    if (k !== undefined) setKey(k);
  };

  if (listing?.ok && listing.platform === 'win32') return <WindowsTalkKeySetup source={source} binding={binding} onSaved={onSaved} onClose={onClose} />;

  return (
    <section className="talk-setup" aria-labelledby={heading}>
      <h3 id={heading}>{title}</h3>

      {listing === null ? <p role="status" className="hint">Reading what is available…</p> : null}
      {listing !== null && !listing.ok ? (
        <>
          <p role="status" className="hint">The {binding} key setup is not available: {oneShotWords[listing.error]}</p>
          <button type="button" onClick={load}>Check again</button>
        </>
      ) : null}

      {listing !== null && listing.ok ? (
        <div className="talk-disclosure">
          {listing.disclosure.map((line) => (
            <p key={line}>{line}</p>
          ))}
          {accepted ? null : (
            <button type="button" className="primary" onClick={() => { setAccepted(true); setCapturing(true); }}>
              I understand
            </button>
          )}
        </div>
      ) : null}

      {accepted && listing !== null && listing.ok ? (
        <>
          <div className="talk-binding">
            <p>
              {title}: <strong>{key === null ? 'none chosen' : <bdi>{key.label}</bdi>}</strong>
            </p>
            {capturing ? (
              <>
                <p role="status">Press the key or Mouse 4 now. Escape cancels.</p>
                <button type="button" onClick={() => { setCapturing(false); }}>Cancel</button>
              </>
            ) : (
              <button type="button" disabled={saving} onClick={() => { setRefused(false); setCapturing(true); }}>Set {binding} key</button>
            )}
            {refused ? <p role="status" className="hint">{binding === 'mute' ? "That key can't be used as a mute key." : cantUse}</p> : null}
            <label>
              Or pick one{' '}
              <select value={key?.label ?? ''} disabled={saving} onChange={(e) => { pick(e.target.value); }}>
                <option value="">Choose a key…</option>
                {mouseKeys().map((k) => (
                  <option key={k.label} value={k.label}>{k.label}</option>
                ))}
                {keyboardKeys().map(([dom, k]) => (
                  <option key={dom} value={k.label}>{k.label}</option>
                ))}
              </select>
            </label>
            {key === null || capturing ? null : (
              <>
                {binding === 'mute' ? <p className="hint">The talk and mute keys use the same device.</p> : null}
                {readable.length === 1 && device !== null
                  ? (
                    <>
                      <p className="hint">Using <bdi>{device.name}</bdi>.</p>
                      {matching.length <= 1 ? null : (
                        <details>
                          <summary>Other matching devices</summary>
                          {matching.filter((d) => !d.readable).map((d) => <Rule key={d.id} device={d} />)}
                        </details>
                      )}
                    </>
                  )
                  : (
                    <fieldset className="talk-devices">
                      <legend>{key.kind === 'mouse' ? 'Mouse' : 'Keyboard'}</legend>
                      {readable.length === 0 ? (
                        <p role="status" className="hint">
                          Conch needs access to a {key.kind} to use this binding while another app is focused.
                        </p>
                      ) : null}
                      {matching.map((d) => (
                        <div key={d.id} className="talk-device">
                          <label>
                            <input
                              type="radio"
                              name="talk-device"
                              checked={device?.id === d.id}
                              disabled={!d.readable || saving}
                              onChange={() => { setDeviceId(d.id); }}
                            />{' '}
                            <bdi>{d.name}</bdi>{d.readable ? null : ' (not readable)'}
                          </label>
                          {d.readable ? null : (
                            <details>
                              <summary>Device access instructions</summary>
                              <Rule device={d} />
                            </details>
                          )}
                        </div>
                      ))}
                    </fieldset>
                  )}
                <button type="button" disabled={saving} onClick={load}>Check again</button>
              </>
            )}
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
