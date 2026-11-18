import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import type { NativeVoice } from '../../platform/types';
import type { NativeVoiceView, VoiceTalkBindingRequest } from '../../platform/desktop/bridge';
import { WindowsTalkKeySetup } from './WindowsTalkKeySetup';

afterEach(cleanup);
function setup() {
  let view: NativeVoiceView = { status: 'running', generation: 7, channel: 'voice-room', retryable: false,
    helper: { version: 2, phase: 'connected', capture: 'on', transmitting: false, muted: false, deafened: false, blocked: [], ptt: 'ready' } };
  const listeners = new Set<() => void>();
  const saveTalkBinding = vi.fn<(request: VoiceTalkBindingRequest) => Promise<{ ok: true }>>(() => Promise.resolve({ ok: true }));
  const source = { snapshot: () => view, subscribe: (l: () => void) => { listeners.add(l); return () => { listeners.delete(l); }; }, saveTalkBinding } as unknown as NativeVoice;
  const saved = vi.fn();
  render(<WindowsTalkKeySetup source={source} onSaved={saved} onClose={vi.fn()} />);
  return { saveTalkBinding, saved, update: (next: NativeVoiceView) => { act(() => { view = next; for (const l of listeners) l(); }); }, view };
}
it('captures only explicitly, ignores repeat and text, translates Mouse 5, and sends no event or raw text', async () => {
  const f = setup();
  fireEvent.keyDown(window, { code: 'KeyA', key: 'private text' });
  expect(screen.getByText('none chosen')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Set talk key' }));
  fireEvent.keyDown(window, { code: 'KeyA', repeat: true });
  expect(screen.getByText('none chosen')).toBeTruthy();
  fireEvent.mouseDown(window, { button: 0 });
  expect(screen.getByRole('button', { name: 'Cancel' })).toBeTruthy();
  fireEvent.mouseDown(window, { button: 4 });
  fireEvent.keyDown(window, { code: 'KeyB', key: 'private text' });
  fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await waitFor(() => { expect(f.saveTalkBinding).toHaveBeenCalledWith({ windowsCode: 'Mouse5', generation: 7 }); });
  expect(JSON.stringify(f.saveTalkBinding.mock.calls)).not.toContain('private text');
  expect(await screen.findByText(/Saved: hold Mouse 5/)).toBeTruthy();
  expect(f.saved).toHaveBeenCalledWith('Mouse 5', true);
});
it('cancels on blur, Escape, and a session change', () => {
  const f = setup();
  for (const cancel of [() => fireEvent.blur(window), () => fireEvent.keyDown(window, { code: 'Escape' }), () => { f.update({ ...f.view, generation: 8 }); }]) {
    fireEvent.click(screen.getByRole('button', { name: 'Set talk key' }));
    cancel();
    fireEvent.keyDown(window, { code: 'KeyA' });
    expect(screen.getByText('none chosen')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
  }
});
it('tests only confirmed active-session state and respects mute and deafen', () => {
  const f = setup();
  fireEvent.click(screen.getByRole('button', { name: 'Test active binding' }));
  expect(screen.getByText(/Testing voice in/).textContent).toContain('voice-room');
  expect(screen.getByText(/Testing voice in/).textContent).toContain('Not transmitting');
  if (f.view.helper === undefined) throw new Error('missing helper');
  f.update({ ...f.view, helper: { ...f.view.helper, transmitting: true } });
  expect(screen.getByText(/Testing voice in/).textContent).toContain('Transmitting');
  f.update({ ...f.view, helper: { ...f.view.helper, muted: true, transmitting: true } });
  expect(screen.getByText(/Muted; nothing is sent/)).toBeTruthy();
  f.update({ ...f.view, helper: { ...f.view.helper, deafened: true, transmitting: true } });
  expect(screen.getByText(/Deafened; nothing is sent/)).toBeTruthy();
  expect(f.saveTalkBinding).not.toHaveBeenCalled();
});

it.each(['Enter', ' '])('keeps keyboard focus through %s activation, Escape cancellation and a key selection', (key) => {
  setup();
  const control = screen.getByRole('button', { name: 'Set talk key' });
  control.focus();
  expect(document.activeElement).toBe(control);
  // jsdom does not synthesize a button click from keyboard events.
  const activate = () => {
    fireEvent.keyDown(control, { key });
    fireEvent.keyUp(control, { key });
    fireEvent.click(control);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
  };
  activate();
  fireEvent.keyDown(document.activeElement ?? window, { code: 'Escape' });
  expect(document.activeElement).toBe(control);
  activate();
  fireEvent.keyDown(document.activeElement ?? window, { code: 'ControlRight' });
  expect(document.activeElement).toBe(control);
  activate();
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  expect(document.activeElement).toBe(control);
});
