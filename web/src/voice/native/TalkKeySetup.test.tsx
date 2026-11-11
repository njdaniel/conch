// The talk-key setup (issue #1382, D3-11), driven by a fake native voice.
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { platform } from '../../platform';
import type { VoiceKeyDevice, VoiceKeyDevicesResult, VoiceTalkBindingResult } from '../../platform/desktop/bridge';
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
  await screen.findByText(disclosure[0] ?? '');
  return { ...f, onSaved, onClose };
}
const accept = () => { fireEvent.click(screen.getByRole('button', { name: 'I understand' })); };

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('TalkKeySetup', () => {
  it('shows the exposure before any device or rule, and lists nothing until accepted', async () => {
    await open();
    expect(screen.queryByText(/Locked board/)).toBeNull();
    expect(screen.queryByText(/Mouse \(/)).toBeNull();
    accept();
    const text = screen.getByText(disclosure[1] ?? '');
    const device = screen.getAllByText('Locked board')[0] as HTMLElement;
    const rule = screen.getByText('KERNEL=="event5", TAG+="uaccess"');
    expect(text.compareDocumentPosition(device) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(device.compareDocumentPosition(rule) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('renders device names isolated and as text', async () => {
    await open();
    accept();
    const bdi = screen.getByText(`Keyboard ${rlo}evil`);
    expect(bdi.tagName).toBe('BDI');
  });

  it('shows the file, rule, reload and revocation steps and copies only on a click; runs nothing', async () => {
    const copy = vi.spyOn(platform.services, 'copyText').mockResolvedValue();
    await open();
    accept();
    expect(copy).not.toHaveBeenCalled();
    expect(screen.getByText('/etc/udev/rules.d/70-conch.rules')).toBeTruthy();
    expect(screen.getByText('sudo udevadm trigger')).toBeTruthy();
    expect(screen.getByText('Delete that line from the file.')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Copy rule' }));
    expect(copy).toHaveBeenCalledWith('KERNEL=="event5", TAG+="uaccess"');
    await screen.findByText('Copied the rule.');
    // An unreadable device cannot be chosen.
    expect(screen.getByRole('radio', { name: /Locked board/ })).toHaveProperty('disabled', true);
  });

  it('Check again lists afresh', async () => {
    const f = await open();
    accept();
    fireEvent.click(screen.getAllByRole('button', { name: 'Check again' })[0] as HTMLElement);
    await waitFor(() => { expect(f.keyDevices).toHaveBeenCalledTimes(2); });
  });

  it('captures only between Set talk key and the key; a keydown outside calls nothing', async () => {
    const f = await open();
    accept();
    fireEvent.keyDown(window, { code: 'KeyQ' });
    expect(screen.getByText('none chosen')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Set talk key' }));
    fireEvent.keyDown(window, { code: 'Unidentified' });
    expect(screen.getByText(cantUse)).toBeTruthy();
    fireEvent.keyDown(window, { code: 'CapsLock' });
    expect(screen.getByText('Caps Lock', { selector: 'bdi' })).toBeTruthy();
    // Capture has ended: the next key changes nothing.
    fireEvent.keyDown(window, { code: 'KeyQ' });
    expect(screen.getByText('Caps Lock', { selector: 'bdi' })).toBeTruthy();
    expect(f.saveTalkBinding).not.toHaveBeenCalled();
  });

  it('Escape cancels the capture', async () => {
    await open();
    accept();
    fireEvent.click(screen.getByRole('button', { name: 'Set talk key' }));
    fireEvent.keyDown(window, { code: 'Escape' });
    expect(screen.getByText('none chosen')).toBeTruthy();
    expect(screen.queryByText(/Press the key/)).toBeNull();
  });

  it('captures Mouse 4, and the first buttons stay clicks', async () => {
    await open();
    accept();
    fireEvent.click(screen.getByRole('button', { name: 'Set talk key' }));
    fireEvent.mouseDown(window, { button: 0 });
    expect(screen.getByText(/Press the key/)).toBeTruthy();
    fireEvent.mouseDown(window, { button: 3 });
    expect(screen.getByText('Mouse 4', { selector: 'bdi' })).toBeTruthy();
  });

  it('says a key needs a keyboard and a mouse button a mouse', async () => {
    await open();
    accept();
    fireEvent.click(screen.getByRole('radio', { name: /Mouse/ }));
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'Caps Lock' } });
    expect(screen.getByText(/needs a keyboard device/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', true);
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'Mouse 4' } });
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', false);
    fireEvent.click(screen.getByRole('radio', { name: /Keyboard/ }));
    expect(screen.getByText(/needs a mouse device/)).toBeTruthy();
  });

  it('saves through voiceSaveTalkBinding and says when it takes effect', async () => {
    const f = await open(fake(undefined, { ok: true }), true);
    accept();
    fireEvent.click(screen.getByRole('radio', { name: /Keyboard/ }));
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'Caps Lock' } });
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Save' })); await Promise.resolve(); });
    await waitFor(() => { expect(f.saveTalkBinding).toHaveBeenCalledWith({ deviceId: 'k1', code: 58 }); });
    expect(await screen.findByText(new RegExp(`hold Caps Lock to talk. ${takeEffect}`))).toBeTruthy();
    expect(f.onSaved).toHaveBeenCalledWith('Caps Lock');
  });

  it('does not mention joining when not joined', async () => {
    await open();
    accept();
    fireEvent.click(screen.getByRole('radio', { name: /Keyboard/ }));
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'F13' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    const saved = await screen.findByText(/Saved: hold F13 to talk\./);
    expect(saved.textContent).not.toContain(takeEffect);
  });

  it('shows which step failed', async () => {
    const f = await open(fake(undefined, { ok: false, error: 'invalid', failed: 'talk' }));
    accept();
    fireEvent.click(screen.getByRole('radio', { name: /Keyboard/ }));
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'F13' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/The key was not saved\./)).toBeTruthy();
    expect(f.onSaved).not.toHaveBeenCalled();
    expect(failedWords({ ok: false, error: 'busy', failed: 'device' })).toMatch(/^The device was not saved\./);
  });

  it('says so when the listing fails', async () => {
    render(<TalkKeySetup source={fake({ ok: false, error: 'not-installed' }).nv} joined={false} onSaved={() => undefined} onClose={() => undefined} />);
    expect(await screen.findByText(/not installed/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'I understand' })).toBeNull();
  });
});
