// The strip's one status phrase, "<mode>: <what others hear>" (issue #2198,
// UI-talk-3): each talk mode and state says one thing, "Sending" is the only
// highlighted state and is said in words, and the microphone is no chip.
import { cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { initialVoice, type VoiceState } from './connection';
import { transmitLabel, VoiceStatus } from './VoiceStatus';

afterEach(cleanup);

const connected = (patch: Partial<VoiceState> = {}): VoiceState => ({ ...initialVoice, phase: 'connected', channel: 'c', mode: 'ptt', ...patch });
const line = (state: VoiceState) => {
  const view = render(<VoiceStatus state={state} />);
  const text = view.container.querySelector('.voice-tx')?.textContent ?? '';
  const chips = [...view.container.querySelectorAll('.chip')].map((c) => c.textContent);
  view.unmount();
  return { text, chips };
};

describe('the status phrase', () => {
  it.each([
    ['push to talk, idle', {}, 'Push to talk: not sending'],
    ['push to talk, key held, not yet sending', { talking: true }, 'Waiting for the server'],
    ['push to talk, key held and sending', { talking: true, transmitting: true }, 'Sending'],
    ['open mic, sending', { mode: 'open', transmitting: true }, 'Open mic: sending'],
    ['open mic, below the level', { mode: 'open' }, 'Open mic: not sending'],
    ['muted, push to talk', { muted: true }, 'Muted: not sending'],
    ['muted, open mic', { mode: 'open', muted: true }, 'Muted: not sending'],
    ['muted with the key held', { muted: true, talking: true }, 'Muted: not sending'],
    ['deafened', { muted: true, deafened: true }, 'Deafened (you hear no one, and you are muted). Muted: not sending'],
    ['watch only', { watchOnly: true }, 'Watch only: your microphone is off.'],
    ['listen only', { listenOnly: true }, 'Listen only.'],
    ['a viewer beside another device', { viewer: true }, 'Watching beside your voice on another device.'],
  ] as const)('%s', (_what, patch, said) => {
    const { text } = line(connected(patch));
    expect(text).toBe(said);
    expect(text).not.toMatch(/transmitting/i);
  });

  it('keeps Sending the only highlighted state, in words, not by a class alone', () => {
    expect(transmitLabel({ talking: true, transmitting: true, mode: 'ptt', muted: false, deafened: false })).toBe('Sending');
    for (const state of [{ talking: false, transmitting: false }, { talking: true, transmitting: false }]) {
      expect(transmitLabel({ ...state, mode: 'ptt', muted: false, deafened: false })).not.toMatch(/^(Open mic: )?sending$/i);
    }
  });

  it('draws no Microphone in use chip while the microphone is held, in any state', () => {
    for (const patch of [{}, { mode: 'open' }, { muted: true }, { talking: true, transmitting: true }] as const) {
      const { chips } = line(connected(patch));
      expect(chips).not.toContain('Microphone in use');
    }
  });

  it('keeps the compact header\'s tooltip saying the microphone is in use', () => {
    const view = render(<VoiceStatus state={connected()} showPhase />);
    expect(view.container.querySelector('.voice-sub')?.getAttribute('title')).toBe('Microphone in use · Push to talk: not sending');
    expect(view.container.textContent).not.toContain('Microphone in use');
  });
});
