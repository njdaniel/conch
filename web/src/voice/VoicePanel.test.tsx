// The voice strip says where it goes and where it is (issue #584): Join voice
// and Watch only name the channel they join, Watch only says the room is
// heard, and the channel voice is in is a way back to it while another is on
// screen. Driven by a fake VoiceControl: no SDK, no network, no device.
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import type { VoicePresenceV1 } from '../schema/voice';
import { initialVoice, VoiceClient, type Phase, type VoiceState } from './connection';
import { FakeClock } from './fakes';
import type { VoiceControl } from './useVoice';
import { VoicePanel } from './VoicePanel';

const presence: VoicePresenceV1 = { schema: 'conch.voice_presence.v1', channel_id: 1, configured: true, available: true, rooms: [] };

// A client that is never asked to connect: the panel reads the state it is
// given, and join and leave are recorded instead.
function control(state: VoiceState, calls: string[]): VoiceControl {
  const client = new VoiceClient({
    clock: new FakeClock(),
    random: () => 0.5,
    createSession: () => new Promise(() => undefined),
    report: () => Promise.resolve('ok'),
    newRoom: () => {
      throw new Error('no room in this test');
    },
    claimShare: () => new Promise(() => undefined),
    stopShare: () => Promise.resolve(),
  });
  return {
    state,
    snapshot: { kind: 'presence', presence },
    shown: state.channel,
    talkKey: 'Backquote',
    capturing: false,
    secure: true,
    features: { canShare: true, chromium: true, platform: 'linux' },
    client,
    setTalkKey: () => undefined,
    capture: () => undefined,
    join: () => {
      calls.push('join');
    },
    watchOnly: () => {
      calls.push('watch only');
    },
    leave: () => {
      calls.push('leave');
    },
  };
}

function panel(state: VoiceState, viewed: string | null) {
  const calls: string[] = [];
  const opened: string[] = [];
  render(
    <VoicePanel
      voice={control(state, calls)}
      viewer={7}
      people={new Map()}
      viewed={viewed}
      onOpen={(name) => {
        opened.push(name);
      }}
    />,
  );
  return { calls, opened, voice: screen.getByRole('region', { name: 'Voice' }) };
}

const inVoice = (phase: Phase, channel: string): VoiceState => ({ ...initialVoice, phase, channel });

afterEach(() => {
  cleanup();
});

describe('Join voice and Watch only', () => {
  it('name the channel on screen, which is the room they join', () => {
    const { voice, calls } = panel(initialVoice, 'general');
    const join = within(voice).getByRole('button', { name: 'Join voice in #general' });
    const watch = within(voice).getByRole('button', { name: 'Watch only in #general' });
    // The channel is its own span, hidden on a phone and still read out
    // there, so the verbs stay what is seen and the names stay whole.
    for (const button of [join, watch]) expect(button.querySelector('.dest')?.textContent).toBe('in #general');
    fireEvent.click(join);
    fireEvent.click(watch);
    expect(calls).toEqual(['join', 'watch only']);
  });

  it('name no room while no channel is on screen', () => {
    const { voice } = panel(initialVoice, null);
    expect(voice.querySelector('.dest')).toBeNull();
    expect(voice.textContent).not.toContain('in #');
  });

  it('follow the channel on screen', () => {
    const { voice } = panel(initialVoice, 'book-club');
    expect(within(voice).getByRole('button', { name: 'Join voice in #book-club' })).toBeTruthy();
    expect(within(voice).getByRole('button', { name: 'Watch only in #book-club' })).toBeTruthy();
  });

  it('Watch only is described, in a hint on screen, as keeping the microphone off and hearing the room', () => {
    const { voice } = panel(initialVoice, 'general');
    const said = 'Microphone off. You hear the room and can watch a share.';
    const watch = within(voice).getByRole('button', { name: 'Watch only in #general', description: said });
    const hint = document.getElementById(watch.getAttribute('aria-describedby') ?? '');
    expect(hint?.textContent).toBe(said);
    expect(hint?.classList.contains('hint')).toBe(true);
    expect(voice.contains(hint)).toBe(true);
    // Join voice opens the microphone; it is not described so.
    expect(within(voice).getByRole('button', { name: 'Join voice in #general' }).hasAttribute('aria-describedby')).toBe(false);
  });
});

describe('the channel voice is in', () => {
  for (const [phase, label] of [
    ['connected', 'Connected'],
    ['reconnecting', 'Reconnecting'],
    ['unavailable', 'Voice unavailable'],
  ] as const) {
    it(`is a button back to it while another channel is on screen (${phase})`, () => {
      const { voice, opened, calls } = panel(inVoice(phase, 'a'), 'b');
      expect(voice.querySelector('.voice-phase')?.textContent).toBe(`${label} in #a`);
      fireEvent.click(within(voice).getByRole('button', { name: '#a' }));
      expect(opened).toEqual(['a']);
      // Opening the channel is all it does: no join, no leave.
      expect(calls).toEqual([]);
    });

    it(`is plain text while it is the channel on screen (${phase})`, () => {
      const { voice } = panel(inVoice(phase, 'a'), 'a');
      expect(voice.querySelector('.voice-phase')?.textContent).toBe(`${label} in #a`);
      expect(within(voice).queryByRole('button', { name: '#a' })).toBeNull();
      expect(voice.querySelector('.voice-phase button')).toBeNull();
    });
  }

  it('is a way back when no channel is on screen', () => {
    const { voice, opened } = panel(inVoice('connected', 'a'), null);
    fireEvent.click(within(voice).getByRole('button', { name: '#a' }));
    expect(opened).toEqual(['a']);
  });
});
