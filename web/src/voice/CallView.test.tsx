// The camera in the voice section (ADR-015, design §6.1, §7.1, §7.4): the
// Camera button and where it is offered, the indicators from the local track
// (S5), "Show video (n)", the roster's "camera", Voice options' words, and
// the call view's tiles and grid. Driven by a fake VoiceControl: no SDK, no
// network, no camera.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { VoiceStrip } from '../phone';
import type { VoicePresenceV1 } from '../schema/voice';
import type { Person } from '../useLiveChannel';
import { cameraOffForYou, watchOnlyCamera, type CameraState } from './camera';
import { CallView, saidFor, type Tile } from './CallView';
import { CameraTest, previewNote } from './CameraTest';
import { initialVoice, VoiceClient, type VoiceState } from './connection';
import { FakeClock } from './fakes';
import type { ShareFeatures } from './share';
import type { Snapshot, VoiceControl } from './useVoice';
import { VoicePanel } from './VoicePanel';

const chrome: ShareFeatures = { canShare: true, chromium: true, platform: 'linux' };

const roomWith = (cameras: readonly number[], sharer: number | null = null): VoicePresenceV1 => ({
  schema: 'conch.voice_presence.v1',
  channel_id: 1,
  configured: true,
  available: true,
  rooms: [
    {
      participants: [
        { principal_id: 8, joined_at: '2026-11-03T10:00:01.000Z' },
        { principal_id: 7, joined_at: '2026-11-03T10:00:00.000Z' },
        { principal_id: 9, joined_at: '2026-11-03T10:00:02.000Z' },
      ].map((p) => ({
        ...p,
        can_publish: true,
        transmitting: false,
        ...(cameras.includes(p.principal_id) ? { camera: true } : {}),
        ...(sharer === p.principal_id ? { sharing: true, sharing_audio: true } : {}),
      })),
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

function panel(state: VoiceState, { cameras = [] as readonly number[], allowed = true, sharer = null as number | null } = {}) {
  const client = newClient();
  const snapshot: Snapshot = { kind: 'presence', presence: roomWith(cameras, sharer) };
  const { rerender } = render(<VoicePanel voice={control(state, snapshot, client)} viewer={7} people={people} viewed="general" cameraAllowed={allowed} onOpen={() => undefined} />);
  const redraw = (next: VoiceState, nextCameras: readonly number[] = cameras) => {
    rerender(<VoicePanel voice={control(next, { kind: 'presence', presence: roomWith(nextCameras, sharer) }, client)} viewer={7} people={people} viewed="general" cameraAllowed={allowed} onOpen={() => undefined} />);
  };
  return { client, redraw, voice: screen.getByRole('region', { name: 'Voice' }) };
}

afterEach(() => {
  cleanup();
  window.localStorage.clear();
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

  it('Hide non-video participants hides every camera-off tile, the viewer included, and brings them back when off', () => {
    panel(inVoice({ viewing: true }), { cameras: [8] });
    const call = screen.getByRole('region', { name: 'Video call' });
    const toggle = within(call).getByRole('button', { name: 'Hide non-video participants' });
    expect(toggle.getAttribute('aria-pressed')).toBe('false');
    expect(captions()).toEqual(['member-1', 'member-2', 'You']);
    fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-pressed')).toBe('true');
    expect(captions()).toEqual(['member-1']);
    fireEvent.click(toggle);
    expect(captions()).toEqual(['member-1', 'member-2', 'You']);
  });

  it('with the filter on and no camera or share, closes to the voice bar and focuses Show video when a camera returns without opening itself', () => {
    const { client, redraw } = panel(inVoice({ viewing: true }));
    const view = vi.spyOn(client, 'viewVideo');
    fireEvent.click(screen.getByRole('button', { name: 'Hide non-video participants' }));
    expect(view).toHaveBeenCalledWith(false);
    redraw(inVoice());
    expect(screen.queryByRole('region', { name: 'Video call' })).toBeNull();
    expect(screen.queryByRole('button', { name: /Show video/ })).toBeNull();
    redraw(inVoice(), [8]);
    const show = screen.getByRole('button', { name: 'Show video (1)' });
    expect(document.activeElement).toBe(show);
    expect(screen.queryByRole('region', { name: 'Video call' })).toBeNull();
  });

  it('with the filter on and no camera, a watched share stays on the stage alone and the view does not close', () => {
    const { client } = panel(inVoice({ viewing: true, watching: true }), { sharer: 9 });
    const view = vi.spyOn(client, 'viewVideo');
    const share = screen.getByRole('region', { name: 'Shared screen' });
    fireEvent.click(screen.getByRole('button', { name: 'Hide non-video participants' }));
    expect(view).not.toHaveBeenCalledWith(false);
    expect(screen.getByRole('region', { name: 'Video call' })).toBeTruthy();
    expect(screen.getByRole('region', { name: 'Shared screen' })).toBe(share);
    expect(share.closest('li')?.className).toBe('call-share main');
    expect(captions()).toEqual(['share']);
  });

  it('the last camera going with the filter on is still announced after the view closes', () => {
    const { redraw } = panel(inVoice({ viewing: true }), { cameras: [8] });
    fireEvent.click(screen.getByRole('button', { name: 'Hide non-video participants' }));
    redraw(inVoice({ viewing: true }), []);
    expect(screen.getByText("member-1's camera is off").getAttribute('role')).toBe('status');
    redraw(inVoice(), []);
    expect(screen.getByText("member-1's camera is off").getAttribute('role')).toBe('status');
  });

  it('attaches each camera to its tile and lets go when the tile goes', () => {
    const client = newClient();
    const attach = vi.spyOn(client, 'attachCamera');
    const tiles = [{ principal: 8, name: 'member-1', me: false, camera: true }];
    const { unmount } = render(<CallView client={client} state={inVoice({ viewing: true })} tiles={tiles} hidden={0} open share={null} />);
    expect(attach.mock.calls[0]?.[0]).toBe(8);
    expect(attach.mock.calls[0]?.[1]).toBeInstanceOf(HTMLVideoElement);
    unmount();
    expect(attach.mock.calls.at(-1)).toEqual([8, null]);
  });
});

// Small groups (C6b, design §7.4, §7.7): the CallView alone, its tiles given.
const tile = (principal: number, name: string, camera = true, me = false): Tile => ({ principal, name, me, camera });
const threeTiles = [tile(8, 'member-1'), tile(9, 'member-2', false), tile(7, 'You', true, true)];

function callOf(tiles: readonly Tile[], patch: Partial<VoiceState> = {}) {
  const client = newClient();
  const draw = (t: readonly Tile[], p: Partial<VoiceState>, open: boolean) => <CallView client={client} state={inVoice({ viewing: open, ...p })} tiles={t} hidden={0} open={open} share={null} />;
  const { rerender } = render(draw(tiles, patch, true));
  const redraw = (t: readonly Tile[], p: Partial<VoiceState> = patch, open = true) => {
    rerender(draw(t, p, open));
  };
  return { client, redraw };
}

const callRegion = () => screen.getByRole('region', { name: 'Video call' });
const itemOf = (name: string): HTMLElement => {
  const li = within(callRegion())
    .getAllByRole('listitem')
    .find((x) => x.querySelector('figcaption')?.textContent === name);
  if (li === undefined) throw new Error(`no tile for ${name}`);
  return li;
};
const captions = () => within(callRegion()).getAllByRole('listitem').map((li) => li.querySelector('figcaption')?.textContent ?? 'share');
const listClass = () => callRegion().querySelector('ul')?.className;
const said = () => within(callRegion()).getByRole('status').textContent;

describe('the call view in a small group', () => {
  it('a speaking person gets a ring and the word "speaking"; the order and the focus stay where they were', () => {
    const { redraw } = callOf(threeTiles);
    const enlarge = within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" });
    enlarge.focus();
    expect(callRegion().textContent).not.toContain('speaking');
    redraw(threeTiles, { speaking: new Set([9]) });
    expect(itemOf('member-2').classList.contains('speaking')).toBe(true);
    expect(within(itemOf('member-2')).getByText('speaking')).toBeTruthy();
    expect(itemOf('member-1').classList.contains('speaking')).toBe(false);
    expect(captions()).toEqual(['member-1', 'member-2', 'You']);
    redraw(threeTiles, { speaking: new Set([7, 8]) });
    expect(within(itemOf('You')).getByText('speaking')).toBeTruthy();
    expect(within(itemOf('member-1')).getByText('speaking')).toBeTruthy();
    expect(within(itemOf('member-2')).queryByText('speaking')).toBeNull();
    expect(captions()).toEqual(['member-1', 'member-2', 'You']);
    expect(document.activeElement).toBe(enlarge);
  });

  it('only a tile with a camera is a button, the self-view included: a "Camera off" tile enlarges nothing', () => {
    callOf(threeTiles);
    const names = within(callRegion())
      .getAllByRole('button')
      .map((b) => b.getAttribute('aria-label') ?? b.textContent);
    expect(names.filter((n) => n.startsWith('Enlarge'))).toEqual(["Enlarge member-1's video", 'Enlarge your video']);
    expect(within(itemOf('member-2')).queryByRole('button')).toBeNull();
    // The video keeps its own label, and the name is visible text.
    expect(within(itemOf('member-1')).getByLabelText("member-1's camera")).toBeInstanceOf(HTMLVideoElement);
    expect(within(itemOf('member-1')).getByText('member-1').tagName).toBe('FIGCAPTION');
  });

  it('a click enlarges a tile, the rest go in a strip, and focus moves to it; Escape returns, focus on the tile it came from', () => {
    callOf(threeTiles);
    expect(listClass()).toBe('call-grid cols-2 rows-2');
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    expect(listClass()).toBe('call-grid focus');
    expect(itemOf('member-1').classList.contains('main')).toBe(true);
    expect(itemOf('member-2').classList.contains('in-strip')).toBe(true);
    expect(itemOf('You').classList.contains('in-strip')).toBe(true);
    // The order is the same: only the classes change.
    expect(captions()).toEqual(['member-1', 'member-2', 'You']);
    // In words, and focus on the enlarged tile.
    expect(callRegion().textContent).toContain("member-1's video is enlarged");
    expect(document.activeElement?.getAttribute('aria-label')).toBe("member-1's video is enlarged");
    // The enlarged tile is no button now; the self-view in the strip still is.
    expect(within(callRegion()).queryByRole('button', { name: "Enlarge member-1's video" })).toBeNull();
    expect(within(callRegion()).getByRole('button', { name: 'Enlarge your video' })).toBeTruthy();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape', code: 'Escape' });
    expect(listClass()).toBe('call-grid cols-2 rows-2');
    expect(callRegion().textContent).not.toContain('enlarged');
    expect(document.activeElement).toBe(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
  });

  it('"Back to grid" returns too; a tile in the strip is enlarged in place of the other, and focus goes back to it', () => {
    callOf(threeTiles);
    expect(within(callRegion()).queryByRole('button', { name: 'Back to grid' })).toBeNull();
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    fireEvent.click(within(callRegion()).getByRole('button', { name: 'Enlarge your video' }));
    expect(itemOf('You').classList.contains('main')).toBe(true);
    expect(itemOf('member-1').classList.contains('in-strip')).toBe(true);
    expect(document.activeElement?.getAttribute('aria-label')).toBe('Your video is enlarged');
    fireEvent.click(within(callRegion()).getByRole('button', { name: 'Back to grid' }));
    expect(listClass()).toBe('call-grid cols-2 rows-2');
    expect(document.activeElement).toBe(within(callRegion()).getByRole('button', { name: 'Enlarge your video' }));
  });

  it('the enlarged tile has Full screen, as the share has, on the tile alone', () => {
    callOf(threeTiles);
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    Object.defineProperty(document, 'fullscreenEnabled', { value: true, configurable: true });
    const frame = itemOf('member-1').querySelector('figure');
    if (frame === null) throw new Error('no frame');
    const requested = vi.fn(() => Promise.resolve());
    frame.requestFullscreen = requested;
    // Only the enlarged tile has it.
    expect(within(callRegion()).getAllByRole('button', { name: 'Full screen' })).toHaveLength(1);
    fireEvent.click(within(itemOf('member-1')).getByRole('button', { name: 'Full screen' }));
    expect(requested).toHaveBeenCalledTimes(1);
  });

  it("Escape inside full screen is the browser's, to leave full screen: the tile stays enlarged", () => {
    callOf(threeTiles);
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    const frame = itemOf('member-1').querySelector('figure');
    Object.defineProperty(document, 'fullscreenElement', { value: frame, configurable: true });
    try {
      fireEvent(document, new Event('fullscreenchange'));
      fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape', code: 'Escape' });
      expect(listClass()).toBe('call-grid focus');
    } finally {
      Object.defineProperty(document, 'fullscreenElement', { value: null, configurable: true });
    }
    // Leaving full screen drops focus; it comes back to Full screen, and the
    // next Escape returns to the grid.
    (document.activeElement as HTMLElement | null)?.blur();
    fireEvent(document, new Event('fullscreenchange'));
    expect(document.activeElement).toBe(within(itemOf('member-1')).getByRole('button', { name: 'Full screen' }));
    fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape', code: 'Escape' });
    expect(listClass()).toBe('call-grid cols-2 rows-2');
  });

  it("the enlarged person's camera going returns to the grid: focus to Hide video, and the live region says why", () => {
    const { redraw } = callOf(threeTiles);
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    redraw([tile(8, 'member-1', false), tile(9, 'member-2', false), tile(7, 'You', true, true)]);
    expect(listClass()).toBe('call-grid cols-2 rows-2');
    expect(within(itemOf('member-1')).getByText('Camera off')).toBeTruthy();
    expect(document.activeElement).toBe(within(callRegion()).getByRole('button', { name: 'Hide video' }));
    expect(said()).toBe("member-1's camera is off");
  });

  it('the enlarged person leaving returns to the grid too; focus that was elsewhere stays there', () => {
    const { redraw } = callOf(threeTiles);
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    redraw([tile(9, 'member-2', false), tile(7, 'You', true, true)]);
    expect(listClass()).toBe('call-grid cols-2 rows-1');
    expect(document.activeElement).toBe(within(callRegion()).getByRole('button', { name: 'Hide video' }));
    // Someone typing elsewhere keeps their place.
    const outside = document.createElement('input');
    document.body.append(outside);
    try {
      redraw([tile(8, 'member-1'), tile(9, 'member-2', false), tile(7, 'You', true, true)]);
      fireEvent.click(within(callRegion()).getByRole('button', { name: 'Enlarge your video' }));
      outside.focus();
      redraw([tile(8, 'member-1'), tile(9, 'member-2', false), tile(7, 'You', false, true)]);
      expect(listClass()).toBe('call-grid cols-2 rows-2');
      expect(document.activeElement).toBe(outside);
    } finally {
      outside.remove();
    }
  });

  it('a focused tile that goes, not enlarged, hands focus to Hide video while the view stays open', () => {
    // Two cameras with camera-off tiles filtered out: focus sits on member-1's
    // tile, member-1's camera goes off and the filter drops the tile.
    const { redraw } = callOf([tile(8, 'member-1'), tile(9, 'member-2'), tile(7, 'You', true, true)]);
    within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }).focus();
    redraw([tile(9, 'member-2'), tile(7, 'You', true, true)]);
    expect(document.activeElement).toBe(within(callRegion()).getByRole('button', { name: 'Hide video' }));
  });

  it("the live region says someone else's camera starting and stopping, never the person's own", () => {
    const off = [tile(8, 'member-1', false), tile(9, 'member-2', false), tile(7, 'You', false, true)];
    const { redraw } = callOf(off);
    expect(said()).toBe('');
    redraw([tile(8, 'member-1'), tile(9, 'member-2', false), tile(7, 'You', false, true)]);
    expect(said()).toBe("member-1's camera is on");
    // The person's own camera is CameraNote's, said once.
    redraw([tile(8, 'member-1'), tile(9, 'member-2', false), tile(7, 'You', true, true)]);
    expect(said()).toBe("member-1's camera is on");
    redraw([tile(8, 'member-1', false), tile(9, 'member-2'), tile(7, 'You', true, true)]);
    expect(said()).toBe("member-2's camera is on. member-1's camera is off");
  });

  it('the live region lets go of what it said after a while, so it is not read again as new', () => {
    vi.useFakeTimers();
    try {
      const { redraw } = callOf([tile(8, 'member-1', false), tile(7, 'You', false, true)]);
      redraw([tile(8, 'member-1'), tile(7, 'You', false, true)]);
      expect(said()).toBe("member-1's camera is on");
      act(() => {
        vi.advanceTimersByTime(saidFor - 1);
      });
      expect(said()).toBe("member-1's camera is on");
      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(said()).toBe('');
    } finally {
      vi.useRealTimers();
    }
  });

  it('the enlarged camera going while it is full screen: it leaves full screen, and focus goes to Hide video once it has', () => {
    const { redraw } = callOf(threeTiles);
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    const frame = itemOf('member-1').querySelector('figure');
    const exit = vi.fn(() => Promise.resolve());
    Object.defineProperty(document, 'exitFullscreen', { value: exit, configurable: true });
    Object.defineProperty(document, 'fullscreenElement', { value: frame, configurable: true });
    try {
      fireEvent(document, new Event('fullscreenchange'));
      redraw([tile(8, 'member-1', false), tile(9, 'member-2', false), tile(7, 'You', true, true)]);
      expect(exit).toHaveBeenCalledTimes(1);
      expect(listClass()).toBe('call-grid cols-2 rows-2');
      const hide = within(callRegion()).getByRole('button', { name: 'Hide video' });
      expect(document.activeElement).not.toBe(hide);
      // The browser leaves full screen, dropping focus; then it moves.
      Object.defineProperty(document, 'fullscreenElement', { value: null, configurable: true });
      (document.activeElement as HTMLElement | null)?.blur();
      fireEvent(document, new Event('fullscreenchange'));
      expect(document.activeElement).toBe(hide);
      expect(said()).toBe("member-1's camera is off");
    } finally {
      Object.defineProperty(document, 'fullscreenElement', { value: null, configurable: true });
      Reflect.deleteProperty(document, 'exitFullscreen');
    }
  });

  it('says nothing while the view is closed, nor for the cameras already on when it opens', () => {
    const { redraw } = panel(inVoice());
    redraw(inVoice(), [8]);
    redraw(inVoice(), [8, 9]);
    expect(screen.queryByText(/camera is (on|off)/)).toBeNull();
    redraw(inVoice({ viewing: true }), [8, 9]);
    expect(said()).toBe('');
    redraw(inVoice({ viewing: true }), [8]);
    expect(said()).toBe("member-2's camera is off");
    // Closed again: nothing.
    redraw(inVoice(), [8, 9]);
    expect(screen.queryByText(/camera is (on|off)/)).toBeNull();
  });

  it('with a share watched and the view closed, changes to cameras are not said', () => {
    const { redraw } = panel(inVoice({ watching: true }), { sharer: 9 });
    expect(screen.getByRole('region', { name: 'Shared screen' })).toBeTruthy();
    redraw(inVoice({ watching: true }), [8]);
    redraw(inVoice({ watching: true }), []);
    expect(screen.queryByText(/camera is (on|off)/)).toBeNull();
  });
});

describe('the call view beside a watched share', () => {
  const shareRegion = () => within(callRegion()).getByRole('region', { name: 'Shared screen' });
  const shareControls = (view: HTMLElement) => {
    expect(within(view).getByRole('button', { name: 'Mute share' })).toBeTruthy();
    expect(within(view).getByLabelText('Share volume')).toBeTruthy();
    expect(within(view).getByRole('button', { name: 'Full screen' })).toBeTruthy();
    expect(within(view).getByRole('button', { name: 'Stop watching' })).toBeTruthy();
  };

  it('the share is the main picture and the cameras are the strip; enlarging a camera swaps them; the share keeps its controls', () => {
    panel(inVoice({ viewing: true, watching: true, cameraTrack: 'published' }), { cameras: [8], sharer: 9 });
    const share = shareRegion();
    expect(listClass()).toBe('call-grid focus');
    expect(share.closest('li')?.className).toBe('call-share main');
    expect(share.textContent).toContain('member-2 is sharing');
    expect(captions()).toEqual(['share', 'member-1', 'member-2', 'You']);
    for (const name of ['member-1', 'member-2', 'You']) expect(itemOf(name).classList.contains('in-strip')).toBe(true);
    shareControls(share);
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    expect(itemOf('member-1').classList.contains('main')).toBe(true);
    // The same share, moved into the strip: not drawn again.
    expect(shareRegion()).toBe(share);
    expect(share.closest('li')?.className).toBe('call-share in-strip');
    shareControls(share);
    fireEvent.click(within(callRegion()).getByRole('button', { name: 'Back to the share' }));
    expect(share.closest('li')?.className).toBe('call-share main');
    expect(itemOf('member-1').classList.contains('in-strip')).toBe(true);
    expect(document.activeElement).toBe(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
  });

  it('Stop watching with a camera enlarged and the share in the strip leaves the grid too, focus on the tile that was enlarged', () => {
    const { client, redraw } = panel(inVoice({ viewing: true, watching: true }), { cameras: [8], sharer: 9 });
    fireEvent.click(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
    expect(itemOf('member-1').classList.contains('main')).toBe(true);
    const watch = vi.spyOn(client, 'watch').mockReturnValue(undefined);
    const stop = within(shareRegion()).getByRole('button', { name: 'Stop watching' });
    stop.focus();
    fireEvent.click(stop);
    expect(watch).toHaveBeenCalledWith(false);
    redraw(inVoice({ viewing: true, watching: false }));
    expect(within(callRegion()).queryByRole('region', { name: 'Shared screen' })).toBeNull();
    expect(listClass()).toBe('call-grid cols-2 rows-2');
    expect(callRegion().textContent).not.toContain('enlarged');
    expect(within(callRegion()).queryByRole('button', { name: /^Back to/ })).toBeNull();
    expect(document.activeElement).toBe(within(callRegion()).getByRole('button', { name: "Enlarge member-1's video" }));
  });

  it('Stop watching takes the share away and leaves the grid', () => {
    const { client, redraw } = panel(inVoice({ viewing: true, watching: true }), { cameras: [8], sharer: 9 });
    const watch = vi.spyOn(client, 'watch').mockReturnValue(undefined);
    fireEvent.click(within(shareRegion()).getByRole('button', { name: 'Stop watching' }));
    expect(watch).toHaveBeenCalledWith(false);
    redraw(inVoice({ viewing: true, watching: false }));
    expect(within(callRegion()).queryByRole('region', { name: 'Shared screen' })).toBeNull();
    expect(listClass()).toBe('call-grid cols-2 rows-2');
    expect(captions()).toEqual(['member-1', 'member-2', 'You']);
  });

  it('opening and closing the call view never draws the share again: alone while the view is closed, as before', () => {
    const { redraw } = panel(inVoice({ watching: true }), { cameras: [8], sharer: 9 });
    const share = screen.getByRole('region', { name: 'Shared screen' });
    expect(screen.queryByRole('region', { name: 'Video call' })).toBeNull();
    // The wrappers holding its place say nothing.
    expect(share.closest('ul')?.getAttribute('role')).toBe('none');
    expect(share.closest('li')?.getAttribute('role')).toBe('none');
    redraw(inVoice({ watching: true, viewing: true }));
    expect(shareRegion()).toBe(share);
    expect(share.closest('ul')?.getAttribute('role')).toBeNull();
    redraw(inVoice({ watching: true }));
    expect(screen.getByRole('region', { name: 'Shared screen' })).toBe(share);
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
    const { unmount } = render(<CameraTest chosen="" choose={() => Promise.resolve('cam-1')} deps={deps} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test camera' }));
    expect(await screen.findByText(previewNote)).toBeTruthy();
    await vi.waitFor(() => {
      expect(shown).toHaveLength(1);
    });
    expect(shown[0]?.className).toBe('mirrored');
    unmount();
    expect(stops).toEqual(['stop']);
  });

  it('a camera chosen while the test runs: the old capture stops, and the new camera is captured', async () => {
    const asked: (string | null)[] = [];
    const stopped: string[] = [];
    const deps = {
      capture: (choice: string | null) => {
        asked.push(choice);
        return Promise.resolve({
          show: () => undefined,
          stop: () => {
            stopped.push(choice ?? 'browser');
          },
        });
      },
    };
    let choice = 'cam-a';
    const choose = () => Promise.resolve(choice);
    const { rerender } = render(<CameraTest chosen={choice} choose={choose} deps={deps} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test camera' }));
    await vi.waitFor(() => {
      expect(asked).toEqual(['cam-a']);
    });
    choice = 'cam-b';
    rerender(<CameraTest chosen={choice} choose={choose} deps={deps} />);
    await vi.waitFor(() => {
      expect(asked).toEqual(['cam-a', 'cam-b']);
    });
    expect(stopped).toEqual(['cam-a']);
  });

  it('a chosen camera that is not connected is said, and nothing is captured: never another camera', async () => {
    let asked = 0;
    const deps = {
      capture: () => {
        asked++;
        return Promise.reject(new Error('not to be asked'));
      },
    };
    render(<CameraTest chosen="" choose={() => Promise.resolve(false)} deps={deps} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test camera' }));
    expect(await screen.findByText('Your chosen camera is not connected.')).toBeTruthy();
    expect(asked).toBe(0);
  });

  it('a refused capture says why, and nothing is shown', async () => {
    const deps = { capture: () => Promise.reject(new DOMException('no', 'NotAllowedError')) };
    render(<CameraTest chosen="" choose={() => Promise.resolve(null)} deps={deps} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test camera' }));
    expect(await screen.findByText("The browser blocked the camera. Allow it in this site's settings.")).toBeTruthy();
    expect(screen.queryByText(previewNote)).toBeNull();
  });
});

// The camera on a phone (C7, design §8, ADR-015 O9-O11): in the phone layout
// under Chat and Channels the section is the strip, which draws no call view
// and receives no video; "Camera on" is in its state button, from the local
// track alone.
describe('the phone strip with the camera (C7)', () => {
  function stripOf(state: VoiceState, opens: (() => void) | null, { cameras = [8], sharer = null as number | null } = {}) {
    const client = newClient();
    const shown = vi.spyOn(client, 'showVideo');
    const page = (next: VoiceState, opening: (() => void) | null) => (
      <VoiceStrip.Provider value={opening}>
        <VoicePanel voice={control(next, { kind: 'presence', presence: roomWith(cameras, sharer) }, client)} viewer={7} people={people} viewed="general" cameraAllowed onOpen={() => undefined} />
      </VoiceStrip.Provider>
    );
    const { rerender } = render(page(state, opens));
    return {
      shown,
      client,
      voice: screen.getByRole('region', { name: 'Voice' }),
      redraw: (next: VoiceState, opening: (() => void) | null) => {
        rerender(page(next, opening));
      },
    };
  }

  it('draws no call view and no camera tile under Chat and Channels, and is back in Voice with no tap', () => {
    const open = inVoice({ viewing: true, cameraTrack: 'published', camera: { kind: 'sending' }, cameraTracks: new Set([8]) });
    const { redraw, shown } = stripOf(open, () => undefined);
    expect(screen.queryByRole('region', { name: 'Video call' })).toBeNull();
    expect(screen.queryByLabelText("member-1's camera")).toBeNull();
    expect(shown).toHaveBeenLastCalledWith(false);
    redraw(open, null);
    expect(screen.getByRole('region', { name: 'Video call' })).toBeTruthy();
    expect(shown).toHaveBeenLastCalledWith(true);
    // And away again: the person's choice to view is kept, not asked again.
    redraw(open, () => undefined);
    expect(screen.queryByRole('region', { name: 'Video call' })).toBeNull();
    expect(shown).toHaveBeenLastCalledWith(false);
  });

  it('keeps a watched share on the stage under Chat and Channels: the share alone, no cameras', () => {
    const open = inVoice({ viewing: true, watching: true });
    stripOf(open, () => undefined, { cameras: [8], sharer: 9 });
    expect(screen.getByRole('button', { name: 'Stop watching' })).toBeTruthy();
    expect(screen.queryByRole('region', { name: 'Video call' })).toBeNull();
    expect(screen.queryByLabelText("member-1's camera")).toBeNull();
    expect(screen.queryByRole('listitem')).toBeNull();
  });

  it('says "Camera on" in the state button from the local track, and still opens the Voice view', () => {
    let opened = 0;
    const { voice, redraw } = stripOf(inVoice({ cameraTrack: 'published', camera: { kind: 'sending' } }), () => {
      opened++;
    });
    const button = within(voice).getByRole('button', { name: /Camera on/ });
    expect(button.textContent).toBe('Camera on in #general');
    expect(button.querySelector('.camera-on svg')).not.toBeNull();
    // Still four controls and the state button, no fifth.
    expect(within(voice).getAllByRole('button').map((b) => b.textContent)).toEqual(['Camera on in #general', 'Hold to talk', 'Open mic', 'Mute', 'Leave voice']);
    fireEvent.click(button);
    expect(opened).toBe(1);
    // A capture not yet sent is in use too (the device is on either way).
    redraw(inVoice({ cameraTrack: 'captured', camera: { kind: 'claiming' } }), () => undefined);
    expect(within(voice).getByRole('button', { name: /Camera on/ })).toBeTruthy();
    // Still read out when the connection is not up.
    redraw(inVoice({ phase: 'reconnecting', cameraTrack: 'captured', camera: { kind: 'resuming' } }), () => undefined);
    expect(within(voice).getByRole('button', { name: /Camera on, Reconnecting/ })).toBeTruthy();
  });

  it('never says "Camera on" because presence shows this person holding a camera', () => {
    const { voice } = stripOf(inVoice({ cameraTrack: 'none' }), () => undefined, { cameras: [7, 8] });
    expect(within(voice).getByRole('button', { name: 'In voice in #general' })).toBeTruthy();
    expect(voice.textContent).not.toContain('Camera on');
  });
});

// A hidden page stops the camera test on a phone (O9), and a desktop's keeps
// going.
describe('the camera test on a hidden page', () => {
  function hidden(phone: boolean) {
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({ matches: phone, addEventListener: () => undefined, removeEventListener: () => undefined })),
    );
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
  }
  afterEach(() => {
    vi.unstubAllGlobals();
    Reflect.deleteProperty(document, 'visibilityState');
  });
  const deps = (stops: string[], profiles: string[]) => ({
    capture: (_choice: string | null, profile: string) => {
      profiles.push(profile);
      return Promise.resolve({
        show: () => undefined,
        stop: () => {
          stops.push('stop');
        },
      });
    },
  });

  it('stops on a phone, and starts nothing when the page shows again', async () => {
    const stops: string[] = [];
    const profiles: string[] = [];
    vi.stubGlobal('matchMedia', vi.fn(() => ({ matches: true, addEventListener: () => undefined, removeEventListener: () => undefined })));
    render(<CameraTest chosen="" choose={() => Promise.resolve(null)} deps={deps(stops, profiles)} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test camera' }));
    expect(await screen.findByText(previewNote)).toBeTruthy();
    // Chosen as it starts: a phone's profile.
    expect(profiles).toEqual(['phone']);
    hidden(true);
    expect(stops).toEqual(['stop']);
    expect(screen.queryByText(previewNote)).toBeNull();
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    expect(profiles).toEqual(['phone']);
    expect(screen.queryByText(previewNote)).toBeNull();
  });

  it('keeps going on a desktop', async () => {
    const stops: string[] = [];
    const profiles: string[] = [];
    vi.stubGlobal('matchMedia', vi.fn(() => ({ matches: false, addEventListener: () => undefined, removeEventListener: () => undefined })));
    render(<CameraTest chosen="" choose={() => Promise.resolve(null)} deps={deps(stops, profiles)} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test camera' }));
    expect(await screen.findByText(previewNote)).toBeTruthy();
    expect(profiles).toEqual(['desktop']);
    hidden(false);
    expect(stops).toEqual([]);
    expect(screen.getByText(previewNote)).toBeTruthy();
  });
});
