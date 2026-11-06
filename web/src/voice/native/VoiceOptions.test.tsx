// Voice options of the desktop strip (issue #1379, D3-8 of #771), driven by a
// fake native voice: no helper, no bridge, no network.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import type { NativeVoiceDevicesResult, NativeVoiceHelper, NativeVoiceView, VoiceCommandResult, VoiceSaveResult } from '../../platform/desktop/bridge';
import type { NativeVoice } from '../../platform/types';
import type { VoiceControlPTT, VoiceControlPTTProblem, VoiceDevice } from '../../schema/generated';
import { pttProblemWords, pttWords } from './words';
import { micMissing, nextTime, noPipeWire, openMeans, speakersMissing, VoiceOptions } from './VoiceOptions';

afterEach(cleanup);

const idle: NativeVoiceView = { status: 'idle', generation: 0, channel: null, retryable: false };
const helper = (over: Partial<NativeVoiceHelper> = {}): NativeVoiceHelper => ({ version: 2, phase: 'connected', capture: 'on', playback: 'on', transmitting: false, muted: false, deafened: false, blocked: [], talkMode: 'push_to_talk', ptt: 'ready', ...over });
const running = (over: Partial<NativeVoiceHelper> = {}): NativeVoiceView => ({ status: 'running', generation: 1, channel: 'general', retryable: false, helper: helper(over) });

const dev = (name: string, description: string, def = false): VoiceDevice => ({ name, description, default: def });
const mics = [dev('alsa_input.usb', 'USB Mic', true), dev('alsa_input.pci', 'Built-in Mic')];
const outs = [dev('alsa_output.pci', 'Speakers', true), dev('alsa_output.usb', 'Headset')];
const listed: NativeVoiceDevicesResult = { ok: true, pipewire: 'ok', synced: true, microphones: mics, speakers: outs };

class Fake {
  readonly calls: string[] = [];
  deviceAck: ((r: VoiceCommandResult) => void) | null = null;
  list: NativeVoiceDevicesResult = listed;
  saved: VoiceSaveResult = { ok: true };
  readonly nv: NativeVoice = {
    snapshot: () => idle,
    subscribe: () => () => undefined,
    join: () => Promise.reject(new Error('no')),
    leave: () => Promise.reject(new Error('no')),
    retry: () => Promise.reject(new Error('no')),
    setMute: () => Promise.resolve({ ok: true }),
    setDeafen: () => Promise.resolve({ ok: true }),
    setDevices: (d) => {
      this.calls.push(`setDevices ${JSON.stringify(d)}`);
      return new Promise((resolve) => {
        this.deviceAck = resolve;
      });
    },
    setTalkMode: (m) => {
      this.calls.push(`setTalkMode ${m}`);
      return Promise.resolve({ ok: true });
    },
    devices: () => Promise.resolve(this.list),
    saveDevice: (r) => {
      this.calls.push(`save ${r.key}=${r.value}`);
      return Promise.resolve(this.saved);
    },
  };
}

async function open(fake: Fake, view: NativeVoiceView = idle) {
  const r = render(<VoiceOptions source={fake.nv} view={view} open />);
  await waitFor(() => { expect(screen.queryByText('Listing devices…')).toBeNull(); });
  return r;
}

const mic = () => screen.getByLabelText<HTMLSelectElement>('Microphone');
const speakers = () => screen.getByLabelText<HTMLSelectElement>('Speakers');
const texts = (s: HTMLSelectElement) => Array.from(s.options).map((o) => o.text.replace(/[\u2068\u2069]/gu, ''));

describe('VoiceOptions devices', () => {
  it('lists System default, then the microphones and speakers, marking the defaults', async () => {
    await open(new Fake());
    expect(texts(mic())).toEqual(['Choose microphone', 'System default', 'USB Mic (default)', 'Built-in Mic']);
    expect(texts(speakers())).toEqual(['Choose speakers', 'System default', 'Speakers (default)', 'Headset']);
  });

  it('says PipeWire is not available instead of listing', async () => {
    const fake = new Fake();
    fake.list = { ok: true, pipewire: 'unavailable', synced: true, microphones: [], speakers: [] };
    await open(fake);
    expect(screen.getByText(noPipeWire)).toBeTruthy();
    expect(screen.queryByLabelText('Microphone')).toBeNull();
  });

  it('says why the list failed and offers a retry', async () => {
    const fake = new Fake();
    fake.list = { ok: false, error: 'timed-out' };
    await open(fake);
    expect(screen.getByText(/Devices could not be listed: The voice helper did not answer in time/u)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy();
  });

  it('while not joined only saves, with the name or default', async () => {
    const fake = new Fake();
    await open(fake);
    fireEvent.change(mic(), { target: { value: 'alsa_input.pci' } });
    await waitFor(() => { expect(mic().value).toBe('alsa_input.pci'); });
    fireEvent.change(speakers(), { target: { value: 'default' } });
    await waitFor(() => { expect(speakers().value).toBe('default'); });
    expect(fake.calls).toEqual(['save audio.input=alsa_input.pci', 'save audio.output=default']);
  });

  it('while joined shows switching until the ack, applies, then saves and shows the confirmed capture', async () => {
    const fake = new Fake();
    const view = running();
    const r = await open(fake, view);
    fireEvent.change(mic(), { target: { value: 'alsa_input.pci' } });
    expect(await screen.findByText('switching…')).toBeTruthy();
    expect(fake.calls).toEqual(['setDevices {"input":"alsa_input.pci"}']);
    await act(async () => {
      fake.deviceAck?.({ ok: true });
      await Promise.resolve();
    });
    await waitFor(() => { expect(fake.calls).toContain('save audio.input=alsa_input.pci'); });
    expect(screen.queryByText('switching…')).toBeNull();
    r.rerender(<VoiceOptions source={fake.nv} view={running({ capture: 'on' })} open />);
    expect(screen.getByText('Microphone is on')).toBeTruthy();
    expect(screen.getByText('Speakers are on')).toBeTruthy();
  });

  it('does not save a choice the helper refused', async () => {
    const fake = new Fake();
    await open(fake, running());
    fireEvent.change(speakers(), { target: { value: 'alsa_output.usb' } });
    await waitFor(() => { expect(fake.deviceAck).not.toBeNull(); });
    await act(async () => {
      fake.deviceAck?.({ ok: false, error: 'busy' });
      await Promise.resolve();
    });
    expect(await screen.findByText('Voice could not switch; try again.')).toBeTruthy();
    expect(fake.calls.filter((c) => c.startsWith('save'))).toEqual([]);
  });

  it('a microphone missing after the switch is shown as missing, with no alternative', async () => {
    const fake = new Fake();
    await open(fake, running({ capture: 'unavailable', blocked: ['no_microphone'] }));
    expect(screen.getByText(micMissing)).toBeTruthy();
    expect(screen.queryByText(speakersMissing)).toBeNull();
    // Nothing was chosen on the person's behalf.
    expect(fake.calls).toEqual([]);
  });

  it('a chosen device the list lacks is shown as not found and not replaced', async () => {
    const fake = new Fake();
    fake.list = { ok: true, pipewire: 'ok', synced: true, microphones: [mics[0] as VoiceDevice], speakers: outs };
    await open(fake);
    // Choose it while listed, then it leaves the list on refresh.
    fireEvent.change(mic(), { target: { value: 'alsa_input.usb' } });
    await waitFor(() => { expect(mic().value).toBe('alsa_input.usb'); });
    fake.list = { ok: true, pipewire: 'ok', synced: true, microphones: [mics[1] as VoiceDevice], speakers: outs };
    fireEvent.click(screen.getByRole('button', { name: 'Refresh the list' }));
    await waitFor(() => { expect(texts(mic())).toContain('alsa_input.usb (not found)'); });
    expect(mic().value).toBe('alsa_input.usb');
    expect(screen.getByText(micMissing)).toBeTruthy();
  });

  it('a v1 helper takes the choices next time and only saves', async () => {
    const fake = new Fake();
    await open(fake, running({ version: 1, playback: undefined, talkMode: undefined }));
    expect(screen.getAllByText(new RegExp(nextTime, 'u')).length).toBeGreaterThan(0);
    fireEvent.change(mic(), { target: { value: 'alsa_input.pci' } });
    await waitFor(() => { expect(fake.calls).toEqual(['save audio.input=alsa_input.pci']); });
  });

  it('isolates a name carrying bidi format characters', async () => {
    const rlo = String.fromCharCode(0x202e);
    const fake = new Fake();
    fake.list = { ok: true, pipewire: 'ok', synced: true, microphones: [dev('evil', `Mic ${rlo}gnp.exe`)], speakers: [dev('s', 'Speakers')] };
    await open(fake);
    const option = Array.from(mic().options).find((o) => o.value === 'evil');
    expect(option?.text.includes(rlo)).toBe(false);
    expect(option?.text.startsWith(String.fromCharCode(0x2068))).toBe(true);
    expect(option?.text.endsWith(String.fromCharCode(0x2069))).toBe(true);
    fireEvent.change(mic(), { target: { value: 'evil' } });
    const chosen = await screen.findByText(/Chosen:/u);
    const bdi = chosen.querySelector('bdi');
    expect(bdi?.textContent).toBe('Mic gnp.exe');
    expect(chosen.textContent.includes(rlo)).toBe(false);
  });
});

describe('VoiceOptions talk mode', () => {
  it('shows the helper confirmed mode and changes to push to talk at once', async () => {
    const fake = new Fake();
    await open(fake, running({ talkMode: 'open' }));
    const ptt = screen.getByRole<HTMLInputElement>('radio', { name: 'Push to talk' });
    expect(screen.getByRole<HTMLInputElement>('radio', { name: 'Open microphone' }).checked).toBe(true);
    fireEvent.click(ptt);
    await waitFor(() => { expect(fake.calls).toEqual(['setTalkMode push_to_talk', 'save audio.talk_mode=push_to_talk']); });
  });

  it('open microphone says what it means and needs a confirm', async () => {
    const fake = new Fake();
    await open(fake, running());
    fireEvent.click(screen.getByRole('radio', { name: 'Open microphone' }));
    const box = screen.getByRole('alertdialog');
    expect(within(box).getByText(new RegExp(openMeans, 'u'))).toBeTruthy();
    expect(fake.calls).toEqual([]);
    fireEvent.click(within(box).getByRole('button', { name: 'Cancel' }));
    expect(fake.calls).toEqual([]);
    expect(screen.queryByRole('alertdialog')).toBeNull();
    fireEvent.click(screen.getByRole('radio', { name: 'Open microphone' }));
    fireEvent.click(screen.getByRole('button', { name: 'Use open microphone' }));
    await waitFor(() => { expect(fake.calls).toEqual(['setTalkMode open', 'save audio.talk_mode=open']); });
  });

  it('while not joined only saves the mode', async () => {
    const fake = new Fake();
    await open(fake);
    fireEvent.click(screen.getByRole('radio', { name: 'Push to talk' }));
    await waitFor(() => { expect(fake.calls).toEqual(['save audio.talk_mode=push_to_talk']); });
  });

  it('no talk-key problem state changes the talk mode', async () => {
    const ptts = Object.keys(pttWords) as VoiceControlPTT[];
    const problems = Object.keys(pttProblemWords) as VoiceControlPTTProblem[];
    expect(ptts.length).toBeGreaterThan(0);
    expect(problems.length).toBeGreaterThan(0);
    const fake = new Fake();
    for (const ptt of ptts) {
      for (const problem of [undefined, ...problems]) {
        const { unmount } = render(<VoiceOptions source={fake.nv} view={running({ ptt, pttProblem: problem, blocked: ['key_device_lost'] })} open />);
        await waitFor(() => { expect(screen.queryByText('Listing devices…')).toBeNull(); });
        unmount();
      }
    }
    expect(fake.calls).toEqual([]);
  });
});
