// Windows uses the existing config-set bridge for talk and mute keys.
// Capture starts on request and ends on blur, Escape, a pick or a session change.
// A saved binding waits for the next join; the test reads confirmed state of
// the active voice session and cannot open a microphone or choose a channel.
import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import type { VoiceTalkBindingResult } from '../../platform/desktop/bridge';
import type { NativeVoice } from '../../platform/types';
import { windowsKey, windowsKeys, type WindowsKey } from '../../platform/desktop/windows-keys';
import { useNativeVoice } from './useNativeVoice';
import { failedWords, savedBindingWords, takeEffect } from './TalkKeySetup';
import { pttWords, pttProblemWords } from './words';

export function WindowsTalkKeySetup({ source, binding = 'talk', onSaved, onDetachedSaveError, onClose }: {
  readonly source: NativeVoice;
  readonly binding?: 'talk' | 'mute';
  readonly onSaved: (label: string, nextJoinOnly?: boolean) => void;
  readonly onDetachedSaveError?: (message: string) => void;
  readonly onClose: () => void;
}) {
  const { view } = useNativeVoice(source);
  const title = binding === 'mute' ? 'Mute key' : 'Talk key';
  const [key, setKey] = useState<WindowsKey | null>(null);
  const [capturing, setCapturing] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testFocused, setTestFocused] = useState(false);
  const section = useRef<HTMLElement>(null);
  const captureButton = useRef<HTMLButtonElement>(null);
  const cancelButton = useRef<HTMLButtonElement>(null);
  const saveButton = useRef<HTMLButtonElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  const headingElement = useRef<HTMLHeadingElement>(null);
  const wasCapturing = useRef(false);
  const heading = useId();
  const saveStatus = useId();
  const canTest = view.status === 'running';
  // Transfer focus before the browser disables the active test's button.
  useLayoutEffect(() => {
    if (!canTest && testFocused) (saving ? headingElement.current : closeButton.current)?.focus();
  }, [canTest, testFocused, saving]);
  useEffect(() => {
    setCapturing(false);
    setTesting(false);
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
    if (key === null || saving || capturing) return;
    saveButton.current?.focus();
    setSaving(true);
    setNote(null);
    let result: VoiceTalkBindingResult;
    try {
      result = await source.saveTalkBinding?.({ windowsCode: key.code, generation: view.generation, ...(binding === 'mute' ? { binding } : {}) })
        ?? { ok: false, error: 'unsupported' };
    } catch {
      result = { ok: false, error: 'failed', failed: binding };
    }
    // Main validates the write against its generation. Report its result
    // even if the setup or active session has since changed.
    if (result.ok) onSaved(key.label, view.status !== 'idle' && view.status !== 'ended');
    const message = result.ok ? savedBindingWords(binding, key.label, true) : failedWords(result);
    const root = section.current;
    if (root === null) {
      if (!result.ok) onDetachedSaveError?.(message);
      return;
    }
    if (!result.ok && root.contains(document.activeElement)) saveButton.current?.focus();
    setNote(message);
    setSaving(false);
  };
  const h = canTest ? view.helper : undefined;
  return <section ref={section} className="talk-setup" aria-labelledby={heading} onKeyDown={(event) => {
    if (event.key !== 'Escape' || event.defaultPrevented) return;
    event.preventDefault();
    event.stopPropagation();
    if (!event.repeat && !saving) onClose();
  }}>
    <h3 ref={headingElement} id={heading} tabIndex={-1}>{title}</h3>
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
    <button ref={saveButton} type="button" disabled={key === null || capturing} aria-disabled={saving || undefined} onClick={() => { void save(); }}>Save</button>
    <p>{takeEffect} Leave and join to use a saved change.</p>
    {binding === 'mute' ? (
      <p>Tap and release the mute key once after joining, then press it to toggle mute.</p>
    ) : (
      <>
        <p role="status">Availability: {h === undefined ? 'Join voice to check the active binding.' : pttWords[h.ptt]}{h?.pttProblem === undefined ? '' : `: ${pttProblemWords[h.pttProblem]}`}</p>
        <p>Tap and release the talk key once after joining or recovering, then hold to talk. Elevated games, secure desktop and anti-cheat compatibility require hardware testing; compatibility is unverified here.</p>
        <button type="button" disabled={!canTest && !testFocused} aria-disabled={!canTest || undefined} aria-pressed={testing}
          onFocus={() => { setTestFocused(true); }} onBlur={() => { setTestFocused(false); }}
          onClick={() => { if (canTest) setTesting(!testing); }}>Test active binding</button>
        {testing ? <p role="status">Testing voice in <bdi>{view.channel}</bdi>: {h?.deafened ? 'Deafened; nothing is sent.' : h?.muted ? 'Muted; nothing is sent.' : h?.transmitting ? 'Transmitting' : 'Not transmitting'}. Ask a second listener to confirm press and release. This test sends audio when the helper transmits.</p> : null}
      </>
    )}
    {saving || note !== null ? <p id={saveStatus} role="status">{saving ? 'Saving…' : note}</p> : null}
    <button ref={closeButton} type="button" aria-disabled={saving || undefined} aria-describedby={saving ? saveStatus : undefined} onClick={() => { if (!saving) onClose(); }}>Close</button>
  </section>;
}
