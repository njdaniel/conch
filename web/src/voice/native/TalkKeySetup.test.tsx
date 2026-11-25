// The talk-key setup (issue #1382, D3-11), driven by a fake native voice.
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { platform } from '../../platform';
import type { NativeVoiceView, VoiceKeyDevice, VoiceKeyDevicesResult, VoiceTalkBindingResult } from '../../platform/desktop/bridge';
import type { NativeVoice } from '../../platform/types';
import { cantUse, failedWords, TalkKeySetup, takeEffect } from './TalkKeySetup';

const rlo = String.fromCharCode(0x202e);
const kbd: VoiceKeyDevice = { id: 'k1', name: `Keyboard ${rlo}evil`, kind: 'keyboard', readable: true };
const mouse: VoiceKeyDevice = { id: 'm1', name: 'Mouse', kind: 'mouse', readable: true };
const locked: VoiceKeyDevice = {
  id: 'k2',
  name: 'Locked board',
  kind: 'keyboard',
  readable: false,
  rule: { file: '/etc/udev/rules.d/70-conch.rules', line: 'KERNEL=="event5", TAG+="uaccess"', reload: ['sudo udevadm control --reload', 'sudo udevadm trigger'] },
};
const disclosure = ['Every program running as you can see every key pressed.', 'Conch looks only at the keys you bind.'];

function fake(listing: VoiceKeyDevicesResult = { ok: true, devices: [kbd, mouse, locked], disclosure }, save: VoiceTalkBindingResult = { ok: true }) {
  const keyDevices = vi.fn(() => Promise.resolve(listing));
  const saveTalkBinding = vi.fn<(r: { readonly deviceId: string; readonly code: number }) => Promise<VoiceTalkBindingResult>>(() => Promise.resolve(save));
  const nv = { keyDevices, saveTalkBinding } as unknown as NativeVoice;
  return { nv, keyDevices, saveTalkBinding };
}

async function open(f = fake(), joined = false) {
  const onSaved = vi.fn();
  const onClose = vi.fn();
  render(<TalkKeySetup source={f.nv} joined={joined} onSaved={onSaved} onClose={onClose} />);
  await screen.findByText(/Press the key or Mouse 4 now/);
  return { ...f, onSaved, onClose };
}
const press = (code: string) => { fireEvent.keyDown(window, { code }); };

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('TalkKeySetup', () => {
  it('captures first and explains the exposure before showing a device rule', async () => {
    await open(fake({ ok: true, devices: [locked], disclosure }));
    expect(screen.queryByText(disclosure[0] ?? '')).toBeNull();
    expect(screen.queryByText(/Locked board/)).toBeNull();
    press('CapsLock');
    expect(screen.getByText('Caps Lock', { selector: 'bdi' })).toBeTruthy();
    expect(screen.getByText(/Conch needs access to a keyboard/)).toBeTruthy();
    expect(screen.getByText(/A read grant lets other apps/)).toBeTruthy();
    const instructions = screen.getByText('Device access instructions');
    expect(instructions.closest('details')).toHaveProperty('open', false);
    fireEvent.click(instructions);
    expect(instructions.closest('details')).toHaveProperty('open', true);
    const warning = screen.getByText(disclosure[0] ?? '');
    const rule = screen.getByRole('button', { name: 'Copy rule' });
    expect(warning.compareDocumentPosition(rule) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('renders device names isolated and as text', async () => {
    await open();
    press('CapsLock');
    const bdi = screen.getByText(`Keyboard ${rlo}evil`);
    expect(bdi.tagName).toBe('BDI');
  });

  it('keeps device access instructions available when there is one readable match', async () => {
    await open();
    press('CapsLock');
    expect(screen.queryByRole('radio')).toBeNull();
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', false);
    fireEvent.click(screen.getByText('Device access instructions'));
    expect(screen.getByRole('button', { name: 'Copy rule' })).toBeTruthy();
  });

  it('shows the file, rule, reload and revocation steps and copies only on a click; runs nothing', async () => {
    const copy = vi.spyOn(platform.services, 'copyText').mockResolvedValue();
    await open(fake({ ok: true, devices: [locked], disclosure }));
    press('CapsLock');
    expect(copy).not.toHaveBeenCalled();
    expect(screen.getByText(/Conch needs access to a keyboard/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', true);
    fireEvent.click(screen.getByText('Device access instructions'));
    expect(screen.getByText('/etc/udev/rules.d/70-conch.rules')).toBeTruthy();
    expect(screen.getByText('sudo udevadm trigger')).toBeTruthy();
    expect(screen.getByText('Delete that line from the file.')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Copy rule' }));
    expect(copy).toHaveBeenCalledWith('KERNEL=="event5", TAG+="uaccess"');
    await screen.findByText('Copied the rule.');
    // An unreadable device cannot be chosen.
    expect(screen.queryByRole('radio', { name: /Locked board/ })).toBeNull();
  });

  it('Check again lists afresh', async () => {
    const f = await open();
    press('CapsLock');
    fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
    await waitFor(() => { expect(f.keyDevices).toHaveBeenCalledTimes(2); });
  });

  it('captures one key and ignores later keydowns', async () => {
    const f = await open();
    expect(screen.getByText('none chosen')).toBeTruthy();
    press('Unidentified');
    expect(screen.getByText(cantUse)).toBeTruthy();
    press('CapsLock');
    expect(screen.getByText('Caps Lock', { selector: 'bdi' })).toBeTruthy();
    // Capture has ended: the next key changes nothing.
    press('KeyQ');
    expect(screen.getByText('Caps Lock', { selector: 'bdi' })).toBeTruthy();
    expect(f.saveTalkBinding).not.toHaveBeenCalled();
  });

  it('Escape closes setup during automatic capture', async () => {
    const f = await open();
    const close = screen.getByRole('button', { name: 'Close' });
    close.focus();
    // Capture must consume Escape before it reaches even a native target
    // listener. The setup's React handler also closes it when capture is off.
    const reachedTarget = vi.fn();
    close.addEventListener('keydown', reachedTarget);
    fireEvent.keyDown(close, { key: 'Escape', code: 'Escape' });
    expect(reachedTarget).not.toHaveBeenCalled();
    expect(f.onClose).toHaveBeenCalledOnce();
    expect(f.saveTalkBinding).not.toHaveBeenCalled();
  });

  it('Tab leaves automatic capture without consuming navigation or binding a key', async () => {
    await open();
    const tab = new KeyboardEvent('keydown', { code: 'Tab', cancelable: true });
    fireEvent(window, tab);
    expect(tab.defaultPrevented).toBe(false);
    expect(screen.getByText('none chosen')).toBeTruthy();
    expect(screen.queryByText(/Press the key/)).toBeNull();
  });

  it('Shift+Tab leaves automatic capture without binding Shift', async () => {
    await open();
    const target = screen.getByRole('button', { name: 'Cancel' });
    target.focus();
    fireEvent.keyDown(target, { code: 'ShiftLeft', shiftKey: true });
    const tab = new KeyboardEvent('keydown', { code: 'Tab', shiftKey: true, bubbles: true, cancelable: true });
    fireEvent(target, tab);
    fireEvent.keyUp(target, { code: 'ShiftLeft' });
    expect(tab.defaultPrevented).toBe(false);
    expect(screen.getByText('none chosen')).toBeTruthy();
    expect(screen.queryByText(/Press the key/)).toBeNull();
  });

  it('binds Shift when pressed and released on its own', async () => {
    const f = await open();
    fireEvent.keyDown(window, { code: 'ShiftRight', shiftKey: true });
    expect(screen.getByText('none chosen')).toBeTruthy();
    fireEvent.keyUp(window, { code: 'ShiftRight' });
    expect(screen.getByText('Right Shift', { selector: 'bdi' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => { expect(f.saveTalkBinding).toHaveBeenCalledWith({ deviceId: 'k1', code: 54 }); });
  });

  it('captures Mouse 4, and the first buttons stay clicks', async () => {
    await open();
    fireEvent.mouseDown(window, { button: 0 });
    expect(screen.getByText(/Press the key/)).toBeTruthy();
    fireEvent.mouseDown(window, { button: 3 });
    expect(screen.getByText('Mouse 4', { selector: 'bdi' })).toBeTruthy();
  });

  it('asks for a matching device only when multiple readable devices match', async () => {
    const second = { ...kbd, id: 'k3', name: 'Second keyboard' };
    const f = await open(fake({ ok: true, devices: [kbd, mouse, second], disclosure }));
    press('CapsLock');
    expect(screen.queryByRole('radio', { name: 'Mouse' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', true);
    fireEvent.click(screen.getByRole('radio', { name: 'Second keyboard' }));
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', false);
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => { expect(f.saveTalkBinding).toHaveBeenCalledWith({ deviceId: 'k3', code: 58 }); });
    await screen.findByText(/Saved: hold Caps Lock/);
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'Mouse 4' } });
    expect(screen.queryByRole('radio')).toBeNull();
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', false);
  });

  it('saves through voiceSaveTalkBinding and says when it takes effect', async () => {
    const f = await open(fake(undefined, { ok: true }), true);
    press('CapsLock');
    expect(screen.queryByRole('radio')).toBeNull();
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Save' })); await Promise.resolve(); });
    await waitFor(() => { expect(f.saveTalkBinding).toHaveBeenCalledWith({ deviceId: 'k1', code: 58 }); });
    expect(await screen.findByText(new RegExp(`hold Caps Lock to talk. ${takeEffect}`))).toBeTruthy();
    expect(f.onSaved).toHaveBeenCalledWith('Caps Lock');
  });

  it('does not mention joining when not joined', async () => {
    await open();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'F13' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    const saved = await screen.findByText(/Saved: hold F13 to talk\./);
    expect(saved.textContent).not.toContain(takeEffect);
  });

  it('shows which step failed', async () => {
    const f = await open(fake(undefined, { ok: false, error: 'invalid', failed: 'talk' }));
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'F13' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/The key was not saved\./)).toBeTruthy();
    expect(f.onSaved).not.toHaveBeenCalled();
    expect(failedWords({ ok: false, error: 'busy', failed: 'device' })).toMatch(/^The device was not saved\./);
  });

  it('guides recovery from an untrusted helper and keeps the binding available to retry', async () => {
    const f = await open(fake(undefined, { ok: false, error: 'untrusted', failed: 'device' }));
    const key = screen.getByRole<HTMLSelectElement>('combobox');
    const save = screen.getByRole<HTMLButtonElement>('button', { name: 'Save' });
    fireEvent.change(key, { target: { value: 'F13' } });
    fireEvent.click(save);

    expect(await screen.findByText(/relaunch your development build from a folder other users cannot change/)).toBeTruthy();
    expect(screen.getByText(/Keyboard .*evil/)).toBeTruthy();
    expect(key.value).toBe('F13');
    expect(save.disabled).toBe(false);
    expect(f.onSaved).not.toHaveBeenCalled();

    f.saveTalkBinding.mockResolvedValueOnce({ ok: true });
    fireEvent.click(save);
    expect(await screen.findByText(/Saved: hold F13 to talk/)).toBeTruthy();
    expect(f.saveTalkBinding.mock.calls).toEqual([[{ deviceId: 'k1', code: 183 }], [{ deviceId: 'k1', code: 183 }]]);
  });

  it('ignores keys while loading and when the listing fails', async () => {
    render(<TalkKeySetup source={fake({ ok: false, error: 'not-installed' }).nv} joined={false} onSaved={() => undefined} onClose={() => undefined} />);
    const pendingKey = new KeyboardEvent('keydown', { code: 'KeyQ', cancelable: true });
    fireEvent(window, pendingKey);
    expect(pendingKey.defaultPrevented).toBe(false);
    expect(await screen.findByText(/not installed/)).toBeTruthy();
    expect(screen.queryByText(/Press the key/)).toBeNull();
    const failedKey = new KeyboardEvent('keydown', { code: 'KeyQ', cancelable: true });
    fireEvent(window, failedKey);
    expect(failedKey.defaultPrevented).toBe(false);
  });

  it('leaves Windows capture off until requested', async () => {
    const f = fake({ ok: true, platform: 'win32', devices: [], disclosure: [] });
    const view: NativeVoiceView = { status: 'idle', generation: 0, channel: null, retryable: false };
    const source: NativeVoice = { ...f.nv, snapshot: () => view, subscribe: () => () => undefined };
    render(<TalkKeySetup source={source} joined={false} onSaved={() => undefined} onClose={() => undefined} />);
    await screen.findByRole('button', { name: 'Set talk key' });
    const key = new KeyboardEvent('keydown', { code: 'KeyQ', cancelable: true });
    fireEvent(window, key);
    expect(key.defaultPrevented).toBe(false);
    expect(screen.getByText('none chosen')).toBeTruthy();
  });

  it('says no device was found, with no access line, for a key and for a mouse button', async () => {
    await open(fake({ ok: true, devices: [mouse], disclosure }));
    press('CapsLock');
    expect(screen.getByText(/No keyboard was found/)).toBeTruthy();
    expect(screen.queryByText(/Conch needs access/)).toBeNull();
    expect(screen.queryByText('Device access instructions')).toBeNull();
    // With no device there is nothing to save the key against.
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Save' }).disabled).toBe(true);
    cleanup();
    await open(fake({ ok: true, devices: [kbd], disclosure }));
    fireEvent.mouseDown(window, { button: 4 });
    expect(screen.getByText('Mouse 5', { selector: 'bdi' })).toBeTruthy();
    expect(screen.getByText(/No mouse was found/)).toBeTruthy();
    expect(screen.queryByText(/Conch needs access/)).toBeNull();
  });

  it('lists each unreadable device in its own details', async () => {
    const other = { ...locked, id: 'k4', name: 'Other locked' };
    await open(fake({ ok: true, devices: [locked, other], disclosure }));
    press('CapsLock');
    fireEvent.click(screen.getByText('Device access instructions'));
    expect(screen.getAllByText('Locked board', { selector: 'summary bdi' })).toHaveLength(1);
    fireEvent.click(screen.getByText('Other locked', { selector: 'summary bdi' }));
    expect(screen.getAllByRole('button', { name: 'Copy rule' })).toHaveLength(2);
  });

  it('ends capture when the select is clicked or focused', async () => {
    await open();
    fireEvent.mouseDown(screen.getByRole('combobox'), { button: 0 });
    expect(screen.queryByText(/Press the key/)).toBeNull();
    press('CapsLock');
    expect(screen.getByText('none chosen')).toBeTruthy();
    cleanup();
    await open();
    fireEvent.focus(screen.getByRole('combobox'));
    expect(screen.queryByText(/Press the key/)).toBeNull();
  });

  it('forgets a pending Shift on window blur and on a click', async () => {
    await open();
    fireEvent.keyDown(window, { code: 'ShiftLeft' });
    fireEvent.blur(window);
    fireEvent.keyUp(window, { code: 'ShiftLeft' });
    expect(screen.getByText('none chosen')).toBeTruthy();
    fireEvent.keyDown(window, { code: 'ShiftLeft' });
    fireEvent.mouseDown(window, { button: 0 });
    fireEvent.keyUp(window, { code: 'ShiftLeft' });
    expect(screen.getByText('none chosen')).toBeTruthy();
    expect(screen.getByText(/Press the key/)).toBeTruthy();
  });
});
