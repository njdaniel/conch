// The voice strip says where it goes and where it is (issue #584): Join voice
// and Watch only name the channel they join, Watch only says the room is
// heard, and the channel voice is in is a way back to it while another is on
// screen. Driven by a fake VoiceControl: no SDK, no network, no device.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { VoicePresenceV1 } from '../schema/voice';
import type { Person } from '../useLiveChannel';
import { initialVoice, VoiceClient, type Phase, type VoiceState } from './connection';
import { FakeClock } from './fakes';
import { ownPageNote, soundNote, type ShareFeatures } from './share';
import type { Snapshot, VoiceControl } from './useVoice';
import { stoppedFor, VoicePanel } from './VoicePanel';

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

    it('is off, and described as who shares; on again with no description once nobody else does', () => {
      const { voice, redraw } = panel(joined(), 'c', bobShares, bob);
      const button = within(voice).getByRole('button', { name: 'Share screen', description: said });
      expect(button.hasAttribute('disabled')).toBe(true);
      const hint = document.getElementById(button.getAttribute('aria-describedby') ?? '');
      expect(hint?.textContent).toBe(said);
      expect(voice.contains(hint)).toBe(true);

      redraw(joined(), nobodyShares);
      const again = within(voice).getByRole('button', { name: 'Share screen' });
      expect(again.hasAttribute('disabled')).toBe(false);
      expect(again.hasAttribute('aria-describedby')).toBe(false);
      expect(voice.textContent).not.toContain(said);
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

    it('still says why a share of ours ended, beside who shares now', () => {
      const { voice } = panel(joined({ share: { kind: 'idle', ended: 'self' } }), 'c', bobShares, bob);
      expect(voice.textContent).toContain('You stopped sharing.');
      expect(voice.textContent).toContain(said);
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
    const { voice, redraw } = panel(joined(), 'c', bobShares, bob);
    expect(within(voice).queryByText(line)).toBeNull();
    expect(within(voice).queryByRole('button', { name: 'More about sharing' })).toBeNull();
    expect(voice.querySelector('#voice-share-about')).toBeNull();
    for (const text of full) expect(voice.textContent).not.toContain(text);
    // One line about bob's share, not two.
    expect(within(voice).getAllByText(/bob is sharing/).map((el) => el.textContent)).toEqual([said]);
    expect(within(voice).getByRole('button', { name: 'Share screen', description: said })).toBeTruthy();

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
