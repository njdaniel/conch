// Windows uses the existing config-set bridge for talk and mute keys.
// Capture exists only on explicit request and ends on blur, Escape or a pick.
// A saved binding waits for the next join; the test reads confirmed state of
// the active voice session and cannot open a microphone or choose a channel.
import { useEffect, useId, useRef, useState } from 'react';
import type { NativeVoice } from '../../platform/types';
import { windowsKey, windowsKeys, type WindowsKey } from '../../platform/desktop/windows-keys';
import { useNativeVoice } from './useNativeVoice';
import { failedWords, takeEffect } from './TalkKeySetup';
import { pttWords, pttProblemWords } from './words';

export function WindowsTalkKeySetup({ source, binding = 'talk', onSaved, onClose }: {
  readonly source: NativeVoice;
  readonly binding?: 'talk' | 'mute';
  readonly onSaved: (label: string, nextJoinOnly?: boolean) => void;
  readonly onClose: () => void;
}) {
  const { view } = useNativeVoice(source);
  const title = binding === 'mute' ? 'Mute key' : 'Talk key';
  const [key, setKey] = useState<WindowsKey | null>(null);
  const [capturing, setCapturing] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const captureButton = useRef<HTMLButtonElement>(null);
  const cancelButton = useRef<HTMLButtonElement>(null);
  const wasCapturing = useRef(false);
  const alive = useRef(true);
  const generation = useRef(view.generation);
  const heading = useId();
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => {
    setCapturing(false);
    setTesting(false);
    setNote(null);
    generation.current = view.generation;
  }, [view.generation, view.channel, view.status]);
  useEffect(() => {
    if (capturing) cancelButton.current?.focus();
    else if (wasCapturing.current) captureButton.current?.focus();
    wasCapturing.current = capturing;
  }, [capturing]);
  useEffect(() => {
    if (!capturing) return;
    const take = (code: string) => {
      const found = windowsKey(code);
      if (found === null) { setNote(`That key can't be used as a ${binding} key.`); return; }
      setKey(found);
      setNote(null);
      setCapturing(false);
    };
    const keyboard = (event: KeyboardEvent) => {
      event.preventDefault();
      event.stopPropagation();
      if (event.repeat || event.isComposing) return;
      if (event.code === 'Escape') setCapturing(false);
      else take(event.code);
    };
    const mouse = (event: MouseEvent) => {
      if (event.button !== 3 && event.button !== 4) return;
      event.preventDefault();
      event.stopPropagation();
      take(event.button === 3 ? 'Mouse4' : 'Mouse5');
    };
    const cancel = () => { setCapturing(false); };
    window.addEventListener('keydown', keyboard, true);
    window.addEventListener('mousedown', mouse, true);
    window.addEventListener('blur', cancel);
    return () => {
      window.removeEventListener('keydown', keyboard, true);
      window.removeEventListener('mousedown', mouse, true);
      window.removeEventListener('blur', cancel);
    };
  }, [binding, capturing]);
  const save = async () => {
    if (key === null || saving) return;
    const begun = view.generation;
    setSaving(true);
    try {
      const result = await source.saveTalkBinding?.({ windowsCode: key.code, generation: begun, ...(binding === 'mute' ? { binding } : {}) });
      if (!alive.current || generation.current !== begun) return;
      if (result?.ok) {
        // The active binding stays in use until the next join. The talk
        // strip keeps its current-session label and setup shows the saved one.
        onSaved(key.label, view.status !== 'idle' && view.status !== 'ended');
        const action = binding === 'mute' ? `press ${key.label} to toggle mute` : `hold ${key.label} to talk`;
        setNote(`Saved: ${action}. ${takeEffect}`);
      } else setNote(failedWords(result ?? { ok: false, error: 'unsupported' }));
    } catch {
      if (alive.current && generation.current === begun) setNote('The key was not saved. Try again.');
    } finally {
      if (alive.current) setSaving(false);
    }
  };
  const h = view.status === 'running' ? view.helper : undefined;
  return <section className="talk-setup" aria-labelledby={heading}>
    <h3 id={heading}>{title}</h3>
    <p>Choose one physical key or modifier, or Mouse 4/5. Conch observes the binding without blocking game input. Game and system shortcuts may also use it; conflicts with other applications cannot be detected here.</p>
    <p>{title}: <strong>{key?.label ?? 'none chosen'}</strong></p>
    <button ref={captureButton} type="button" disabled={saving || capturing} onClick={() => { setCapturing(true); setNote(null); }}>Set {binding} key</button>
    {capturing ? <>
      <p role="status">Press a key or Mouse 4/5 now. Escape cancels.</p>
      <button ref={cancelButton} type="button" onClick={() => { setCapturing(false); }}>Cancel</button>
    </> : null}
    <label>Or pick one <select disabled={saving || capturing} value={key?.code ?? ''} onChange={(e) => { setKey(windowsKey(e.target.value)); }}>
      <option value="">Choose a key…</option>
      {windowsKeys.map((row) => <option key={row.code} value={row.code}>{row.label}</option>)}
    </select></label>
    <button type="button" disabled={key === null || saving || capturing} onClick={() => { void save(); }}>{saving ? 'Saving…' : 'Save'}</button>
    <p>{takeEffect} Leave and join to use a saved change.</p>
    {binding === 'mute' ? (
      <p>Tap and release the mute key once after joining, then press it to toggle mute.</p>
    ) : (
      <>
        <p role="status">Availability: {h === undefined ? 'Join voice to check the active binding.' : pttWords[h.ptt]}{h?.pttProblem === undefined ? '' : `: ${pttProblemWords[h.pttProblem]}`}</p>
        <p>Tap and release the talk key once after joining or recovering, then hold to talk. Elevated games, secure desktop and anti-cheat compatibility require hardware testing; compatibility is unverified here.</p>
        <button type="button" disabled={view.status !== 'running'} aria-pressed={testing} onClick={() => { setTesting(!testing); }}>Test active binding</button>
        {testing ? <p role="status">Testing voice in <bdi>{view.channel}</bdi>: {h?.deafened ? 'Deafened; nothing is sent.' : h?.muted ? 'Muted; nothing is sent.' : h?.transmitting ? 'Transmitting' : 'Not transmitting'}. Ask a second listener to confirm press and release. This test sends audio when the helper transmits.</p> : null}
      </>
    )}
    {note === null ? null : <p role="status">{note}</p>}
    <button type="button" onClick={onClose}>Close</button>
  </section>;
}
