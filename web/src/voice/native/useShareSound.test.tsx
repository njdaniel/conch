// The page's two moments for the share's sound (issue #1854): ask main when
// the share goes live, say so when it ends (naming the share main gave), and
// the person's two clicks. A fake source records every call; no bridge, no
// helper. The checklist: event-driven work in handlers, an effect only for the
// share going live and ending, cleanup that ends what was started, no stale
// answer acting on a share that has gone.
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import type { NativeVoiceView, ShareSoundChooseResult, ShareSoundStartResult } from '../../platform/desktop/bridge';
import type { NativeShareSound, NativeVoice } from '../../platform/types';
import { initialVoice, type VoiceState } from '../connection';
import type { ShareState } from '../share';
import type { Companion, CompanionClient } from './useCompanion';
import { useShareSound, type ShareSoundUi } from './useShareSound';

afterEach(cleanup);

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve: (value: T) => void = () => undefined;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

class Sound implements NativeShareSound {
  readonly calls: string[] = [];
  startResult: Promise<ShareSoundStartResult> = Promise.resolve({ ok: true, share: 7 });
  chooseResult: Promise<ShareSoundChooseResult> = Promise.resolve({ ok: true, share: null });
  muteResult: Promise<{ ok: true } | { ok: false; error: 'lost' }> = Promise.resolve({ ok: true });
  start(): Promise<ShareSoundStartResult> {
    this.calls.push('start');
    return this.startResult;
  }
  choose(choice: 'system_excluding_call' | 'none'): Promise<ShareSoundChooseResult> {
    this.calls.push(`choose ${choice}`);
    return this.chooseResult;
  }
  mute(muted: boolean): Promise<{ ok: true } | { ok: false; error: 'lost' }> {
    this.calls.push(`mute ${String(muted)}`);
    return this.muteResult;
  }
  end(share: number): Promise<boolean> {
    this.calls.push(`end ${String(share)}`);
    return Promise.resolve(true);
  }
  shareEnded(): Promise<boolean> {
    this.calls.push('share ended');
    return Promise.resolve(true);
  }
}

const sourceOf = (sound: Sound | undefined): NativeVoice => ({ shareSound: sound }) as unknown as NativeVoice;

const companionOf = (share: ShareState): Companion => {
  const state: VoiceState = { ...initialVoice, share };
  return { client: {} as CompanionClient, state };
};
const sharing: ShareState = { kind: 'sharing', audio: false };
const idle: ShareState = { kind: 'idle', ended: null };

// null: main's switch is off, so the view carries no share sound.
const viewOf = (shareSound: NonNullable<NativeVoiceView['shareSound']> | null = { choice: 'system_excluding_call' }): NativeVoiceView => ({
  status: 'running',
  generation: 1,
  channel: 'general',
  retryable: false,
  ...(shareSound === null ? {} : { shareSound }),
});

let last: ShareSoundUi | undefined;

function Probe({ source, companion, view }: { source: NativeVoice | undefined; companion: Companion | null; view: NativeVoiceView }) {
  last = useShareSound(source, companion, view);
  return <p>{last === undefined ? 'none' : `${last.busy ? 'busy' : 'ready'}|${last.note ?? ''}`}</p>;
}

function show(sound: Sound | undefined, share: ShareState, view: NativeVoiceView = viewOf()) {
  const ui = (s: ShareState, v: NativeVoiceView) => <Probe source={sourceOf(sound)} companion={companionOf(s)} view={v} />;
  const r = render(ui(share, view));
  return { ...r, again: (s: ShareState, v: NativeVoiceView = view) => { r.rerender(ui(s, v)); } };
}

describe('the share’s sound hook', () => {
  it('is nothing without main’s share sound in the view, and asks main nothing, even for a live share', () => {
    const sound = new Sound();
    show(sound, sharing, viewOf(null));
    expect(screen.getByText('none')).toBeTruthy();
    expect(sound.calls).toEqual([]);
  });

  it('is nothing without the bridge’s calls, or without a companion', () => {
    show(undefined, sharing);
    expect(screen.getByText('none')).toBeTruthy();
    cleanup();
    const sound = new Sound();
    render(<Probe source={sourceOf(sound)} companion={null} view={viewOf()} />);
    expect(screen.getByText('none')).toBeTruthy();
    expect(sound.calls).toEqual([]);
  });

  it('asks nothing while the share is not live', () => {
    const sound = new Sound();
    show(sound, idle);
    expect(sound.calls).toEqual([]);
  });

  it('asks main once when the share goes live, and ends that share’s sound when it ends', async () => {
    const sound = new Sound();
    const r = show(sound, idle);
    r.again(sharing);
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    r.again({ kind: 'stopping' });
    expect(sound.calls).toEqual(['start', 'end 7', 'share ended']);
    r.again(idle);
    expect(sound.calls).toEqual(['start', 'end 7', 'share ended']);
  });

  it('does not ask again when the choice changes during the share, or a resume follows a dropped connection', async () => {
    const sound = new Sound();
    const r = show(sound, sharing);
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    r.again(sharing, viewOf({ choice: 'none' }));
    r.again({ kind: 'resuming' }, viewOf({ choice: 'none' }));
    r.again(sharing, viewOf({ choice: 'system_excluding_call' }));
    expect(sound.calls).toEqual(['start']);
  });

  it('a share that ends before main has answered is ended when the answer comes, and acts on nothing else', async () => {
    const sound = new Sound();
    const slow = deferred<ShareSoundStartResult>();
    sound.startResult = slow.promise;
    const r = show(sound, sharing);
    r.again(idle);
    expect(sound.calls).toEqual(['start', 'share ended']);
    await act(async () => {
      slow.resolve({ ok: true, share: 9 });
      await Promise.resolve();
    });
    expect(sound.calls).toEqual(['start', 'share ended', 'end 9']);
  });

  it('a start that main refused has nothing to end', async () => {
    const sound = new Sound();
    sound.startResult = Promise.resolve({ ok: false, error: 'not-offered' });
    const r = show(sound, sharing);
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    r.again(idle);
    expect(sound.calls).toEqual(['start', 'share ended']);
  });

  it.each(['failed', 'unavailable'] as const)('a start that %s but named the record main kept is ended with the share, so its words do not outlive it', async (error) => {
    const sound = new Sound();
    sound.startResult = Promise.resolve({ ok: false, error, share: 11 });
    const r = show(sound, sharing);
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    r.again(idle);
    expect(sound.calls).toEqual(['start', 'end 11', 'share ended']);
  });

  it('each share has its own: the next share asks again and ends its own', async () => {
    const sound = new Sound();
    const r = show(sound, sharing);
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    r.again(idle);
    sound.startResult = Promise.resolve({ ok: true, share: 8 });
    r.again(sharing);
    await waitFor(() => { expect(sound.calls).toEqual(['start', 'end 7', 'share ended', 'start']); });
    r.again(idle);
    expect(sound.calls).toEqual(['start', 'end 7', 'share ended', 'start', 'end 8', 'share ended']);
  });

  it('the end of a share that never had a sound record is told to main too, with no token (#1854 review round 2)', async () => {
    const sound = new Sound();
    sound.startResult = Promise.resolve({ ok: false, error: 'not-offered' });
    const r = show(sound, sharing, viewOf({ choice: 'none' }));
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    r.again({ kind: 'stopping' }, viewOf({ choice: 'none' }));
    expect(sound.calls).toEqual(['start', 'share ended']);
    // Once: going on to idle says nothing more.
    r.again(idle, viewOf({ choice: 'none' }));
    expect(sound.calls).toEqual(['start', 'share ended']);
  });

  it('with main’s switch off nothing is told of a share’s end', () => {
    const sound = new Sound();
    const r = show(sound, sharing, viewOf(null));
    r.again(idle, viewOf(null));
    expect(sound.calls).toEqual([]);
  });

  it('a share that is only publishing is not live: nothing is asked, and nothing is told when it ends', () => {
    const sound = new Sound();
    const r = show(sound, { kind: 'publishing' });
    expect(last?.live).toBe(false);
    r.again(idle);
    expect(sound.calls).toEqual([]);
  });

  it('the page leaving ends the share’s sound too', async () => {
    const sound = new Sound();
    const r = show(sound, sharing);
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    r.unmount();
    expect(sound.calls).toEqual(['start', 'end 7', 'share ended']);
  });
});

describe('the person’s clicks', () => {
  const click = async (act_: () => void): Promise<void> => {
    await act(async () => {
      act_();
      await Promise.resolve();
    });
  };

  it('choose asks main, shows busy until it answers, and refuses a second ask meanwhile', async () => {
    const sound = new Sound();
    const slow = deferred<ShareSoundChooseResult>();
    sound.chooseResult = slow.promise;
    show(sound, idle);
    await click(() => last?.choose('none'));
    expect(screen.getByText('busy|')).toBeTruthy();
    await click(() => last?.choose('system_excluding_call'));
    expect(sound.calls).toEqual(['choose none']);
    await act(async () => {
      slow.resolve({ ok: true, share: null });
      await Promise.resolve();
    });
    expect(screen.getByText('ready|')).toBeTruthy();
  });

  it('says a note when the choice could not be kept, and no share is made of it', async () => {
    const sound = new Sound();
    sound.chooseResult = Promise.resolve({ ok: false, error: 'not-saved' });
    show(sound, idle);
    await click(() => last?.choose('none'));
    await waitFor(() => { expect(screen.getByText("ready|That choice couldn't be saved.")).toBeTruthy(); });
    expect(sound.calls).toEqual(['choose none']);
  });

  it('a choice that put the sound on for the live share is ended with the share', async () => {
    const sound = new Sound();
    sound.startResult = Promise.resolve({ ok: false, error: 'not-offered' });
    sound.chooseResult = Promise.resolve({ ok: true, share: 12 });
    const r = show(sound, sharing, viewOf({ choice: 'none' }));
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    await click(() => last?.choose('system_excluding_call'));
    await waitFor(() => { expect(screen.getByText('ready|')).toBeTruthy(); });
    r.again(idle, viewOf({ choice: 'system_excluding_call' }));
    expect(sound.calls).toEqual(['start', 'choose system_excluding_call', 'end 12', 'share ended']);
  });

  it('a choice answered after the share ended ends what it made at once', async () => {
    const sound = new Sound();
    const slow = deferred<ShareSoundChooseResult>();
    sound.startResult = Promise.resolve({ ok: false, error: 'not-offered' });
    sound.chooseResult = slow.promise;
    const r = show(sound, sharing, viewOf({ choice: 'none' }));
    await waitFor(() => { expect(sound.calls).toEqual(['start']); });
    await click(() => last?.choose('system_excluding_call'));
    r.again(idle, viewOf({ choice: 'none' }));
    await act(async () => {
      slow.resolve({ ok: true, share: 13 });
      await Promise.resolve();
    });
    expect(sound.calls).toEqual(['start', 'choose system_excluding_call', 'share ended', 'end 13']);
  });

  it('mute asks main for the share’s mute, and says a note when it could not', async () => {
    const sound = new Sound();
    show(sound, idle);
    await click(() => last?.mute(true));
    expect(sound.calls).toEqual(['mute true']);
    sound.muteResult = Promise.resolve({ ok: false, error: 'lost' });
    await click(() => last?.mute(false));
    await waitFor(() => { expect(screen.getByText("ready|The share's sound could not be muted.")).toBeTruthy(); });
  });

  it('hands the page main’s view and the helper’s confirmed state, and nothing it made up', () => {
    const sound = new Sound();
    const view: NativeVoiceView = {
      ...viewOf({ choice: 'none', unavailable: 'no-pipewire' }),
      helper: { version: 3, phase: 'connected', capture: 'on', transmitting: false, muted: false, deafened: false, blocked: [], ptt: 'ready', shareAudio: { state: 'unavailable', problem: 'no_pipewire', muted: false, sources: 0 } },
    };
    show(sound, idle, view);
    expect(last?.view).toEqual({ choice: 'none', unavailable: 'no-pipewire' });
    expect(last?.audio).toEqual({ state: 'unavailable', problem: 'no_pipewire', muted: false, sources: 0 });
    expect(last?.live).toBe(false);
  });
});