// The talk and mute key setup of the desktop app (#1382, #2056). Windows
// dispatches to WindowsTalkKeySetup (#777), with no device grant or path.
// On Linux, capture a key or Mouse 4 in this window as soon as setup opens.
// Show a device choice only when needed. The device-access instructions
// explain what a read grant exposes before showing the user any rule.
//
// Capture: listeners exist only while choosing a key. Escape closes setup and
// Cancel stops the capture. During a save, Close and Escape wait for the result.
// The event's code is turned at once into an evdev number and a name; the
// event itself is not kept, logged or passed on. Only the number and the name
// outlive the capture. Rule text is rendered as text and copied with
// services.copyText; nothing here executes it.
import { WindowsTalkKeySetup } from './WindowsTalkKeySetup';
import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
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

export function savedBindingWords(binding: 'talk' | 'mute', label: string, nextJoin: boolean): string {
  const action = binding === 'mute' ? `press ${label} to toggle mute` : `hold ${label} to talk`;
  return `Saved: ${action}.${nextJoin ? ` ${takeEffect}` : ''}`;
}

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

function DeviceAccess({ devices, disclosure }: {
  readonly devices: readonly VoiceKeyDevice[];
  readonly disclosure: readonly string[];
}) {
  const [first] = devices;
  if (first === undefined) return null;
  return (
    <details>
      <summary>Device access instructions</summary>
      <div className="talk-disclosure">
        {disclosure.map((line) => <p key={line}>{line}</p>)}
      </div>
      {devices.length === 1 ? <Rule device={first} /> : (
        devices.map((device) => (
          <details key={device.id}>
            <summary><bdi>{device.name}</bdi></summary>
            <Rule device={device} />
          </details>
        ))
      )}
    </details>
  );
}

export function TalkKeySetup({
  source,
  joined,
  binding = 'talk',
  openRequest = 0,
  onSaved,
  onDetachedSaveError,
  onClose,
}: {
  readonly source: NativeVoice;
  // Whether a voice session is on: a saved key then waits for the next join.
  readonly joined: boolean;
  readonly binding?: 'talk' | 'mute';
  // A new activation requests focus even when the chosen binding is unchanged.
  readonly openRequest?: number;
  // A successful write reaches the parent even after setup closes.
  // Windows says whether the change waits for a join.
  readonly onSaved: (label: string, nextJoinOnly?: boolean) => void;
  readonly onDetachedSaveError?: (message: string) => void;
  readonly onClose: () => void;
}) {
  const title = binding === 'mute' ? 'Mute key' : 'Talk key';
  const [listing, setListing] = useState<VoiceKeyDevicesResult | null>(null);
  const [deviceId, setDeviceId] = useState<string | null>(null);
  const [key, setKey] = useState<EvdevKey | null>(null);
  const [capturing, setCapturing] = useState(true);
  const [refused, setRefused] = useState(false);
  const [saving, setSaving] = useState(false);
  const [outcome, setOutcome] = useState<string | null>(null);
  const live = useRef(true);
  const heading = useId();
  const saveStatus = useId();
  const setup = useRef<HTMLDivElement>(null);
  const saveButton = useRef<HTMLButtonElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  const handledOpen = useRef<number | null>(null);
  const windows = listing?.ok === true && listing.platform === 'win32';

  // A launch takes focus. Replacing the loading view on Windows only recovers
  // focus if the user has stayed in setup.
  useLayoutEffect(() => {
    const root = setup.current;
    if (root === null) return;
    const opened = handledOpen.current !== openRequest;
    handledOpen.current = openRequest;
    const active = document.activeElement;
    if (windows && !opened && active !== null && active !== document.body && !root.contains(active)) return;
    const target = root.querySelector<HTMLElement>('button:not(:disabled, [aria-disabled="true"]), select:not(:disabled), input:not(:disabled)')
      ?? root.querySelector<HTMLElement>('h3');
    target?.focus();
  }, [openRequest, windows]);

  const load = useCallback(() => {
    // Check again disappears while the device list reloads.
    if (setup.current?.contains(document.activeElement)) closeButton.current?.focus();
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

  const canCapture = capturing && listing?.ok === true && listing.platform !== 'win32';
  // Install the listeners before the capture prompt becomes visible.
  useLayoutEffect(() => {
    if (!canCapture) return;
    let pendingShift: string | null = null;
    const take = (next: EvdevKey) => {
      pendingShift = null;
      setKey(next);
      setRefused(false);
      setCapturing(false);
      setOutcome(null);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' || e.code === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        if (!e.repeat) onClose();
        return;
      }
      // Leave the capture without consuming keyboard navigation.
      if (e.code === 'Tab' || e.target instanceof HTMLSelectElement) {
        pendingShift = null;
        setCapturing(false);
        return;
      }
      e.preventDefault();
      e.stopPropagation();
      if (e.repeat) return;
      // Wait for Shift to be released so Shift+Tab can move focus.
      if (e.code === 'ShiftLeft' || e.code === 'ShiftRight') {
        pendingShift = e.code;
        return;
      }
      pendingShift = null;
      const found = keyForCode(e.code);
      if (found === null) setRefused(true);
      else take(found);
    };
    const onKeyUp = (e: KeyboardEvent) => {
      if (e.code !== pendingShift) return;
      const found = keyForCode(e.code);
      if (found !== null) take(found);
    };
    // A blur or any click means a held Shift no longer stands alone.
    const onBlur = () => { pendingShift = null; };
    const onMouse = (e: MouseEvent) => {
      pendingShift = null;
      // The first three buttons are the click itself and stay a click.
      const found = keyForMouseButton(e.button);
      if (found === null) return;
      e.preventDefault();
      e.stopPropagation();
      take(found);
    };
    window.addEventListener('keydown', onKey, true);
    window.addEventListener('keyup', onKeyUp, true);
    window.addEventListener('mousedown', onMouse, true);
    window.addEventListener('blur', onBlur);
    return () => {
      window.removeEventListener('blur', onBlur);
      window.removeEventListener('keydown', onKey, true);
      window.removeEventListener('keyup', onKeyUp, true);
      window.removeEventListener('mousedown', onMouse, true);
    };
  }, [canCapture, onClose]);

  const devices = listing !== null && listing.ok ? listing.devices : [];
  const matching = devices.filter((d) => d.kind === key?.kind);
  const readable = matching.filter((d) => d.readable);
  const unreadable = matching.filter((d) => !d.readable);
  const device = (readable.length === 1 ? readable[0] : readable.find((d) => d.id === deviceId)) ?? null;
  const ready = device !== null && key !== null && !capturing;

  const save = async () => {
    if (device === null || key === null || !ready || saving) return;
    const call = source.saveTalkBinding;
    // Keep Save focusable as the other editing controls become disabled.
    saveButton.current?.focus();
    setSaving(true);
    setOutcome(null);
    let result: VoiceTalkBindingResult;
    try {
      result = await call?.({ deviceId: device.id, code: key.code, ...(binding === 'mute' ? { binding } : {}) })
        ?? { ok: false, error: 'unsupported' };
    } catch {
      result = { ok: false, error: 'failed' };
    }
    if (result.ok) onSaved(key.label);
    const message = result.ok ? savedBindingWords(binding, key.label, joined) : failedWords(result);
    const root = setup.current;
    if (root === null) {
      if (!result.ok) onDetachedSaveError?.(message);
      return;
    }
    if (!result.ok && root.contains(document.activeElement)) saveButton.current?.focus();
    setOutcome(message);
    setSaving(false);
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

  const content = windows ? (
    <WindowsTalkKeySetup source={source} binding={binding} onSaved={onSaved} onDetachedSaveError={onDetachedSaveError} onClose={onClose} />
  ) : (
    <section className="talk-setup" aria-labelledby={heading} onKeyDown={(event) => {
      if (event.key !== 'Escape' || event.defaultPrevented) return;
      event.preventDefault();
      event.stopPropagation();
      if (!event.repeat && !saving) onClose();
    }}>
      <h3 id={heading} tabIndex={-1}>{title}</h3>

      {listing === null ? <p role="status" className="hint">Reading what is available…</p> : null}
      {listing !== null && !listing.ok ? (
        <>
          <p role="status" className="hint">The {binding} key setup is not available: {oneShotWords[listing.error]}</p>
          <button type="button" onClick={load}>Check again</button>
        </>
      ) : null}

      {listing !== null && listing.ok ? (
        <>
          <div className="talk-binding">
            <p>
              {title}: <strong>{key === null ? 'none chosen' : <bdi>{key.label}</bdi>}</strong>
            </p>
            {capturing ? <p role="status">Press the key or Mouse 4 now. Escape closes setup.</p> : null}
            <button type="button" disabled={saving} onClick={() => {
              setRefused(false);
              setCapturing(!capturing);
            }}>{capturing ? 'Cancel' : `Set ${binding} key`}</button>
            {refused ? <p role="status" className="hint">{binding === 'mute' ? "That key can't be used as a mute key." : cantUse}</p> : null}
            <label>
              Or pick one{' '}
              <select value={key?.label ?? ''} disabled={saving} onMouseDown={() => { setCapturing(false); }} onFocus={() => { setCapturing(false); }} onChange={(e) => { pick(e.target.value); }}>
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
                {matching.length === 0 ? (
                  <p role="status" className="hint">
                    No {key.kind} was found that Conch can use, so this {key.kind === 'mouse' ? 'button' : 'key'} can't be set
                    on this machine yet. There is no device to grant access to. Connect a {key.kind} and choose Check again.
                  </p>
                ) : null}
                {matching.length > 0 && readable.length === 0 ? (
                  <p role="status" className="hint">
                    Conch needs access to a {key.kind} to use this binding while another app is focused.
                    A read grant lets other apps you run read that device too.
                  </p>
                ) : null}
                {readable.length === 1 && device !== null ? <p className="hint">Using <bdi>{device.name}</bdi>.</p> : null}
                {readable.length > 1 ? (
                  <fieldset className="talk-devices">
                    <legend>{key.kind === 'mouse' ? 'Mouse' : 'Keyboard'}</legend>
                    {readable.map((d) => (
                      <label key={d.id} className="talk-device">
                        <input
                          type="radio"
                          name="talk-device"
                          checked={device?.id === d.id}
                          disabled={saving}
                          onChange={() => { setDeviceId(d.id); }}
                        />{' '}
                        <bdi>{d.name}</bdi>
                      </label>
                    ))}
                  </fieldset>
                ) : null}
                <DeviceAccess devices={unreadable} disclosure={listing.disclosure} />
                <button type="button" disabled={saving} onClick={load}>Check again</button>
              </>
            )}
            <button ref={saveButton} type="button" className="primary" disabled={!ready} aria-disabled={saving || undefined} onClick={() => { void save(); }}>Save</button>
          </div>
        </>
      ) : null}

      {saving || outcome !== null ? <p id={saveStatus} role="status">{saving ? 'Saving…' : outcome}</p> : null}
      <button ref={closeButton} type="button" aria-disabled={saving || undefined} aria-describedby={saving ? saveStatus : undefined} onClick={() => { if (!saving) onClose(); }}>Close</button>
    </section>
  );

  return <div ref={setup}>{content}</div>;
}
