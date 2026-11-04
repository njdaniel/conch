// The camera in the voice section (ADR-015, design §6.1, §7.1, §7.4): the
// Camera button and where it is offered, the indicators from the local track
// (S5), "Show video (n)", the roster's "camera", Voice options' words, and
// the call view's tiles and grid. Driven by a fake VoiceControl: no SDK, no
// network, no camera.
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { VoicePresenceV1 } from '../schema/voice';
import type { Person } from '../useLiveChannel';
import { cameraOffForYou, watchOnlyCamera, type CameraState } from './camera';
import { CallView } from './CallView';
import { CameraTest, previewNote } from './CameraTest';
import { initialVoice, VoiceClient, type VoiceState } from './connection';
import { FakeClock } from './fakes';
import type { ShareFeatures } from './share';
import type { Snapshot, VoiceControl } from './useVoice';
import { VoicePanel } from './VoicePanel';

const chrome: ShareFeatures = { canShare: true, chromium: true, platform: 'linux' };

const roomWith = (cameras: readonly number[]): VoicePresenceV1 => ({
  schema: 'conch.voice_presence.v1',
  channel_id: 1,
  configured: true,
  available: true,
  rooms: [
    {
      participants: [
        { principal_id: 8, can_publish: true, transmitting: false, joined_at: '2026-11-03T10:00:01.000Z', ...(cameras.includes(8) ? { camera: true } : {}) },
        { principal_id: 7, can_publish: true, transmitting: false, joined_at: '2026-11-03T10:00:00.000Z', ...(cameras.includes(7) ? { camera: true } : {}) },
        { principal_id: 9, can_publish: true, transmitting: false, joined_at: '2026-11-03T10:00:02.000Z', ...(cameras.includes(9) ? { camera: true } : {}) },
      ],
    },
  ],
});

const people: ReadonlyMap<number, Person> = new Map([
  [8, { name: 'member-1', kind: 'human' }],
  [9, { name: 'member-2', kind: 'human' }],
]);

function control(state: VoiceState, snapshot: Snapshot, client: VoiceClient): VoiceControl {
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
    setReleaseDelay: () => undefined,
    capture: () => undefined,
    join: () => undefined,
    watchOnly: () => undefined,
    leave: () => undefined,
  };
}

function newClient(): VoiceClient {
  return new VoiceClient({
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
}

const inVoice = (patch: Partial<VoiceState> = {}): VoiceState => ({ ...initialVoice, mode: 'ptt', phase: 'connected', channel: 'general', ...patch });

function panel(state: VoiceState, { cameras = [] as readonly number[], allowed = true } = {}) {
  const client = newClient();
  const snapshot: Snapshot = { kind: 'presence', presence: roomWith(cameras) };
  const { rerender } = render(<VoicePanel voice={control(state, snapshot, client)} viewer={7} people={people} viewed="general" cameraAllowed={allowed} onOpen={() => undefined} />);
  const redraw = (next: VoiceState, nextCameras: readonly number[] = cameras) => {
    rerender(<VoicePanel voice={control(next, { kind: 'presence', presence: roomWith(nextCameras) }, client)} viewer={7} people={people} viewed="general" cameraAllowed={allowed} onOpen={() => undefined} />);
  };
  return { client, redraw, voice: screen.getByRole('region', { name: 'Voice' }) };
}

afterEach(() => {
  cleanup();
});

describe('the Camera button', () => {
  it('is offered beside Mute and Share to someone holding use_camera, unpressed until the camera is on', () => {
    const { voice, client, redraw } = panel(inVoice());
    const start = vi.spyOn(client, 'startCamera').mockResolvedValue(undefined);
    const button = within(voice).getByRole('button', { name: 'Camera' });
    expect(button.getAttribute('aria-pressed')).toBe('false');
    fireEvent.click(button);
    expect(start).toHaveBeenCalledTimes(1);
    const sending: CameraState = { kind: 'sending' };
    redraw(inVoice({ camera: sending, cameraTrack: 'published' }));
    const on = within(voice).getByRole('button', { name: 'Camera' });
    expect(on.getAttribute('aria-pressed')).toBe('true');
    const stop = vi.spyOn(client, 'stopCamera').mockResolvedValue(undefined);
    fireEvent.click(on);
    expect(stop).toHaveBeenCalledTimes(1);
  });

  it('is not offered without use_camera: Voice options says the camera is off here', () => {
    const { voice } = panel(inVoice(), { allowed: false });
    expect(within(voice).queryByRole('button', { name: 'Camera' })).toBeNull();
    expect(voice.textContent).toContain(cameraOffForYou);
  });

  it('stays while the camera is on, whatever whoami says by now, so it can be turned off', () => {
    const { voice } = panel(inVoice({ camera: { kind: 'sending' }, cameraTrack: 'published' }), { allowed: false });
    expect(within(voice).getByRole('button', { name: 'Camera' }).getAttribute('aria-pressed')).toBe('true');
  });

  it('is not offered in Watch only or Listen only, which publish nothing (O14)', () => {
    const { voice, redraw } = panel(inVoice({ watchOnly: true }));
    expect(within(voice).queryByRole('button', { name: 'Camera' })).toBeNull();
    expect(voice.textContent).toContain(watchOnlyCamera);
    redraw(inVoice({ listenOnly: true }));
    expect(within(voice).queryByRole('button', { name: 'Camera' })).toBeNull();
  });

  it('no key turns it on: a key press anywhere starts nothing', () => {
    const { client } = panel(inVoice());
    const start = vi.spyOn(client, 'startCamera').mockResolvedValue(undefined);
    for (const code of ['KeyC', 'KeyV', 'Space', 'Enter']) fireEvent.keyDown(window, { code, key: code });
    expect(start).not.toHaveBeenCalled();
  });
});

describe('the indicators (S5)', () => {
  it('"Camera in use" while captured and not sent, "Sending video" once published, from the local track alone', () => {
    const { voice, redraw } = panel(inVoice({ camera: { kind: 'claiming' }, cameraTrack: 'captured' }));
    expect(voice.textContent).toContain('Camera in use');
    expect(voice.textContent).not.toContain('Sending video');
    redraw(inVoice({ camera: { kind: 'sending' }, cameraTrack: 'published' }));
    expect(voice.textContent).toContain('Sending video');
    expect(voice.textContent).not.toContain('Camera in use');
  });

  it('presence saying this person holds a camera never says "Sending video" on this page', () => {
    const { voice } = panel(inVoice(), { cameras: [7] });
    expect(voice.textContent).not.toContain('Sending video');
    expect(voice.textContent).not.toContain('Camera in use');
    // And the viewer's own chip does not say "camera": that is presence.
    const mine = within(voice).getAllByRole('listitem').find((li) => li.textContent.includes('You'));
    expect(mine?.textContent).not.toContain('camera');
  });

  it('others see "camera" on the chip of someone with a camera on', () => {
    const { voice } = panel(inVoice(), { cameras: [8] });
    const theirs = within(voice).getAllByRole('listitem').find((li) => li.textContent.includes('member-1'));
    expect(theirs?.textContent).toContain('camera');
  });

  it.each([
    [{ kind: 'failed', reason: 'blocked' }, "The browser blocked the camera. Allow it in this site's settings."],
    [{ kind: 'failed', reason: 'full' }, 'This call already has 9 cameras on.'],
    [{ kind: 'failed', reason: 'stale' }, 'Your camera was turned off.'],
    [{ kind: 'off', ended: 'operator' }, 'An operator turned off your camera.'],
    [{ kind: 'off', ended: 'idle' }, 'Your camera stopped: nothing was being sent.'],
  ] as const)('says %j as %j, and voice and chat carry on', (camera, words) => {
    const { voice } = panel(inVoice({ camera }));
    expect(within(voice).getByText(words).getAttribute('role')).toBe('status');
    expect(within(voice).getByRole('button', { name: 'Leave voice' })).toBeTruthy();
  });
});

describe('Show video', () => {
  it('appears while someone else has a camera on, says how many, and opens the view only when pressed (O8)', () => {
    const { voice, client, redraw } = panel(inVoice());
    expect(within(voice).queryByRole('button', { name: /Show video/ })).toBeNull();
    redraw(inVoice(), [8, 9]);
    const show = within(voice).getByRole('button', { name: 'Show video (2)' });
    expect(screen.queryByRole('region', { name: 'Video call' })).toBeNull();
    const view = vi.spyOn(client, 'viewVideo').mockReturnValue(undefined);
    fireEvent.click(show);
    expect(view).toHaveBeenCalledWith(true);
    redraw(inVoice({ viewing: true }), [8, 9]);
    // In the controls, and in the view itself.
    const hide = within(voice).getAllByRole('button', { name: 'Hide video' });
    expect(hide).toHaveLength(2);
    fireEvent.click(hide[0] ?? voice);
    expect(view).toHaveBeenLastCalledWith(false);
  });
});

describe('the call view', () => {
  it('gives each person in voice a tile, in join order with the viewer last, on a 2x2 grid for three', () => {
    panel(inVoice({ viewing: true, cameraTrack: 'published', cameraTracks: new Set([8]) }), { cameras: [8] });
    const call = screen.getByRole('region', { name: 'Video call' });
    const grid = call.querySelector('.call-grid');
    expect(grid?.className).toBe('call-grid cols-2 rows-2');
    const tiles = within(call).getAllByRole('listitem').map((li) => li.querySelector('figcaption')?.textContent);
    expect(tiles).toEqual(['member-1', 'member-2', 'You']);
    // A camera that is on plays muted, inline and by itself; one that is off
    // shows the name and "Camera off".
    const video = within(call).getByLabelText("member-1's camera");
    expect(video).toBeInstanceOf(HTMLVideoElement);
    expect((video as HTMLVideoElement).muted).toBe(true);
    expect(video.hasAttribute('playsinline')).toBe(true);
    expect(video.hasAttribute('autoplay')).toBe(true);
    expect(within(call).getAllByText('Camera off')).toHaveLength(1);
    // The viewer's own tile is mirrored, by a class.
    expect(within(call).getByLabelText('Your camera').className).toBe('mirrored');
  });

  it('"Hide self-view" hides only the viewer\'s tile, and "Sending video" stays', () => {
    const { voice } = panel(inVoice({ viewing: true, camera: { kind: 'sending' }, cameraTrack: 'published' }));
    const call = screen.getByRole('region', { name: 'Video call' });
    fireEvent.click(within(call).getByRole('button', { name: 'Hide self-view' }));
    expect(within(call).queryByLabelText('Your camera')).toBeNull();
    expect(within(call).getAllByRole('listitem')).toHaveLength(2);
    expect(voice.textContent).toContain('Sending video');
  });

  it('attaches each camera to its tile and lets go when the tile goes', () => {
    const client = newClient();
    const attach = vi.spyOn(client, 'attachCamera');
    const tiles = [{ principal: 8, name: 'member-1', me: false, camera: true }];
    const { unmount } = render(<CallView client={client} state={inVoice({ viewing: true })} tiles={tiles} hidden={0} />);
    expect(attach.mock.calls[0]?.[0]).toBe(8);
    expect(attach.mock.calls[0]?.[1]).toBeInstanceOf(HTMLVideoElement);
    unmount();
    expect(attach.mock.calls.at(-1)).toEqual([8, null]);
  });
});

describe('the camera test', () => {
  it('shows a preview, "Preview: not sent", and stops the capture when it is taken away', async () => {
    const stops: string[] = [];
    const shown: (HTMLVideoElement | null)[] = [];
    const deps = {
      capture: () =>
        Promise.resolve({
          show: (el: HTMLVideoElement | null) => {
            shown.push(el);
          },
          stop: () => {
            stops.push('stop');
          },
        }),
    };
    const { unmount } = render(<CameraTest choice="cam-1" deps={deps} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test camera' }));
    expect(await screen.findByText(previewNote)).toBeTruthy();
    await vi.waitFor(() => {
      expect(shown).toHaveLength(1);
    });
    expect(shown[0]?.className).toBe('mirrored');
    unmount();
    expect(stops).toEqual(['stop']);
  });

  it('a refused capture says why, and nothing is shown', async () => {
    const deps = { capture: () => Promise.reject(new DOMException('no', 'NotAllowedError')) };
    render(<CameraTest choice={null} deps={deps} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test camera' }));
    expect(await screen.findByText("The browser blocked the camera. Allow it in this site's settings.")).toBeTruthy();
    expect(screen.queryByText(previewNote)).toBeNull();
  });
});
