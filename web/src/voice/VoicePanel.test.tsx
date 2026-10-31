// The voice strip says where it goes and where it is (issue #584): Join voice
// and Watch only name the channel they join, Watch only says the room is
// heard, and the channel voice is in is a way back to it while another is on
// screen. Driven by a fake VoiceControl: no SDK, no network, no device.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ChannelV0 } from '../schema/channel';
import type { VoicePresenceV1 } from '../schema/voice';
import type { Person } from '../useLiveChannel';
import { initialVoice, VoiceClient, type Phase, type Processing, type VoiceState } from './connection';
import { FakeClock } from './fakes';
import { ownPageNote, soundNote, type ShareFeatures } from './share';
import { MicTestShown, VoiceLive, VoiceStrip } from '../phone';
import type { Snapshot, VoiceControl } from './useVoice';
import { followsNote, missingNote, noOtherOutputs, noOutputChoice, processingLabel, processingOffNote, processingUseNote, reportedNote, stoppedFor, thresholdHint, VoicePanel } from './VoicePanel';

const chrome: ShareFeatures = { canShare: true, chromium: true, platform: 'linux' };
const presence: VoicePresenceV1 = { schema: 'conch.voice_presence.v1', channel_id: 1, configured: true, available: true, rooms: [] };

// A client that is never asked to connect: the panel reads the state it is
// given, and join and leave are recorded instead.
function control(state: VoiceState, calls: string[], snapshot: Snapshot): VoiceControl {
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
    snapshot,
    shown: state.channel,
    talkKey: 'Backquote',
    capturing: false,
    secure: true,
    features: chrome,
    client,
    setTalkKey: () => undefined,
    setReleaseDelay: (ms: number) => {
      calls.push(`release ${String(ms)}`);
    },
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

function panel(state: VoiceState, viewed: string | null, snapshot: Snapshot = { kind: 'presence', presence }, people: ReadonlyMap<number, Person> = new Map()) {
  const calls: string[] = [];
  const opened: string[] = [];
  const voice = control(state, calls, snapshot);
  const { rerender } = render(
    <VoicePanel
      voice={voice}
      viewer={7}
      people={people}
      viewed={viewed}
      onOpen={(name) => {
        opened.push(name);
      }}
    />,
  );
  // Draws the same panel again with another state, as a phase change does,
  // and with another snapshot, as a presence frame does.
  let shown = snapshot;
  const redraw = (next: VoiceState, nextSnapshot: Snapshot = shown) => {
    shown = nextSnapshot;
    rerender(<VoicePanel voice={{ ...voice, state: next, snapshot: nextSnapshot }} viewer={7} people={people} viewed={viewed} onOpen={() => undefined} />);
  };
  return { calls, opened, client: voice.client, redraw, voice: screen.getByRole('region', { name: 'Voice' }) };
}

const inVoice = (phase: Phase, channel: string): VoiceState => ({ ...initialVoice, phase, channel });

afterEach(() => {
  cleanup();
});

describe('the private microphone test (issue #846)', () => {
  it('is offered before joining, and joining voice takes it away', () => {
    const { voice, redraw } = panel(initialVoice, 'general');
    expect(within(voice).getByRole('button', { name: 'Test microphone' })).toBeTruthy();
    redraw(inVoice('connecting', 'general'));
    expect(within(voice).queryByRole('button', { name: /test microphone|stop test/i })).toBeNull();
  });

  it('is offered only when Join is: not while voice is down or the server is gone', () => {
    const { voice, redraw } = panel(initialVoice, 'general');
    const test = () => within(voice).queryByRole('button', { name: /test microphone|stop test/i });
    expect(test()).not.toBeNull();
    redraw(initialVoice, { kind: 'presence', presence: { ...presence, available: false } });
    expect(test()).toBeNull();
    redraw(initialVoice, { kind: 'gone' });
    expect(test()).toBeNull();
    redraw(initialVoice, { kind: 'presence', presence });
    expect(test()).not.toBeNull();
  });
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

// Issue #643 (N07, N09): a control that cannot act says why, and a hint that
// promises what it cannot give is not shown.
describe('controls that cannot act', () => {
  const watchHint = 'Microphone off. You hear the room and can watch a share.';
  const snapshots: readonly (readonly [string, Snapshot, string | null])[] = [
    ['loading', { kind: 'loading' }, 'Checking whether voice is available.'],
    ['unreachable', { kind: 'unreachable' }, 'Voice status unavailable: the server cannot be reached.'],
    ['gone', { kind: 'gone' }, 'Voice status could not be read.'],
    ['down', { kind: 'presence', presence: { ...presence, available: false } }, 'Voice is unavailable right now; chat still works.'],
  ];

  it.each(snapshots)('does not say you hear the room while joins are disabled (%s), and says why', (_what, snapshot, reason) => {
    const { voice } = panel(initialVoice, 'c', snapshot);
    expect(voice.textContent).not.toContain('You hear the room');
    expect(voice.textContent).toContain(reason);
    const watch = within(voice).getByRole('button', { name: 'Watch only in #c' });
    expect(watch.hasAttribute('disabled')).toBe(true);
    expect(watch.hasAttribute('aria-describedby')).toBe(false);
  });

  it('shows the hint again once joins are open', () => {
    const { voice, redraw } = panel(initialVoice, 'c', { kind: 'loading' });
    expect(voice.textContent).not.toContain(watchHint);
    redraw(initialVoice, { kind: 'presence', presence });
    expect(within(voice).getByRole('button', { name: 'Watch only in #c', description: watchHint })).toBeTruthy();
  });

  const silenced = (patch: Partial<VoiceState>): VoiceState => ({ ...inVoice('connected', 'c'), ...patch });
  it.each([
    ['muted', { muted: true }, 'Hold to talk sends nothing while you are muted. Unmute to talk.'],
    ['deafened', { muted: true, deafened: true }, 'Hold to talk sends nothing while you are deafened. Undeafen to talk.'],
  ])('Hold to talk says why it will not send while %s, and a press sends nothing', (_what, patch, said) => {
    const { voice, client } = panel(silenced(patch), 'c');
    const talk = within(voice).getByRole('button', { name: 'Hold to talk', description: said });
    expect(talk.getAttribute('aria-disabled')).toBe('true');
    expect(document.getElementById('voice-talk-why')?.textContent).toBe(said);
    // The press still reaches the client, whose gate is shut while muted or
    // deafened (gate.test.ts; the browser test holds it against a live room).
    const press = vi.spyOn(client, 'press');
    fireEvent.keyDown(talk, { key: ' ', code: 'Space' });
    expect(press).toHaveBeenCalledTimes(1);
    expect(client.snapshot().transmitting).toBe(false);
  });

  it('Hold to talk is not marked off, or described, while it can send', () => {
    const { voice } = panel(inVoice('connected', 'c'), 'c');
    const talk = within(voice).getByRole('button', { name: 'Hold to talk' });
    expect(talk.hasAttribute('aria-disabled')).toBe(false);
    expect(talk.hasAttribute('aria-describedby')).toBe(false);
    expect(voice.querySelector('#voice-talk-why')).toBeNull();
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

// Issue #585: what the strip says about a channel's voice while not in it, row
// by row: nothing before the first frame, the live roster, the server out of
// reach (no roster, which may be stale), a person without voice, and a socket
// the server closed for another reason.
describe('the strip while not in voice', () => {
  const unreachable = 'Voice status unavailable: the server cannot be reached.';
  const notAllowed = 'Your account does not include voice. Ask whoever invited you.';
  const unread = 'Voice status could not be read.';
  const someone: VoicePresenceV1 = {
    ...presence,
    rooms: [{ participants: [{ principal_id: 3, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' }] }],
  };
  const lines = (voice: HTMLElement) => [unreachable, notAllowed, unread].filter((l) => voice.textContent.includes(l));
  const roster = (voice: HTMLElement) => within(voice).queryByRole('list', { name: / in voice$/ });
  // Enabled, disabled, or not there.
  const offered = (voice: HTMLElement) =>
    ['Join voice in #c', 'Watch only in #c'].map((name) => {
      const b = within(voice).queryByRole('button', { name });
      return b === null ? 'absent' : b.hasAttribute('disabled') ? 'disabled' : 'enabled';
    });

  it('says nothing yet before the first frame, and cannot be joined', () => {
    const { voice } = panel(initialVoice, 'c', { kind: 'loading' });
    expect(lines(voice)).toEqual([]);
    expect(roster(voice)).toBeNull();
    expect(offered(voice)).toEqual(['disabled', 'disabled']);
  });

  it('shows the roster of a live snapshot, and can be joined', () => {
    const { voice } = panel(initialVoice, 'c', { kind: 'presence', presence: someone });
    expect(lines(voice)).toEqual([]);
    expect(roster(voice)?.textContent).toContain('Member 3');
    expect(offered(voice)).toEqual(['enabled', 'enabled']);
  });

  it('cannot be joined on a snapshot that says voice is down', () => {
    const { voice } = panel(initialVoice, 'c', { kind: 'presence', presence: { ...presence, available: false } });
    expect(voice.textContent).toContain('Voice is unavailable right now; chat still works.');
    expect(offered(voice)).toEqual(['disabled', 'disabled']);
  });

  it('says the server cannot be reached, shows no roster, and cannot be joined', () => {
    const { voice } = panel(initialVoice, 'c', { kind: 'unreachable' });
    expect(lines(voice)).toEqual([unreachable]);
    expect(roster(voice)).toBeNull();
    expect(offered(voice)).toEqual(['disabled', 'disabled']);
  });

  it('tells a person without voice who to ask, and offers no join', () => {
    const { voice } = panel(initialVoice, 'c', { kind: 'not_allowed' });
    expect(lines(voice)).toEqual([notAllowed]);
    expect(voice.textContent).toMatch(/^Not in voice/);
    expect(roster(voice)).toBeNull();
    expect(offered(voice)).toEqual(['absent', 'absent']);
    expect(within(voice).queryAllByRole('button')).toEqual([]);
  });

  it('says the status could not be read after the server closed the socket, and cannot be joined', () => {
    const { voice } = panel(initialVoice, 'c', { kind: 'gone' });
    expect(lines(voice)).toEqual([unread]);
    expect(roster(voice)).toBeNull();
    expect(offered(voice)).toEqual(['disabled', 'disabled']);
  });

  it('says voice is not set up on a server without LiveKit, and offers no join', () => {
    const { voice } = panel(initialVoice, 'c', { kind: 'presence', presence: { ...presence, configured: false, available: false } });
    expect(voice.textContent).toContain('Voice is not set up on this server');
    expect(within(voice).queryAllByRole('button')).toEqual([]);
  });
});

// Issue #587: the roster never hides people. How many are in voice is its
// heading, which names the list; on a phone a CSS rule on the class set here
// shows three chips, and "+N more" the rest.
describe('the roster', () => {
  const inRoom = (n: number): VoicePresenceV1 => ({
    ...presence,
    rooms: [
      {
        participants: Array.from({ length: n }, (_, i) => ({
          principal_id: 10 + i,
          can_publish: true,
          transmitting: false,
          joined_at: '2026-10-18T00:00:00Z',
        })),
      },
    ],
  });
  const shown = (n: number) => panel(initialVoice, 'c', { kind: 'presence', presence: inRoom(n) }).voice;

  it('is absent, with no count, while nobody is in voice', () => {
    const voice = shown(0);
    expect(within(voice).queryByRole('list')).toBeNull();
    expect(within(voice).queryByRole('heading')).toBeNull();
    expect(voice.textContent).not.toMatch(/\d in voice/);
  });

  for (const n of [1, 8]) {
    it(`says ${String(n)} in voice, as its heading and as the list's name`, () => {
      const voice = shown(n);
      const heading = within(voice).getByRole('heading', { name: `${String(n)} in voice` });
      const list = within(voice).getByRole('list', { name: `${String(n)} in voice` });
      expect(list.getAttribute('aria-labelledby')).toBe(heading.id);
      expect(within(list).getAllByRole('listitem')).toHaveLength(n);
    });
  }

  for (const n of [1, 3]) {
    it(`offers no "more" button with ${String(n)} in voice, and caps nothing`, () => {
      const voice = shown(n);
      expect(within(voice).queryByRole('button', { name: /more$/ })).toBeNull();
      expect(within(voice).getByRole('list').classList.contains('capped')).toBe(false);
    });
  }

  it('with more than three, offers "+N more", which shows the rest and says so', () => {
    const voice = shown(8);
    const list = within(voice).getByRole('list', { name: '8 in voice' });
    const more = within(voice).getByRole('button', { name: '+5 more' });
    expect(more.getAttribute('aria-expanded')).toBe('false');
    expect(more.getAttribute('aria-controls')).toBe(list.id);
    expect(list.classList.contains('capped')).toBe(true);
    // Every name is still in the list: the cap only hides chips on a phone.
    expect(within(list).getAllByRole('listitem')).toHaveLength(8);

    fireEvent.click(more);
    const fewer = within(voice).getByRole('button', { name: 'Show fewer' });
    expect(fewer.getAttribute('aria-expanded')).toBe('true');
    expect(list.classList.contains('capped')).toBe(false);

    fireEvent.click(fewer);
    expect(within(voice).getByRole('button', { name: '+5 more' }).getAttribute('aria-expanded')).toBe('false');
    expect(list.classList.contains('capped')).toBe(true);
  });
});

// Issue #641: a capped roster keeps the viewer's own chip, wherever the
// roster has it, and hides the others after it; the list's order is the
// roster's.
describe('the capped roster keeps the viewer', () => {
  const room = (ids: readonly number[]): VoicePresenceV1 => ({
    ...presence,
    rooms: [{ participants: ids.map((id) => ({ principal_id: id, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' })) }],
  });
  const chips = (ids: readonly number[]) => {
    const { voice } = panel(initialVoice, 'c', { kind: 'presence', presence: room(ids) });
    return within(within(voice).getByRole('list')).getAllByRole('listitem');
  };
  const hidden = (items: readonly HTMLElement[]) => items.map((li) => li.classList.contains('beyond'));

  // The roster is in principal order, and the viewer is 7.
  it('shows the viewer, seventh of eight, with the first two others', () => {
    const items = chips([1, 2, 3, 4, 5, 6, 7, 8]);
    expect(items[6]?.querySelector('.who')?.textContent).toContain('You');
    expect(hidden(items)).toEqual([false, false, true, true, true, true, false, true]);
  });

  it('keeps the first three when the viewer is already among them', () => {
    const items = chips([7, 8, 9, 10, 11, 12, 13, 14]);
    expect(hidden(items)).toEqual([false, false, false, true, true, true, true, true]);
  });

  it('keeps the first three when the viewer is not in voice', () => {
    const items = chips([1, 2, 3, 4, 5]);
    expect(hidden(items)).toEqual([false, false, false, true, true]);
  });

  it('hides nobody with three or fewer', () => {
    expect(hidden(chips([1, 2, 7]))).toEqual([false, false, false]);
  });
});

// Issue #587 (F12): the strip is two rows, the roster above the state and the
// controls, and a longer status line changes only the text in the state
// column: the elements, their order and their classes stay the same, so the
// CSS that keeps the controls in place has nothing else to move.
describe('the strip in voice', () => {
  const joined = (deafened: boolean): VoiceState => ({ ...inVoice('connected', 'c'), muted: deafened, deafened });
  const someone: VoicePresenceV1 = {
    ...presence,
    rooms: [{ participants: [{ principal_id: 7, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' }] }],
  };
  // The strip's elements, in order, each with its classes.
  const shape = (voice: HTMLElement) =>
    Array.from(voice.querySelectorAll('.voice-bar, .voice-bar *')).map((el) => `${el.tagName}.${el.getAttribute('class') ?? ''}`);

  it('is the roster, then a row of the state and the controls, with Leave voice last', () => {
    const { voice } = panel(joined(false), 'c', { kind: 'presence', presence: someone });
    const bar = voice.querySelector('.voice-bar');
    expect(Array.from(bar?.children ?? []).map((el) => el.className)).toEqual(['voice-who', 'voice-row']);
    const row = voice.querySelector('.voice-row');
    expect(Array.from(row?.children ?? []).map((el) => el.className)).toEqual(['voice-state', 'voice-controls']);
    const leave = within(voice).getByRole('button', { name: 'Leave voice' });
    expect(leave.parentElement?.lastElementChild).toBe(leave);
    expect(leave.parentElement?.className).toBe('voice-controls');
  });

  it('keeps the same elements, order and classes when deafened, the longest status line', () => {
    const before = shape(panel(joined(false), 'c', { kind: 'presence', presence: someone }).voice);
    cleanup();
    const { voice } = panel(joined(true), 'c', { kind: 'presence', presence: someone });
    expect(voice.querySelector('.voice-state')?.textContent).toContain('Deafened (you hear no one, and you are muted).');
    expect(shape(voice)).toEqual(before);
  });
});

// Issue #965: the strip says Transmitting only while audio is sent; a press
// that waits for conchd's answer says so, and Hold to talk shows pressed.
// The release delay is set in Voice options.
describe('what is sent, as the strip says it', () => {
  it.each([
    ['nothing held', { talking: false, transmitting: false }, 'Not transmitting', 'false'],
    ['a press waiting for its answer', { talking: true, transmitting: false }, 'Waiting for the server', 'true'],
    ['a press sending', { talking: true, transmitting: true }, 'Transmitting', 'true'],
  ] as const)('%s', (_what, patch, said, pressed) => {
    const { voice } = panel({ ...inVoice('connected', 'c'), ...patch }, 'c');
    const line = voice.querySelector('.voice-tx')?.textContent ?? '';
    expect(line.endsWith(said)).toBe(true);
    if (said !== 'Transmitting') expect(line.endsWith('Transmitting') && !line.endsWith('Not transmitting')).toBe(false);
    expect(within(voice).getByRole('button', { name: 'Hold to talk' }).getAttribute('aria-pressed')).toBe(pressed);
    expect(voice.classList.contains('sending')).toBe(patch.transmitting);
  });

  it('the release delay is under Voice options in push to talk, says its value, and is set by its slider', () => {
    const { voice, calls, redraw } = panel({ ...inVoice('connected', 'c'), releaseDelay: 200 }, 'c');
    fireEvent.click(within(voice).getByRole('button', { name: 'Voice options' }));
    const slider = within(voice).getByRole('slider', { name: /Release delay/ });
    expect(slider.getAttribute('aria-valuetext')).toBe('200 ms');
    fireEvent.change(slider, { target: { value: '450' } });
    expect(calls).toContain('release 450');
    redraw({ ...inVoice('connected', 'c'), releaseDelay: 450, mode: 'open' });
    expect(within(voice).queryByRole('slider', { name: /Release delay/ })).toBeNull();
  });
});

// Issue #590 (F09, F20): Hold to talk works from the keyboard while it has
// focus, the hint says where the talk key does not work, and Leave voice
// hands focus to Join voice instead of dropping it.
describe('the keyboard', () => {
  const talking = () => {
    const { voice, client, redraw, calls } = panel(inVoice('connected', 'c'), 'c');
    const press = vi.spyOn(client, 'press').mockImplementation(() => undefined);
    const release = vi.spyOn(client, 'release').mockImplementation(() => undefined);
    const talk = within(voice).getByRole('button', { name: 'Hold to talk' });
    talk.focus();
    return { voice, talk, press, release, redraw, calls };
  };

  for (const [key, code] of [
    [' ', 'Space'],
    ['Enter', 'Enter'],
  ] as const) {
    it(`holds Hold to talk down with ${code}, and lets go on its keyup`, () => {
      const { talk, press, release } = talking();
      const down = fireEvent.keyDown(talk, { key, code });
      // The key is the button's: its default (a click, a scroll) is not done.
      expect(down).toBe(false);
      expect(press).toHaveBeenCalledTimes(1);
      expect(release).not.toHaveBeenCalled();
      fireEvent.keyUp(talk, { key, code });
      expect(release).toHaveBeenCalledTimes(1);
      expect(press).toHaveBeenCalledTimes(1);
    });
  }

  it('does nothing on a held key repeating', () => {
    const { talk, press, release } = talking();
    fireEvent.keyDown(talk, { key: ' ', code: 'Space' });
    fireEvent.keyDown(talk, { key: ' ', code: 'Space', repeat: true });
    fireEvent.keyDown(talk, { key: 'Enter', code: 'Enter', repeat: true });
    expect(press).toHaveBeenCalledTimes(1);
    expect(release).not.toHaveBeenCalled();
  });

  it('ignores other keys', () => {
    const { talk, press, release } = talking();
    fireEvent.keyDown(talk, { key: 'a', code: 'KeyA' });
    fireEvent.keyUp(talk, { key: 'a', code: 'KeyA' });
    expect(press).not.toHaveBeenCalled();
    expect(release).not.toHaveBeenCalled();
  });

  it('lets go when focus leaves Hold to talk', () => {
    const { voice, talk, press, release } = talking();
    fireEvent.keyDown(talk, { key: ' ', code: 'Space' });
    within(voice).getByRole('button', { name: 'Mute' }).focus();
    expect(press).toHaveBeenCalledTimes(1);
    expect(release).toHaveBeenCalledTimes(1);
  });

  it('says, in the strip and in Voice options, that the talk key does not work while typing', () => {
    const { voice } = talking();
    const said = 'It works only while this tab has focus, and not while you are typing.';
    expect(voice.querySelector('.talk-key-note')?.textContent).toBe(`Talk key: \` (backtick). ${said}`);
    const options = voice.querySelector('#voice-options');
    expect(options?.textContent).toContain('Talk key: ` (backtick).');
    expect(options?.textContent).toContain('The talk key works only while this tab has focus, and not while you are typing.');
  });

  it('gives focus to Join voice after Leave voice, pressed with focus on it, has left', () => {
    const { voice, redraw, calls } = talking();
    const leave = within(voice).getByRole('button', { name: 'Leave voice' });
    leave.focus();
    fireEvent.click(leave);
    expect(calls).toEqual(['leave']);
    // Still leaving: the strip is as it was.
    redraw(inVoice('connected', 'c'));
    expect(document.activeElement).toBe(leave);
    redraw(initialVoice);
    expect(document.activeElement).toBe(within(voice).getByRole('button', { name: 'Join voice in #c' }));
  });

  it('leaves focus alone after Leave voice pressed without focus on it', () => {
    const { voice, talk, redraw } = talking();
    talk.blur();
    fireEvent.click(within(voice).getByRole('button', { name: 'Leave voice' }));
    redraw(initialVoice);
    expect(document.activeElement).toBe(document.body);
  });

  it('does not take focus to Join voice from where it went while leaving', () => {
    const { voice, redraw } = talking();
    const leave = within(voice).getByRole('button', { name: 'Leave voice' });
    leave.focus();
    fireEvent.click(leave);
    const outside = document.createElement('button');
    document.body.append(outside);
    outside.focus();
    redraw(initialVoice);
    expect(document.activeElement).toBe(outside);
    outside.remove();
  });
});

// Issue #591 (F13, F14, F15, F24): the strip's controls and notes say what is
// true. Bob (3) is someone else; the viewer is 7.
describe('what the strip says is true', () => {
  const bob: ReadonlyMap<number, Person> = new Map([[3, { name: 'bob', kind: null }]]);
  const one = (principal: number, sharing = false) => ({ principal_id: principal, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z', sharing });
  const room = (...participants: ReturnType<typeof one>[]): Snapshot => ({ kind: 'presence', presence: { ...presence, rooms: [{ participants }] } });
  const bobShares = room(one(7), one(3, true));
  const nobodyShares = room(one(7), one(3));
  const joined = (patch: Partial<VoiceState> = {}): VoiceState => ({ ...inVoice('connected', 'c'), ...patch });

  describe('Mute, deafened (F13)', () => {
    for (const muted of [false, true]) {
      it(`is pressed, off and named Muted by deafen while deafened, and ${muted ? 'muted' : 'not muted'} again on Undeafen`, () => {
        const name = muted ? 'Unmute' : 'Mute';
        const { voice, redraw } = panel(joined({ muted }), 'c');
        expect(within(voice).getByRole('button', { name, pressed: muted }).hasAttribute('disabled')).toBe(false);

        redraw(joined({ muted, deafened: true }));
        expect(within(voice).queryByRole('button', { name })).toBeNull();
        expect(within(voice).getByRole('button', { name: 'Muted by deafen', pressed: true }).hasAttribute('disabled')).toBe(true);

        redraw(joined({ muted }));
        expect(within(voice).getByRole('button', { name, pressed: muted }).hasAttribute('disabled')).toBe(false);
      });
    }
  });

  describe('Share screen while someone else shares (F14)', () => {
    const said = 'bob is sharing a screen. One person shares at a time.';
    // Not watching, the visible line says where to see it (issue #642, N02);
    // the rule is read out beside it, as Share screen's description.
    const watch = 'bob is sharing a screen. Watch to see it.';
    const described = `${watch} One person shares at a time.`;

    it('is off, and described as who shares and the rule; on again with no description once nobody else does', () => {
      const { voice, redraw } = panel(joined(), 'c', bobShares, bob);
      const button = within(voice).getByRole('button', { name: 'Share screen', description: described });
      expect(button.hasAttribute('disabled')).toBe(true);
      const ids = (button.getAttribute('aria-describedby') ?? '').split(' ');
      expect(ids.map((id) => document.getElementById(id)?.textContent)).toEqual([watch, 'One person shares at a time.']);
      expect(ids.every((id) => voice.contains(document.getElementById(id)))).toBe(true);
      // The one visible line about bob's share is the first.
      expect(within(voice).getAllByText(/bob is sharing/).map((el) => el.id)).toEqual(['voice-share-busy']);
      expect(within(voice).getByText(watch).className).not.toContain('watched');

      redraw(joined(), nobodyShares);
      const again = within(voice).getByRole('button', { name: 'Share screen' });
      expect(again.hasAttribute('disabled')).toBe(false);
      expect(again.hasAttribute('aria-describedby')).toBe(false);
      expect(voice.textContent).not.toContain('Watch to see it.');
    });

    it('while watched keeps the line hidden, without the watch prompt', () => {
      const { voice } = panel(joined({ watching: true }), 'c', bobShares, bob);
      const line = within(voice).getByText(said);
      expect(line.id).toBe('voice-share-busy');
      expect(line.className).toContain('watched');
      expect(voice.textContent).not.toContain('Watch to see it.');
      expect(within(voice).getByRole('button', { name: 'Share screen', description: said })).toBeTruthy();
    });

    it('says a refused claim once while the other share lasts, and not after it ends', () => {
      const refused = joined({ share: { kind: 'idle', ended: 'busy' } });
      const { voice, redraw } = panel(refused, 'c', bobShares, bob);
      // The refusal is the description's line, read out: one line, not two.
      const notes = within(voice).getAllByText(said);
      expect(notes).toHaveLength(1);
      expect(notes[0]?.getAttribute('role')).toBe('status');
      expect(within(voice).getByRole('button', { name: 'Share screen', description: said })).toBeTruthy();

      redraw(refused, nobodyShares);
      expect(voice.textContent).not.toContain('One person shares at a time.');
      expect(within(voice).getByRole('button', { name: 'Share screen' }).hasAttribute('disabled')).toBe(false);
    });

    it('still says why a share of ours ended, beside who shares now, unless we stopped it', () => {
      const { voice } = panel(joined({ share: { kind: 'idle', ended: 'operator' } }), 'c', bobShares, bob);
      expect(voice.textContent).toContain('An operator stopped your share.');
      expect(voice.textContent).toContain(watch);
    });
  });

  describe('"You stopped sharing." (issue #642, N03)', () => {
    afterEach(() => {
      vi.useRealTimers();
    });
    const said = 'You stopped sharing.';
    const stoppedState = joined({ share: { kind: 'idle', ended: 'self' } });

    it(`is gone after ${String(stoppedFor / 1000)} s, within 8 s, and does not come back`, () => {
      vi.useFakeTimers();
      const { voice, redraw } = panel(stoppedState, 'c', nobodyShares);
      expect(within(voice).getByText(said).getAttribute('role')).toBe('status');
      act(() => {
        vi.advanceTimersByTime(stoppedFor - 1);
      });
      expect(within(voice).queryByText(said)).not.toBeNull();
      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(voice.textContent).not.toContain(said);
      expect(stoppedFor).toBeLessThanOrEqual(8000);
      redraw(stoppedState, nobodyShares);
      expect(voice.textContent).not.toContain(said);
    });

    it("is gone at once when another's share starts, and not back when it ends", () => {
      vi.useFakeTimers();
      const { voice, redraw } = panel(stoppedState, 'c', nobodyShares);
      expect(within(voice).queryByText(said)).not.toBeNull();
      redraw(stoppedState, bobShares);
      expect(voice.textContent).not.toContain(said);
      redraw(stoppedState, nobodyShares);
      expect(voice.textContent).not.toContain(said);
    });

    it('is gone at once when ours starts, and a later stop says it afresh', () => {
      vi.useFakeTimers();
      const { voice, redraw } = panel(stoppedState, 'c', nobodyShares);
      redraw(joined({ share: { kind: 'sharing', audio: false } }), nobodyShares);
      expect(voice.textContent).not.toContain(said);
      redraw(stoppedState, nobodyShares);
      expect(within(voice).queryByText(said)).not.toBeNull();
    });

    it('is not shown at all when it arrives beside another share', () => {
      const { voice } = panel(stoppedState, 'c', bobShares, bob);
      expect(voice.textContent).not.toContain(said);
    });

    it('leaves the other ended notes as they were, whatever the time', () => {
      vi.useFakeTimers();
      const { voice } = panel(joined({ share: { kind: 'idle', ended: 'browser' } }), 'c', nobodyShares);
      act(() => {
        vi.advanceTimersByTime(60_000);
      });
      expect(within(voice).queryByText('Sharing stopped from the browser.')).not.toBeNull();
    });

    it('clears its timer on unmount', () => {
      vi.useFakeTimers();
      panel(stoppedState, 'c', nobodyShares);
      expect(vi.getTimerCount()).toBeGreaterThan(0);
      cleanup();
      expect(vi.getTimerCount()).toBe(0);
    });
  });

  describe('a watched share that ends (F15)', () => {
    afterEach(() => {
      vi.useRealTimers();
    });
    const said = 'bob stopped sharing.';
    const shown = (voice: HTMLElement) => within(voice).queryByText(said);

    it(`says the holder stopped, read out, for ${String(stoppedFor / 1000)} s`, () => {
      vi.useFakeTimers();
      const { voice, redraw } = panel(joined({ watching: true }), 'c', bobShares, bob);
      expect(shown(voice)).toBeNull();
      // Bob's share ends: presence says nobody shares, then the client stops
      // watching.
      redraw(joined({ watching: true }), nobodyShares);
      expect(shown(voice)).toBeNull();
      redraw(joined({ watching: false }), nobodyShares);
      expect(shown(voice)?.getAttribute('role')).toBe('status');
      act(() => {
        vi.advanceTimersByTime(stoppedFor - 1);
      });
      expect(shown(voice)).not.toBeNull();
      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(shown(voice)).toBeNull();
    });

    it('says the holder stopped when the share and the watching end in one frame', () => {
      const { voice, redraw } = panel(joined({ watching: true }), 'c', bobShares, bob);
      redraw(joined({ watching: false }), nobodyShares);
      expect(shown(voice)).not.toBeNull();
    });

    it('says nothing when the viewer stops watching, nor when that share ends later', () => {
      vi.useFakeTimers();
      const { voice, redraw } = panel(joined({ watching: true }), 'c', bobShares, bob);
      redraw(joined({ watching: false }), bobShares);
      expect(shown(voice)).toBeNull();
      redraw(joined({ watching: false }), nobodyShares);
      act(() => {
        vi.advanceTimersByTime(stoppedFor);
      });
      expect(voice.textContent).not.toContain('stopped sharing');
    });

    it('says nothing while a share that went with a renewed room is waited for, nor when it is back', () => {
      // The client keeps watching through the wait (connection.ts
      // awaitShare): the room is renewed, and everyone left it.
      const { voice, redraw } = panel(joined({ watching: true }), 'c', bobShares, bob);
      redraw(joined({ phase: 'connecting', watching: true }), room());
      redraw(joined({ watching: true }), room(one(7)));
      expect(shown(voice)).toBeNull();
      redraw(joined({ watching: true }), bobShares);
      expect(voice.textContent).not.toContain('stopped sharing');
    });

    it('says the holder stopped when the wait for a renewed share runs out', () => {
      const { voice, redraw } = panel(joined({ watching: true }), 'c', bobShares, bob);
      redraw(joined({ watching: true }), room(one(7)));
      redraw(joined({ watching: false }), room(one(7)));
      expect(shown(voice)).not.toBeNull();
    });

    it('says nothing on leaving voice while watching', () => {
      const { voice, redraw } = panel(joined({ watching: true }), 'c', bobShares, bob);
      redraw(initialVoice, nobodyShares);
      expect(voice.textContent).not.toContain('stopped sharing');
    });
  });

  describe('the viewer in voice on another device (F24)', () => {
    const elsewhere = 'You are in voice on another device. Joining here moves your voice to this device.';
    const names = (voice: HTMLElement) => Array.from(voice.querySelectorAll('.voice-roster .who')).map((el) => el.textContent);

    it('names the viewer "You (on another device)", and says a join here moves voice, while this page is not in voice', () => {
      const { voice } = panel(initialVoice, 'c', room(one(3), one(7)), bob);
      expect(names(voice)).toEqual(['bob', 'You (on another device)']);
      expect(voice.textContent).toContain(elsewhere);
      // Nothing stops the join.
      expect(within(voice).getByRole('button', { name: 'Join voice in #c' }).hasAttribute('disabled')).toBe(false);
    });

    for (const phase of ['connecting', 'connected', 'reconnecting', 'unavailable'] as const) {
      it(`names the viewer "You" while this page is in voice (${phase})`, () => {
        const { voice } = panel(inVoice(phase, 'c'), 'c', room(one(3), one(7)), bob);
        expect(names(voice)).toEqual(['bob', 'You']);
        expect(voice.textContent).not.toContain('another device');
      });
    }

    it('says so on a page another device took over', () => {
      const why = 'Another device took over your place in this voice room';
      const { voice } = panel({ ...initialVoice, phase: 'stopped', why }, 'c', room(one(7)));
      expect(names(voice)).toEqual(['You (on another device)']);
      expect(voice.textContent).toContain(elsewhere);
    });

    it("does not say so of this page's own place while presence catches up with a Leave voice", () => {
      const { voice, redraw } = panel(inVoice('connected', 'c'), 'c', room(one(7)));
      redraw(initialVoice);
      expect(names(voice)).toEqual(['You']);
      expect(voice.textContent).not.toContain('another device');
      redraw(initialVoice, room());
      expect(names(voice)).toEqual([]);
      // In voice on another device later: said again.
      redraw(initialVoice, room(one(7)));
      expect(names(voice)).toEqual(['You (on another device)']);
      expect(voice.textContent).toContain(elsewhere);
    });
  });
});

// Issue #600 (F08, F12): before a share the notes are one line, and More
// about sharing opens Voice options at the full notes. While someone else
// shares, one line says who, with what sound, and why Share screen is off.
describe('the notes about sharing', () => {
  const bob: ReadonlyMap<number, Person> = new Map([[3, { name: 'bob', kind: null }]]);
  const one = (principal: number, sharing = false) => ({
    principal_id: principal,
    can_publish: true,
    transmitting: false,
    joined_at: '2026-10-18T00:00:00Z',
    ...(sharing ? { sharing: true, sharing_audio: true } : {}),
  });
  const room = (...participants: ReturnType<typeof one>[]): Snapshot => ({ kind: 'presence', presence: { ...presence, rooms: [{ participants }] } });
  const nobodyShares = room(one(7), one(3), one(4));
  const bobShares = room(one(7), one(3, true), one(4));
  const joined = (patch: Partial<VoiceState> = {}): VoiceState => ({ ...inVoice('connected', 'c'), ...patch });
  // The full notes for Chrome on Linux, the fake control's browser.
  const full = [soundNote(chrome), ownPageNote];
  const line = 'Sharing: only a tab carries sound; a whole screen also shows this page.';

  it('are one line before a share, with More about sharing; the full notes wait in Voice options', () => {
    const { voice } = panel(joined(), 'c', nobodyShares, bob);
    const short = within(voice).getByText(line);
    expect(short.classList.contains('hint')).toBe(true);
    expect(within(short).getByRole('button', { name: 'More about sharing' })).toBeTruthy();
    // Nothing else of sharing is in the strip's notes: the full text is only
    // under Voice options, still closed.
    const notes = voice.querySelector('.voice-notes');
    for (const text of full) expect(notes?.textContent).not.toContain(text);
    const options = voice.querySelector('#voice-options');
    expect(options?.hasAttribute('hidden')).toBe(true);
    for (const text of full) expect(options?.textContent).toContain(text);
  });

  it('More about sharing opens Voice options at the full notes, in one press', () => {
    const { voice } = panel(joined(), 'c', nobodyShares, bob);
    fireEvent.click(within(voice).getByRole('button', { name: 'More about sharing' }));
    expect(within(voice).getByRole('button', { name: 'Voice options', expanded: true })).toBeTruthy();
    expect(voice.querySelector('#voice-options')?.hasAttribute('hidden')).toBe(false);
    const about = within(voice).getByRole('region', { name: 'Sharing your screen' });
    expect(document.activeElement).toBe(about);
    for (const text of full) expect(within(about).getByText(text)).toBeTruthy();
  });

  it("are absent while someone else shares, and one line says who, with sound, as Share screen's description", () => {
    const said = 'bob is sharing a screen, with sound. One person shares at a time.';
    const watch = 'bob is sharing a screen, with sound. Watch to see it.';
    const { voice, redraw } = panel(joined(), 'c', bobShares, bob);
    expect(within(voice).queryByText(line)).toBeNull();
    expect(within(voice).queryByRole('button', { name: 'More about sharing' })).toBeNull();
    expect(voice.querySelector('#voice-share-about')).toBeNull();
    for (const text of full) expect(voice.textContent).not.toContain(text);
    // One line about bob's share, not two.
    expect(within(voice).getAllByText(/bob is sharing/).map((el) => el.textContent)).toEqual([watch]);
    expect(within(voice).getByRole('button', { name: 'Share screen', description: `${watch} One person shares at a time.` })).toBeTruthy();

    // Watching it, the same line, and still one in the notes (the watch
    // view has its own title).
    redraw(joined({ watching: true }), bobShares);
    const notes = voice.querySelector<HTMLElement>('.voice-notes');
    if (notes === null) throw new Error('no notes');
    expect(within(notes).getAllByText(/bob is sharing/).map((el) => el.textContent)).toEqual([said]);

    // Once bob stops, the one line about sharing is back.
    redraw(joined(), nobodyShares);
    expect(within(voice).getByText(line)).toBeTruthy();
  });

  it('name who shares in a line of its own for someone listening only, who cannot share', () => {
    const { voice } = panel(joined({ listenOnly: true }), 'c', bobShares, bob);
    expect(within(voice).getAllByText(/bob is sharing/).map((el) => el.textContent)).toEqual(['bob is sharing a screen, with sound.']);
    expect(within(voice).queryByRole('button', { name: 'More about sharing' })).toBeNull();
  });
});

// Issue #596: while not in voice, the strip says where else people are in
// voice, and its button opens that channel and nothing more. The channel on
// screen is the roster's, not this line's.
describe('where else people are in voice', () => {
  const at = '2026-10-18T00:00:00Z';
  const channels: ChannelV0[] = [
    { id: 1, name: 'general', created_at: at },
    { id: 2, name: 'game-night', created_at: at },
    { id: 3, name: 'book-club', created_at: at },
    { id: 4, name: 'quiet', created_at: at },
  ];

  function strip(state: VoiceState, viewed: string | null, occupancy: [number, number][]) {
    const calls: string[] = [];
    const opened: string[] = [];
    render(
      <VoicePanel
        voice={control(state, calls, { kind: 'presence', presence })}
        viewer={7}
        people={new Map()}
        viewed={viewed}
        channels={channels}
        occupancy={new Map(occupancy)}
        onOpen={(name) => {
          opened.push(name);
        }}
      />,
    );
    const voice = screen.getByRole('region', { name: 'Voice' });
    return { voice, calls, opened, line: () => voice.querySelector('.voice-elsewhere') };
  }

  it('names the channel and how many, and its button opens that channel without joining', () => {
    const { voice, calls, opened, line } = strip(initialVoice, 'general', [[2, 3]]);
    expect(line()?.textContent).toBe('3 in voice in #game-night Open #game-night');
    fireEvent.click(within(voice).getByRole('button', { name: 'Open #game-night' }));
    expect(opened).toEqual(['game-night']);
    expect(calls).toEqual([]);
    // Join voice still goes where it went: the channel on screen.
    expect(within(voice).getByRole('button', { name: 'Join voice in #general' })).toBeTruthy();
  });

  it('names the busiest, ties to the lower id, and says how many more channels', () => {
    expect(strip(initialVoice, 'general', [[3, 2], [2, 2], [4, 1]]).line()?.textContent).toBe('2 in voice in #game-night and 2 more channels Open #game-night');
    cleanup();
    expect(strip(initialVoice, 'general', [[3, 1], [4, 5]]).line()?.textContent).toBe('5 in voice in #quiet and 1 more channel Open #quiet');
  });

  it('leaves out the channel on screen: its people are the roster', () => {
    expect(strip(initialVoice, 'game-night', [[2, 3]]).line()).toBeNull();
    cleanup();
    expect(strip(initialVoice, 'game-night', [[2, 3], [3, 1]]).line()?.textContent).toBe('1 in voice in #book-club Open #book-club');
  });

  it('says nothing while nobody is in voice anywhere', () => {
    const { voice, line } = strip(initialVoice, 'general', []);
    expect(line()).toBeNull();
    expect(within(voice).queryByRole('button', { name: /^Open/ })).toBeNull();
  });

  it("is not shown while in voice: the strip then describes the viewer's own connection", () => {
    expect(strip(inVoice('connected', 'general'), 'general', [[2, 3]]).line()).toBeNull();
    cleanup();
    expect(strip(inVoice('connected', 'general'), 'book-club', [[2, 3]]).line()).toBeNull();
  });

  it('is shown while no channel is on screen', () => {
    const { voice, opened, line } = strip(initialVoice, null, [[3, 2]]);
    expect(line()?.textContent).toBe('2 in voice in #book-club Open #book-club');
    fireEvent.click(within(voice).getByRole('button', { name: 'Open #book-club' }));
    expect(opened).toEqual(['book-club']);
  });
});

// Issue #658: the choice of microphone in Voice options, the device in use,
// and the note while the microphone is missing.
describe('the microphone chosen', () => {
  const list = [
    { id: 'default', label: 'Default - Headset' },
    { id: 'mic-1', label: 'Headset' },
    { id: 'mic-2', label: 'Webcam' },
  ];
  const headset = { id: 'mic-1', label: 'Headset' };
  const using = (patch: Partial<VoiceState> = {}): VoiceState => ({ ...inVoice('connected', 'c'), microphones: list, inUse: 'default', ...patch });
  const options = (voice: HTMLElement) => {
    fireEvent.click(within(voice).getByRole('button', { name: 'Voice options' }));
    return voice.querySelector<HTMLElement>('#voice-options') ?? voice;
  };
  const choice = (opts: HTMLElement) => within(opts).getByRole<HTMLSelectElement>('combobox', { name: 'Microphone' });
  const names = (select: HTMLSelectElement) => Array.from(select.options).map((o) => o.textContent);

  it('lists System default first, then every microphone by name, and says which device is in use', () => {
    const { voice } = panel(using(), 'c');
    const opts = options(voice);
    const select = choice(opts);
    expect(names(select)).toEqual(['System default', 'Default - Headset', 'Headset', 'Webcam']);
    expect(select.selectedOptions[0]?.textContent).toBe('System default');
    expect(opts.textContent).toContain('In use: Default - Headset');
    expect(opts.textContent).toContain(followsNote);
  });

  it('shows a device chosen as chosen, and the device the capture is on', () => {
    const { voice } = panel(using({ microphone: headset, inUse: 'mic-1' }), 'c');
    const opts = options(voice);
    expect(choice(opts).selectedOptions[0]?.textContent).toBe('Headset');
    expect(opts.textContent).toContain('In use: Headset');
    expect(opts.textContent).not.toContain(followsNote);
  });

  it('choosing calls the client: a device listed, and System default as none', () => {
    const { voice, client } = panel(using({ microphone: headset, inUse: 'mic-1' }), 'c');
    const chosen = vi.spyOn(client, 'chooseMicrophone').mockResolvedValue();
    const select = choice(options(voice));
    fireEvent.change(select, { target: { value: select.options[3]?.value } });
    fireEvent.change(select, { target: { value: select.options[0]?.value } });
    expect(chosen.mock.calls).toEqual([[{ id: 'mic-2', label: 'Webcam' }], [null]]);
  });

  it.each<[string, Partial<VoiceState>]>([
    ['in watch only', { watchOnly: true, microphones: null, inUse: null }],
    ['when the browser does not name its microphones', { microphones: null }],
  ])('lists nothing %s, and says when it will', (_what, patch) => {
    const { voice } = panel(using(patch), 'c');
    const opts = options(voice);
    expect(within(opts).queryByRole('combobox', { name: 'Microphone' })).toBeNull();
    expect(opts.textContent).toContain('Microphones are listed once you use your microphone.');
  });

  it('says nothing of microphones in listen only', () => {
    const { voice } = panel(using({ listenOnly: true, microphones: null, inUse: null }), 'c');
    const opts = options(voice);
    expect(within(opts).queryByRole('combobox', { name: 'Microphone' })).toBeNull();
    expect(opts.textContent).not.toContain('Microphones are listed');
  });

  it('while missing: the note, no Microphone in use, nothing in use, and the list asks for a choice', () => {
    const { voice, redraw } = panel(using({ microphone: headset, inUse: 'mic-1' }), 'c');
    expect(within(voice).getByText('Microphone in use')).toBeTruthy();
    expect(within(voice).queryByText(missingNote)).toBeNull();
    redraw(using({ microphone: headset, inUse: null, missing: true }));
    expect(within(voice).getByText(missingNote).getAttribute('role')).toBe('status');
    expect(within(voice).queryByText('Microphone in use')).toBeNull();
    const opts = options(voice);
    const select = choice(opts);
    expect(select.selectedOptions[0]?.textContent).toBe('Choose a microphone');
    // The same device, or System default, is a change from there.
    expect(names(select)).toEqual(['Choose a microphone', 'System default', 'Default - Headset', 'Headset', 'Webcam']);
    expect(opts.textContent).not.toContain('In use:');
  });

  it('a switch that failed says why instead of the note', () => {
    const why = 'The microphone is in use by another program, or could not be opened. Close it and join again, or use Watch only.';
    const { voice } = panel(using({ inUse: null, missing: true, why }), 'c');
    expect(within(voice).getByText(why)).toBeTruthy();
    expect(within(voice).queryByText(missingNote)).toBeNull();
  });
});

// Issue #659: the voice processing switch in Voice options' Microphone
// section, what the capture says of it, the note while it is off, and the
// open microphone's hint.
describe('voice processing', () => {
  const on = { echoCancellation: true, noiseSuppression: true, autoGainControl: true };
  const off = { echoCancellation: false, noiseSuppression: false, autoGainControl: false };
  const using = (patch: Partial<VoiceState> = {}): VoiceState => ({ ...inVoice('connected', 'c'), microphones: [], inUse: 'default', reported: on, ...patch });
  const options = (voice: HTMLElement) => {
    fireEvent.click(within(voice).getByRole('button', { name: 'Voice options' }));
    return voice.querySelector<HTMLElement>('#voice-options') ?? voice;
  };
  const box = (opts: HTMLElement) => within(opts).getByRole<HTMLInputElement>('checkbox', { name: processingLabel });

  it('is one checkbox in the Microphone section, on for a device that has it on, and says nothing more', () => {
    const { voice } = panel(using(), 'c');
    const opts = options(voice);
    const section = within(opts).getByRole('group', { name: 'Microphone' });
    expect(within(section).getAllByRole('checkbox')).toEqual([box(opts)]);
    expect(box(opts).checked).toBe(true);
    expect(box(opts).matches(':disabled')).toBe(false);
    expect(opts.textContent).not.toContain(processingOffNote);
    expect(opts.textContent).not.toContain(processingUseNote);
    expect(opts.textContent).not.toContain('Your browser kept');
  });

  it("follows the chosen device's setting", () => {
    const { voice, redraw } = panel(using({ microphone: { id: 'mic-1', label: 'Headset' }, processing: false, reported: off }), 'c');
    const opts = options(voice);
    expect(box(opts).checked).toBe(false);
    redraw(using({ microphone: { id: 'mic-2', label: 'Webcam' }, processing: true, reported: on }));
    expect(box(opts).checked).toBe(true);
  });

  it('turning it off or on calls the client', () => {
    const { voice, client, redraw } = panel(using(), 'c');
    const set = vi.spyOn(client, 'setProcessing').mockResolvedValue();
    const opts = options(voice);
    fireEvent.click(box(opts));
    redraw(using({ processing: false, reported: off }));
    fireEvent.click(box(opts));
    expect(set.mock.calls).toEqual([[false], [true]]);
  });

  it('off: says the microphone is sent as it is, and when to use it', () => {
    const { voice } = panel(using({ processing: false, reported: off }), 'c');
    const opts = options(voice);
    expect(opts.textContent).toContain(processingOffNote);
    expect(opts.textContent).toContain(processingUseNote);
  });

  it.each<[string, Processing, string]>([
    ['one', { ...off, noiseSuppression: true }, 'Your browser kept noise suppression on.'],
    ['two', { ...off, echoCancellation: true, autoGainControl: true }, 'Your browser kept echo cancellation and automatic gain on.'],
    ['all three', on, 'Your browser kept echo cancellation, noise suppression and automatic gain on.'],
  ])('off, with %s kept on by the browser: says which, and not that it is off', (_what, reported, said) => {
    const { voice } = panel(using({ processing: false, reported }), 'c');
    const opts = options(voice);
    expect(opts.textContent).toContain(said);
    expect(opts.textContent).not.toContain(processingOffNote);
    expect(opts.textContent).toContain(processingUseNote);
  });

  it('says nothing of the capture while there is none, or while the browser does not say', () => {
    expect(reportedNote(null)).toBeNull();
    expect(reportedNote({ echoCancellation: false, noiseSuppression: null, autoGainControl: false })).toBeNull();
    const { voice } = panel(using({ processing: false, reported: null, inUse: null }), 'c');
    const opts = options(voice);
    expect(opts.textContent).not.toContain(processingOffNote);
    expect(opts.textContent).toContain(processingUseNote);
  });

  it.each<[string, Partial<VoiceState>]>([
    ['watch only', { watchOnly: true, microphones: null, inUse: null, reported: null }],
    ['listen only', { listenOnly: true, microphones: null, inUse: null, reported: null }],
  ])('is shown but disabled in %s, as the section is', (_what, patch) => {
    const { voice } = panel(using(patch), 'c');
    expect(box(options(voice)).matches(':disabled')).toBe(true);
  });

  it('in open microphone, hints by the level only while processing is off', () => {
    const { voice, redraw } = panel(using({ mode: 'open' }), 'c');
    const opts = options(voice);
    const level = within(opts).getByRole('slider', { name: 'Level that opens the microphone' });
    expect(opts.textContent).not.toContain(thresholdHint);
    expect(level.getAttribute('aria-describedby')).toBeNull();
    redraw(using({ mode: 'open', processing: false, reported: off }));
    expect(opts.textContent).toContain(thresholdHint);
    expect(document.getElementById(level.getAttribute('aria-describedby') ?? '')?.textContent).toBe(thresholdHint);
    // Push to talk has no level, and so no hint.
    redraw(using({ mode: 'ptt', processing: false, reported: off }));
    expect(opts.textContent).not.toContain(thresholdHint);
  });
});

// Issue #660: the speakers in Voice options, where the browser lets a page
// choose them: System default first, then each output listed, the one in use
// selected; a line where it cannot choose, and where it lists none; and
// what the notes say of a refusal and of speakers that went.
describe('the speakers chosen', () => {
  const outputs = [
    { id: 'default', label: 'Default - Headset' },
    { id: 'out-1', label: 'Desk speakers' },
  ];
  const desk = { id: 'out-1', label: 'Desk speakers' };
  const playing = (patch: Partial<VoiceState> = {}): VoiceState => ({ ...inVoice('connected', 'c'), outputSelection: true, outputs, ...patch });
  const options = (voice: HTMLElement) => {
    fireEvent.click(within(voice).getByRole('button', { name: 'Voice options' }));
    return voice.querySelector<HTMLElement>('#voice-options') ?? voice;
  };
  const choice = (opts: HTMLElement) => within(opts).getByRole<HTMLSelectElement>('combobox', { name: 'Speakers' });
  const names = (select: HTMLSelectElement) => Array.from(select.options).map((o) => o.textContent);
  const said = (voice: HTMLElement, text: string) => within(voice).queryAllByRole('status').some((el) => el.textContent === text);

  it('lists System default first, then every output by name, with the one in use selected', () => {
    const { voice, redraw } = panel(playing(), 'c');
    const select = choice(options(voice));
    expect(names(select)).toEqual(['System default', 'Default - Headset', 'Desk speakers']);
    expect(select.selectedOptions[0]?.textContent).toBe('System default');
    redraw(playing({ output: desk }));
    expect(choice(voice).selectedOptions[0]?.textContent).toBe('Desk speakers');
    expect(voice.textContent).not.toContain(noOtherOutputs);
  });

  it('choosing calls the client: an output listed, and System default as none', () => {
    const { voice, client } = panel(playing({ output: desk }), 'c');
    const chosen = vi.spyOn(client, 'chooseOutput').mockResolvedValue();
    const select = choice(options(voice));
    fireEvent.change(select, { target: { value: select.options[1]?.value } });
    fireEvent.change(select, { target: { value: select.options[0]?.value } });
    expect(chosen.mock.calls).toEqual([[{ id: 'default', label: 'Default - Headset' }], [null]]);
  });

  it.each<[string, Partial<VoiceState>]>([
    ['in voice', {}],
    ['in watch only', { watchOnly: true }],
    ['in listen only', { listenOnly: true }],
  ])('where the browser cannot choose, says so %s and shows no control', (_what, patch) => {
    const { voice } = panel(playing({ outputSelection: false, outputs: null, ...patch }), 'c');
    const opts = options(voice);
    expect(within(opts).queryByRole('combobox', { name: 'Speakers' })).toBeNull();
    expect(opts.textContent).toContain('This browser plays sound on its default output.');
    expect(noOutputChoice).toBe('This browser plays sound on its default output.');
  });

  it.each<[string, Partial<VoiceState>]>([
    ['lists no output', { outputs: [] }],
    ['has not been read yet', { outputs: null }],
  ])('offers only System default where the browser %s, and says so', (_what, patch) => {
    const { voice } = panel(playing(patch), 'c');
    const opts = options(voice);
    expect(names(choice(opts))).toEqual(['System default']);
    expect(opts.textContent).toContain('This browser lists no other speakers yet.');
  });

  it('says a refusal in the notes, with the output in use selected', () => {
    const refused = 'Could not play sound on Headset; it stays on Desk speakers.';
    const { voice } = panel(playing({ output: desk, outputNote: refused }), 'c');
    expect(said(voice, refused)).toBe(true);
    expect(choice(options(voice)).selectedOptions[0]?.textContent).toBe('Desk speakers');
  });

  it('says that the speakers were disconnected, with the output now in use selected', () => {
    const moved = 'Your speakers were disconnected; sound now plays on Desk speakers.';
    const { voice, redraw } = panel(playing({ output: { id: 'out-2', label: 'Headset' }, outputs: [...outputs, { id: 'out-2', label: 'Headset' }] }), 'c');
    expect(said(voice, moved)).toBe(false);
    redraw(playing({ output: desk, outputNote: moved }));
    expect(said(voice, moved)).toBe(true);
    expect(choice(options(voice)).selectedOptions[0]?.textContent).toBe('Desk speakers');
  });
});

// Issue #661: a volume for each other person, on this page only. A slider
// each under Voice options, labelled by name, 0 to 100% in steps of 5%, and
// none for the viewer; on a chip, the volume while it is not 100%.
describe('the volume of each person', () => {
  const people: ReadonlyMap<number, Person> = new Map([
    [3, { name: 'bob', kind: null }],
    [5, { name: 'carol', kind: null }],
  ]);
  const one = (principal: number) => ({ principal_id: principal, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' });
  const room: Snapshot = { kind: 'presence', presence: { ...presence, rooms: [{ participants: [one(7), one(3), one(5)] }] } };
  const joined = (patch: Partial<VoiceState> = {}): VoiceState => ({ ...inVoice('connected', 'c'), ...patch });
  const options = (voice: HTMLElement) => {
    fireEvent.click(within(voice).getByRole('button', { name: 'Voice options' }));
    return voice.querySelector<HTMLElement>('#voice-options') ?? voice;
  };
  const chip = (voice: HTMLElement, name: string) => within(voice).getByText(name, { selector: '.who' }).closest('li');

  it.each<[string, Partial<VoiceState>]>([
    ['in voice', {}],
    ['in listen only', { listenOnly: true }],
    ['in watch only', { watchOnly: true }],
  ])('lists a slider for each other person by name, and none for You, %s', (_what, patch) => {
    const { voice } = panel(joined(patch), 'c', room, people);
    const opts = options(voice);
    const group = within(opts).getByRole('group', { name: 'Volume of each person' });
    // Each slider's label, in the roster's order.
    expect(within(group).getAllByRole('slider').map((s) => group.querySelector(`label[for="${s.id}"]`)?.textContent)).toEqual(['Volume of bob', 'Volume of carol']);
    expect(within(opts).queryByRole('slider', { name: /You/ })).toBeNull();
    const bob = within(opts).getByRole<HTMLInputElement>('slider', { name: 'Volume of bob' });
    expect([bob.min, bob.max, bob.step, bob.value]).toEqual(['0', '100', '5', '100']);
    expect(bob.getAttribute('aria-valuetext')).toBe('100%');
  });

  it('shows each value as a percentage', () => {
    const { voice } = panel(joined({ volumes: new Map([[3, 0.4]]) }), 'c', room, people);
    const bob = within(options(voice)).getByRole<HTMLInputElement>('slider', { name: 'Volume of bob' });
    expect(bob.value).toBe('40');
    expect(bob.getAttribute('aria-valuetext')).toBe('40%');
    expect(bob.closest('.voice-volume')?.textContent).toBe('Volume of bob40%');
  });

  it('moving a slider calls the client for that person alone', () => {
    const { voice, client } = panel(joined(), 'c', room, people);
    const set = vi.spyOn(client, 'setVolume').mockReturnValue();
    const opts = options(voice);
    fireEvent.change(within(opts).getByRole('slider', { name: 'Volume of carol' }), { target: { value: '0' } });
    fireEvent.change(within(opts).getByRole('slider', { name: 'Volume of bob' }), { target: { value: '45' } });
    expect(set.mock.calls).toEqual([[5, 0], [3, 0.45]]);
  });

  it('lists nobody while the viewer is alone', () => {
    const alone: Snapshot = { kind: 'presence', presence: { ...presence, rooms: [{ participants: [one(7)] }] } };
    const { voice } = panel(joined(), 'c', alone, people);
    expect(within(options(voice)).queryByRole('group', { name: 'Volume of each person' })).toBeNull();
  });

  it('tags a chip with the volume while it is not 100%, read as " (volume 40%)"', () => {
    const { voice, redraw } = panel(joined({ volumes: new Map([[3, 0.4], [5, 0]]) }), 'c', room, people);
    expect(chip(voice, 'bob')?.textContent).toBe('bob (volume 40%)');
    expect(chip(voice, 'carol')?.textContent).toBe('carol (volume 0%)');
    expect(within(chip(voice, 'bob') ?? voice).getByText('volume 40%', { selector: '.tag' })).toBeTruthy();
    expect(chip(voice, 'You')?.querySelector('.tag')).toBeNull();
    redraw(joined());
    expect(chip(voice, 'bob')?.querySelector('.tag')).toBeNull();
  });
});

// Issue #644: joined while the server says voice is down, the strip says the
// call may drop and lists nobody; when it is back the note goes and the
// roster returns. The connection itself is not touched.
describe('the strip while joined and voice is unreachable', () => {
  const one = (principal: number) => ({ principal_id: principal, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' });
  const rooms = [{ participants: [one(7), one(3)] }];
  const up: Snapshot = { kind: 'presence', presence: { ...presence, rooms } };
  const down: Snapshot = { kind: 'presence', presence: { ...presence, available: false, rooms } };
  const people: ReadonlyMap<number, Person> = new Map([[3, { name: 'bob', kind: null }]]);
  const note = 'Voice server unreachable. You may be cut off.';
  const roster = (voice: HTMLElement) => within(voice).queryByRole('list', { name: / in voice$/ });

  it('shows the note and no roster, then the roster and no note when it is back', () => {
    const state = inVoice('connected', 'c');
    const { voice, redraw, calls } = panel(state, 'c', up, people);
    expect(roster(voice)).not.toBeNull();
    expect(voice.textContent).not.toContain(note);
    redraw(state, down);
    expect(within(voice).getByRole('status').textContent).toBe(note);
    expect(roster(voice)).toBeNull();
    expect(voice.textContent).toContain('Connected');
    redraw(state, up);
    expect(voice.textContent).not.toContain(note);
    expect(roster(voice)).not.toBeNull();
    expect(calls).toEqual([]);
  });

  it('says nothing of it when not joined, where the old line stands', () => {
    const { voice } = panel(initialVoice, 'c', down);
    expect(voice.textContent).not.toContain(note);
    expect(voice.textContent).toContain('Voice is unavailable right now; chat still works.');
  });
});

describe('what the strip reports to the phone layout (issue #929)', () => {
  const mount = (phase: Phase, shown = true) => {
    const seen: boolean[] = [];
    render(
      <VoiceLive.Provider
        value={(live) => {
          seen.push(live);
        }}
      >
        <MicTestShown.Provider value={shown}>
          <VoicePanel voice={control(inVoice(phase, 'general'), [], { kind: 'presence', presence })} viewer={7} people={new Map()} viewed="general" onOpen={() => undefined} />
        </MicTestShown.Provider>
      </VoiceLive.Provider>,
    );
    return seen;
  };

  it.each([
    ['connecting', true],
    ['connected', true],
    ['reconnecting', true],
    ['unavailable', true],
    ['idle', false],
    ['stopped', false],
  ] as const)('%s is reported as in voice: %s', (phase, live) => {
    expect(mount(phase).at(-1)).toBe(live);
  });

  it('draws the microphone test only where the layout shows it: the phone layout hides it outside the Voice view, which ends a running test', () => {
    mount('idle', false);
    expect(screen.queryByRole('button', { name: /test microphone|stop test/i })).toBeNull();
    cleanup();
    mount('idle', true);
    expect(screen.getByRole('button', { name: 'Test microphone' })).toBeTruthy();
  });
});

// Issue #1028: on a phone under Chat and Channels, in voice, the section is
// one row of four controls: where voice is (a button to the Voice view), Hold
// to talk (or the open microphone's level), Mute and Leave voice. The rest is
// in the Voice view, where the lobby gives no VoiceStrip.
describe('the phone strip under Chat and Channels (issue #1028)', () => {
  // Three in the room, the viewer (7) among them, and member-1 (3) sharing.
  const crowd: Snapshot = {
    kind: 'presence',
    presence: {
      ...presence,
      rooms: [
        {
          participants: [3, 5, 7].map((id) => ({
            principal_id: id,
            can_publish: true,
            transmitting: false,
            joined_at: '2026-10-18T00:00:00Z',
            ...(id === 3 ? { sharing: true, sharing_audio: true } : {}),
          })),
        },
      ],
    },
  };
  const people: ReadonlyMap<number, Person> = new Map([
    [3, { name: 'member-1', kind: null }],
    [5, { name: 'member-2', kind: null }],
  ]);
  const mount = (state: VoiceState, opens: (() => void) | null) => {
    const calls: string[] = [];
    const voice = control(state, calls, crowd);
    render(
      <VoiceStrip.Provider value={opens}>
        <VoicePanel voice={voice} viewer={7} people={people} viewed="channel-a" onOpen={() => undefined} />
      </VoiceStrip.Provider>,
    );
    return { calls, voice: screen.getByRole('region', { name: 'Voice' }) };
  };
  const buttons = (voice: HTMLElement) => within(voice).queryAllByRole('button').map((b) => b.textContent);
  const stateName: Record<'connecting' | 'connected' | 'reconnecting', string> = {
    connecting: 'Connecting to #channel-a',
    connected: 'In voice in #channel-a',
    reconnecting: 'Reconnecting to #channel-a',
  };

  it.each(['connecting', 'connected', 'reconnecting'] as const)('is the four controls, and nothing else, while %s', (phase) => {
    const { voice } = mount(inVoice(phase, 'channel-a'), () => undefined);
    expect(within(voice).getByRole('button', { name: stateName[phase] })).toBeTruthy();
    expect(buttons(voice)).toEqual([stateName[phase], 'Hold to talk', 'Mute', 'Leave voice']);
    // No roster, status line, hint or share note is drawn; the notes and
    // Voice options are kept, hidden, for the Voice view.
    expect(within(voice).queryByRole('list')).toBeNull();
    expect(within(voice).queryByText('Microphone in use')).toBeNull();
    expect(voice.querySelector('.voice-phase')).toBeNull();
    expect(voice.querySelector('.voice-notes')?.hasAttribute('hidden')).toBe(true);
    expect(voice.querySelector('.voice-options')?.hasAttribute('hidden')).toBe(true);
  });

  it('is the four controls with the open microphone, its level in place of Hold to talk', () => {
    const { voice } = mount({ ...inVoice('connected', 'channel-a'), mode: 'open' }, () => undefined);
    expect(buttons(voice)).toEqual([stateName.connected, 'Mute', 'Leave voice']);
    const meter = within(voice).getByRole('meter', { name: 'Microphone level' });
    // The words stay, for a screen reader, in their own span, which the strip
    // hides on screen so the meter has the room.
    expect(meter.parentElement?.querySelector('.open-mic-label')?.textContent).toBe('Open microphone');
  });

  it('opens the Voice view from the state button, and its Leave voice leaves', () => {
    let opened = 0;
    const { voice, calls } = mount(inVoice('connected', 'channel-a'), () => {
      opened++;
    });
    fireEvent.click(within(voice).getByRole('button', { name: stateName.connected }));
    expect(opened).toBe(1);
    fireEvent.click(within(voice).getByRole('button', { name: 'Leave voice' }));
    expect(calls).toEqual(['leave']);
  });

  it('is the whole section in the Voice view: who is in voice, the status, and every control', () => {
    const { voice } = mount(inVoice('connected', 'channel-a'), null);
    expect(within(voice).queryByRole('button', { name: stateName.connected })).toBeNull();
    expect(within(voice).getByRole('list', { name: '3 in voice' })).toBeTruthy();
    expect(voice.querySelector('.voice-phase')?.textContent).toBe('Connected in #channel-a');
    for (const name of ['Hold to talk', 'Mute', 'Deafen', 'Share screen', 'Watch', 'Voice options', 'Leave voice']) {
      expect(within(voice).getByRole('button', { name })).toBeTruthy();
    }
    expect(voice.querySelector('.voice-notes')?.hasAttribute('hidden')).toBe(false);
  });

  it('keeps Voice options as they were left across the strip and the Voice view', () => {
    const voiceControl = control(inVoice('connected', 'channel-a'), [], crowd);
    const page = (opens: (() => void) | null) => (
      <VoiceStrip.Provider value={opens}>
        <VoicePanel voice={voiceControl} viewer={7} people={people} viewed="channel-a" onOpen={() => undefined} />
      </VoiceStrip.Provider>
    );
    const { rerender } = render(page(null));
    const voice = screen.getByRole('region', { name: 'Voice' });
    const options = () => voice.querySelector('.voice-options')?.hasAttribute('hidden');
    fireEvent.click(within(voice).getByRole('button', { name: 'Voice options' }));
    expect(options()).toBe(false);
    rerender(page(() => undefined));
    expect(options()).toBe(true);
    rerender(page(null));
    expect(options()).toBe(false);
  });

  it('is not the strip out of voice: Join voice is the Voice view\'s, and the lobby hides the dock', () => {
    const { voice } = mount(initialVoice, () => undefined);
    expect(within(voice).getByRole('button', { name: 'Join voice in #channel-a' })).toBeTruthy();
    expect(voice.classList.contains('strip')).toBe(false);
  });
});
