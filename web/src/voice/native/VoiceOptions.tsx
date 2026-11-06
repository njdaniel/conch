// Microphone, speakers and talk mode in the desktop's Voice options (issue
// #1379, D3-8 of #771). The device list is the helper's (NativeVoice.devices);
// a choice is kept for the next session (saveDevice, `config set`) and, in a
// v2 session, applied now (setDevices, setTalkMode). Rules (UX-11,
// tui-voice.md section 20.5): the page shows the helper's confirmed capture
// and playback, never what it asked for; a device that is gone is shown as
// gone, with no automatic alternative; and the open microphone is only ever
// the person's own, confirmed choice: no problem state changes the talk mode.
//
// The bridge does not say what is saved in voice.toml, so the lists show a
// choice only once it is made here or the helper confirms the talk mode.
//
// Device names and descriptions are other software's text and may carry bidi
// format characters: they are stripped of those, and each is set apart in its
// own isolate (an isolating mark pair inside an option, a bdi elsewhere), so
// one name cannot reorder the words around it.
import { useCallback, useEffect, useRef, useState } from 'react';
import type { NativeVoiceDevicesResult, NativeVoiceView, VoiceDeviceKey, VoiceOneShotError } from '../../platform/desktop/bridge';
import type { NativeVoice } from '../../platform/types';
import type { VoiceControlTalkMode, VoiceDevice } from '../../schema/generated';

export const systemDefault = 'default';
export const noPipeWire = 'PipeWire is not available';
export const micMissing = 'Microphone not found: nothing is sent';
export const speakersMissing = 'Speakers not found: you will hear nothing';
export const nextTime = 'takes effect next time you join';
export const openMeans = 'sends whenever you speak; no key needed';

const oneShotWords: Readonly<Record<VoiceOneShotError, string>> = {
  'not-signed-in': 'Sign in to use voice.',
  'not-installed': 'The voice helper is not installed.',
  untrusted: 'The voice helper is not trusted.',
  busy: 'Voice is busy; try again in a moment.',
  invalid: 'That choice was refused.',
  'spawn-failed': 'The voice helper could not start.',
  'timed-out': 'The voice helper did not answer in time.',
  'too-much-output': 'The voice helper gave too much output.',
  unsupported: 'Voice is not available here.',
  failed: 'The voice helper could not do that.',
  'bad-output': 'The voice helper gave an answer that could not be read.',
  cancelled: 'Cancelled.',
};

// Bidi format characters: marks, embeddings and overrides, isolates.
const bidi = /[؜‎‏‪-‮⁦-⁩]/gu;
const isolateStart = '⁨';
const isolateEnd = '⁩';

// shown is a device's text for people: its description, else its name, with no
// bidi control.
export function shown(d: Pick<VoiceDevice, 'name' | 'description'>): string {
  const text = (d.description === '' ? d.name : d.description).replace(bidi, '');
  return text === '' ? 'Unnamed device' : text;
}

type Side = 'input' | 'output';
const keyOf: Readonly<Record<Side, VoiceDeviceKey>> = { input: 'audio.input', output: 'audio.output' };

type Listing =
  | { readonly kind: 'loading' }
  | { readonly kind: 'failed'; readonly error: VoiceOneShotError }
  | { readonly kind: 'pipewire' }
  | { readonly kind: 'ok'; readonly microphones: readonly VoiceDevice[]; readonly speakers: readonly VoiceDevice[] };

function listing(r: NativeVoiceDevicesResult): Listing {
  if (!r.ok) return { kind: 'failed', error: r.error };
  if (r.pipewire !== 'ok') return { kind: 'pipewire' };
  return { kind: 'ok', microphones: r.microphones, speakers: r.speakers };
}

// What the person chose in this window; absent until they choose.
interface Chosen {
  readonly input?: string;
  readonly output?: string;
  readonly talk?: VoiceControlTalkMode;
}

export function VoiceOptions({
  source,
  view,
  open,
}: {
  readonly source: NativeVoice;
  readonly view: NativeVoiceView;
  readonly open: boolean;
}) {
  const [devices, setDevices] = useState<Listing>({ kind: 'loading' });
  const [chosen, setChosen] = useState<Chosen>({});
  const [busy, setBusy] = useState<Side | 'talk' | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const refresh = useCallback(() => {
    setDevices({ kind: 'loading' });
    source.devices().then(
      (r) => {
        if (alive.current) setDevices(listing(r));
      },
      () => {
        if (alive.current) setDevices({ kind: 'failed', error: 'failed' });
      },
    );
  }, [source]);
  useEffect(() => {
    if (open) refresh();
  }, [open, refresh]);

  const h = view.helper;
  const session = h !== undefined && view.status === 'running';
  // set_devices and set_talk_mode are v2; a v1 helper takes a choice next time.
  const live = session && h.version === 2;
  const saveNote = 'Could not keep that choice for next time.';

  const choose = async (side: Side, value: string) => {
    setNote(null);
    setBusy(side);
    try {
      if (live) {
        const out = await source.setDevices(side === 'input' ? { input: value } : { output: value });
        if (!out.ok) {
          setNote('Voice could not switch; try again.');
          return;
        }
      }
      const saved = await source.saveDevice({ key: keyOf[side], value });
      if (!alive.current) return;
      setChosen((c) => ({ ...c, [side]: value }));
      if (!saved.ok) setNote(saveNote);
    } finally {
      if (alive.current) setBusy(null);
    }
  };

  const chooseTalk = async (mode: VoiceControlTalkMode) => {
    setNote(null);
    setConfirmOpen(false);
    setBusy('talk');
    try {
      if (live) {
        const out = await source.setTalkMode(mode);
        if (!out.ok) {
          setNote('Voice could not switch; try again.');
          return;
        }
      }
      const saved = await source.saveDevice({ key: 'audio.talk_mode', value: mode });
      if (!alive.current) return;
      setChosen((c) => ({ ...c, talk: mode }));
      if (!saved.ok) setNote(saveNote);
    } finally {
      if (alive.current) setBusy(null);
    }
  };

  const mic = live && h.capture === 'unavailable';
  const speakers = live && h.playback === 'unavailable';
  const talk: VoiceControlTalkMode | undefined = live ? (h.talkMode ?? chosen.talk) : chosen.talk;
  const lists = devices.kind === 'ok' ? devices : null;

  return (
    <div id="voice-options" className="voice-options" hidden={!open}>
      {devices.kind === 'pipewire' ? <p role="status" className="hint">{noPipeWire}</p> : null}
      {devices.kind === 'failed' ? (
        <p role="status" className="hint">
          Devices could not be listed: {oneShotWords[devices.error]}{' '}
          <button type="button" onClick={refresh}>Try again</button>
        </p>
      ) : null}
      {devices.kind === 'loading' ? <p className="hint">Listing devices…</p> : null}
      {lists === null ? null : (
        <>
          <DeviceChoice
            side="input"
            label="Microphone"
            list={lists.microphones}
            value={chosen.input}
            busy={busy === 'input'}
            onChoose={(v) => { void choose('input', v); }}
            missing={mic}
            missingNote={micMissing}
            confirmed={live ? (h.capture === 'on' ? 'Microphone is on' : h.capture === 'off' ? 'Microphone is off' : null) : null}
          />
          <DeviceChoice
            side="output"
            label="Speakers"
            list={lists.speakers}
            value={chosen.output}
            busy={busy === 'output'}
            onChoose={(v) => { void choose('output', v); }}
            missing={speakers}
            missingNote={speakersMissing}
            confirmed={live ? (h.playback === 'on' ? 'Speakers are on' : h.playback === 'off' ? 'Speakers are off' : null) : null}
          />
          <p className="hint">
            <button type="button" onClick={refresh}>Refresh the list</button>
          </p>
        </>
      )}
      {session && !live && lists !== null ? <p className="hint">Devices and talk mode {nextTime}.</p> : null}
      <fieldset className="voice-mode" disabled={busy !== null}>
        <legend>Talk mode</legend>
        <label>
          <input
            type="radio"
            name="native-talk-mode"
            checked={talk === 'push_to_talk'}
            onChange={() => { void chooseTalk('push_to_talk'); }}
          />
          Push to talk
        </label>
        <label>
          <input
            type="radio"
            name="native-talk-mode"
            checked={talk === 'open'}
            onChange={() => { if (talk !== 'open') setConfirmOpen(true); }}
          />
          Open microphone
        </label>
        {busy === 'talk' ? <span className="hint">switching…</span> : null}
        {confirmOpen ? (
          <p className="hint" role="alertdialog" aria-label="Use the open microphone">
            Open microphone {openMeans}.{' '}
            <button type="button" className="primary" onClick={() => { void chooseTalk('open'); }}>Use open microphone</button>{' '}
            <button type="button" onClick={() => { setConfirmOpen(false); }}>Cancel</button>
          </p>
        ) : null}
        {talk === undefined ? <span className="hint">Choose a talk mode{live ? '' : `; it ${nextTime}`}.</span> : null}
        {session && !live ? <span className="hint">Talk mode {nextTime}.</span> : null}
      </fieldset>
      {note === null ? null : <p role="status" className="hint">{note}</p>}
    </div>
  );
}

function DeviceChoice({
  side,
  label,
  list,
  value,
  busy,
  onChoose,
  missing,
  missingNote,
  confirmed,
}: {
  readonly side: Side;
  readonly label: string;
  readonly list: readonly VoiceDevice[];
  readonly value: string | undefined;
  readonly busy: boolean;
  readonly onChoose: (value: string) => void;
  readonly missing: boolean;
  readonly missingNote: string;
  readonly confirmed: string | null;
}) {
  const id = `voice-native-${side}`;
  const listed = value === undefined || value === systemDefault || list.some((d) => d.name === value);
  // A chosen device the list lacks is shown as not found, never replaced.
  const gone = !listed || missing;
  const current = list.find((d) => d.name === value);
  const defaults = list.filter((d) => d.default);
  return (
    <div className="voice-device">
      <label htmlFor={id}>{label}</label>
      <select
        id={id}
        value={value ?? ''}
        disabled={busy}
        onChange={(e) => {
          if (e.target.value !== '') onChoose(e.target.value);
        }}
      >
        {value === undefined ? (
          <option value="" disabled>
            Choose {label.toLowerCase()}
          </option>
        ) : null}
        {listed ? null : (
          <option value={value} disabled>
            {`${isolateStart}${shown({ name: value, description: '' })}${isolateEnd} (not found)`}
          </option>
        )}
        <option value={systemDefault}>System default</option>
        {list.map((d) => (
          <option key={d.name} value={d.name}>
            {`${isolateStart}${shown(d)}${isolateEnd}${d.default ? ' (default)' : ''}`}
          </option>
        ))}
      </select>
      {busy ? <p className="hint">switching…</p> : null}
      {current === undefined || value === undefined ? null : (
        <p className="hint">
          Chosen: <bdi>{shown(current)}</bdi>
        </p>
      )}
      {value === systemDefault ? (
        <p className="hint">
          System default{defaults.length === 0 ? '' : <>: <bdi>{shown(defaults[0] ?? { name: '', description: '' })}</bdi></>}
        </p>
      ) : null}
      {gone ? <p role="status" className="hint">{missingNote}</p> : null}
      {confirmed === null || busy ? null : <p className="hint">{confirmed}</p>}
    </div>
  );
}
