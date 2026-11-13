// Share and watch in the desktop strip (issue #1582, D4-05), driven by a fake
// companion client: each state, each refusal, the notes, the quality choices
// and the tiles. No room, no portal, no network.
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { CompanionGrantError } from '../../platform/desktop/bridge';
import type { VoicePresenceV2 } from '../../schema/voice';
import type { Person } from '../../useLiveChannel';
import { companionDefaultQuality, companionQualities, companionRefusals, initialVoice, type VoiceState } from '../connection';
import { askedText } from '../sdk';
import type { SettledQuality, ShareDirection, ShareSample, ShareState } from '../share';
import { estimateWords, sourceSixtyNote } from '../ShareQualityMenu';
import { NativeStreams, ShareButton, ShareNotes, shareDisabledReason } from './NativeShare';
import type { Companion, CompanionClient } from './useCompanion';
import { shareCauseWords, shareDisabledWords, shareElsewhereWords, shareFailedWords, shareFullWords, shareSourceClosedWords, shareUnavailableWords, silentShareBefore, silentShareWhile } from './words';

afterEach(cleanup);

function fake(over: Partial<VoiceState> = {}, stats: ShareSample | null = null): { readonly companion: Companion; readonly calls: string[]; readonly reads: [ShareDirection, number | undefined][] } {
  const calls: string[] = [];
  const reads: [ShareDirection, number | undefined][] = [];
  const client: CompanionClient = {
    subscribe: () => () => undefined,
    snapshot: () => state,
    startCompanionShare: (c) => { calls.push(`share ${c}`); },
    watchCompanionStream: (c, p) => { calls.push(`watch ${c} ${String(p)}`); },
    stopShare: () => { calls.push('stop'); },
    watchStream: (p, on) => { calls.push(`watchStream ${String(p)} ${String(on)}`); },
    focusShare: (p) => { calls.push(`focus ${String(p)}`); },
    setShareQuality: (q) => { calls.push(`quality ${q.resolution}${String(q.frameRate)}`); return Promise.resolve(); },
    presence: () => undefined,
    attachStream: () => undefined,
    readShareStats: (direction, principal) => { reads.push([direction, principal]); return Promise.resolve(stats); },
  };
  // The real client starts at the desktop's default, 720p30 (Q-D4-3).
  const state: VoiceState = { ...initialVoice, shareQuality: companionDefaultQuality, ...over };
  return { companion: { client, state }, calls, reads };
}

const idleShare = (over: Partial<Extract<ShareState, { kind: 'idle' }>> = {}): ShareState => ({ kind: 'idle', ended: null, ...over });
const reasonOf = (over: { joined?: string | null; shown?: string | null; live?: boolean } = {}) =>
  shareDisabledReason({ joined: 'general', shown: 'general', live: true, ...over });

function strip(c: Companion, over: { joined?: string | null; shown?: string | null; live?: boolean; soundRoute?: boolean } = {}) {
  const joined = over.joined === undefined ? 'general' : over.joined;
  const reason = reasonOf(over);
  return render(
    <>
      <ShareButton companion={c} joined={joined} reason={reason} />
      <ShareNotes companion={c} joined={joined} reason={reason} {...(over.soundRoute === undefined ? {} : { soundRoute: over.soundRoute })} />
    </>,
  );
}

describe('Share is offered only in the joined channel while the helper is connected', () => {
  it('starts the share in the joined channel from the click', () => {
    const t = fake();
    strip(t.companion);
    fireEvent.click(screen.getByRole('button', { name: 'Share screen' }));
    expect(t.calls).toEqual(['share general']);
  });

  it.each([
    ['not in voice', { joined: null }, shareDisabledWords.notJoined],
    ['another channel on screen', { shown: 'random' }, shareElsewhereWords('general')],
    ['the helper not connected', { live: false }, shareDisabledWords.connecting],
  ])('is disabled, with its reason, when %s', (_name, over, reason) => {
    const t = fake();
    strip(t.companion, over);
    const button = screen.getByRole<HTMLButtonElement>('button', { name: 'Share screen' });
    expect(button.disabled).toBe(true);
    expect(screen.getByText(reason)).toBeTruthy();
    expect(button.getAttribute('aria-describedby')).toBe('native-share-why');
    fireEvent.click(button);
    expect(t.calls).toEqual([]);
    // Nothing is promised about sound or quality while it is off.
    expect(screen.queryByText(silentShareBefore)).toBeNull();
    expect(screen.queryByLabelText('Share quality')).toBeNull();
  });

  it.each([
    ['no-portal' as const, 'This desktop has no screen-cast portal'],
    ['not-wayland' as const, shareUnavailableWords['not-wayland']],
  ])('says so, from the grant, on a desktop that cannot share (%s; Q-D4-4), and is not locked off', (companionShareUnavailable, words) => {
    const t = fake({ companionShareUnavailable });
    strip(t.companion);
    expect(screen.getByText(words)).toBeTruthy();
    // Not locked: the next click asks again, and the grant is what decides.
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Share screen' }).disabled).toBe(false);
  });
});

describe('the silent share', () => {
  it('is said before the picker opens, and the quality is chosen first', () => {
    strip(fake().companion);
    expect(screen.getByText(silentShareBefore)).toBeTruthy();
    expect(silentShareBefore).toBe("Your screen's sound isn't shared from the Linux app yet. Others will see your screen without sound.");
  });

  it('is said while the picker is open', () => {
    strip(fake({ share: { kind: 'picking' } }).companion);
    expect(screen.getByText(silentShareBefore)).toBeTruthy();
  });

  it('is said in short while sharing', () => {
    strip(fake({ phase: 'connected', channel: 'general', companion: true, share: { kind: 'sharing', audio: false } }).companion);
    expect(screen.getByText(silentShareWhile)).toBeTruthy();
    expect(screen.queryByText(silentShareBefore)).toBeNull();
  });

  it('is not said when the client reports a sound route', () => {
    const r = strip(fake().companion, { soundRoute: true });
    expect(screen.queryByText(silentShareBefore)).toBeNull();
    r.unmount();
    strip(fake({ share: { kind: 'sharing', audio: true } }).companion, { soundRoute: true });
    expect(screen.queryByText(silentShareWhile)).toBeNull();
  });
});

describe('the indicator and Stop', () => {
  it('names the channel and offers Stop, which stops the share and nothing of voice', () => {
    const t = fake({ phase: 'connected', channel: 'general', companion: true, share: { kind: 'sharing', audio: false } });
    strip(t.companion);
    expect(screen.getByText('Sharing your screen in #general')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Share screen' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
    expect(t.calls).toEqual(['stop']);
  });

  it.each<[ShareState, string]>([
    [{ kind: 'claiming' }, 'Starting your screen share…'],
    [{ kind: 'picking' }, 'Starting your screen share…'],
    [{ kind: 'stopping' }, 'Stopping your screen share…'],
  ])('says %j', (share, words) => {
    strip(fake({ share }).companion);
    expect(screen.getByText(words)).toBeTruthy();
  });

  it('Stop can cancel a share that is starting, and is disabled while stopping', () => {
    const t = fake({ share: { kind: 'picking' } });
    const r = strip(t.companion);
    fireEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
    expect(t.calls).toEqual(['stop']);
    r.unmount();
    strip(fake({ share: { kind: 'stopping' } }).companion);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Stopping…' }).disabled).toBe(true);
  });
});

describe('the refusals, each in its own words', () => {
  it.each<[CompanionGrantError, string]>([
    ['elsewhere', 'Your screen is being shared from another device of yours'],
    ['not-yet', 'Voice is still connecting; try again in a moment'],
    ['not-in-voice', companionRefusals['not-in-voice']],
    ['ended', companionRefusals.ended],
    ['failed', companionRefusals.failed],
  ])('grant refused: %s', (error, words) => {
    strip(fake({ phase: 'stopped', companion: true, companionRefusal: error, why: 'the client words' }).companion);
    expect(screen.getByText(words)).toBeTruthy();
    expect(screen.queryByText('the client words')).toBeNull();
  });

  it('voice_share_full: six are already sharing', () => {
    strip(fake({ share: idleShare({ ended: 'full' }) }).companion);
    expect(screen.getByText('Six people are already sharing in this room.')).toBeTruthy();
    expect(shareFullWords).toContain('Six');
  });

  it('a refused pick (NotAllowedError: main refused) says the screen could not be captured', () => {
    strip(fake({ share: idleShare({ ended: 'cancelled', cause: 'denied' }) }).companion);
    expect(screen.getAllByText('The screen could not be captured').length).toBeGreaterThan(0);
    expect(shareCauseWords.denied).toBe('The screen could not be captured');
  });

  it('a cancelled picker says nothing', () => {
    const { container } = strip(fake({ share: idleShare({ ended: 'cancelled' }) }).companion);
    expect(container.textContent).not.toMatch(/picker|cancel|closed|denied|portal/i);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Share screen' }).disabled).toBe(false);
  });

  it('a share the person stopped says nothing', () => {
    const { container } = strip(fake({ share: idleShare({ ended: 'self' }) }).companion);
    expect(container.textContent).not.toMatch(/stopped/i);
  });

  it('a closed source says why', () => {
    strip(fake({ share: idleShare({ ended: 'browser' }) }).companion);
    expect(screen.getByText(shareSourceClosedWords)).toBeTruthy();
  });

  it('a share ended by a room renewal ends with words and is offered again, not resumed (Q-D4-2)', () => {
    const t = fake({ phase: 'stopped', companion: true, why: 'The voice room was renewed, so your screen ended. Share again to carry on.', share: idleShare({ ended: 'rotated' }) });
    strip(t.companion);
    expect(screen.getByText('The voice room was renewed, so your screen ended. Share again to carry on.')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Share screen' }));
    expect(t.calls).toEqual(['share general']);
  });

  it('a failed share says so', () => {
    strip(fake({ share: idleShare({ ended: 'failed' }) }).companion);
    expect(screen.getByText(shareFailedWords)).toBeTruthy();
    expect(shareFailedWords).toMatch(/not be permitted/);
  });
});

describe('the quality choices (Q-D4-3, #1772)', () => {
  it('are 720p30 (the default), 1080p30, 1080p60, Source 30 and Source 60, and never Auto', () => {
    const t = fake();
    strip(t.companion);
    const select = screen.getByLabelText<HTMLSelectElement>('Share quality');
    expect(Array.from(select.options).map((o) => o.value)).toEqual(['720p30', '1080p30', '1080p60', 'source30', 'source60']);
    expect(Array.from(select.options).map((o) => o.textContent)).not.toContain('Auto');
    expect(select.value).toBe('720p30');
    expect(companionDefaultQuality).toEqual({ resolution: '720p', frameRate: 30 });
  });

  it.each([
    { value: '720p30', label: '720p, 30 fps' },
    { value: '1080p30', label: '1080p, 30 fps' },
    { value: '1080p60', label: '1080p, 60 fps' },
    { value: 'source30', label: 'Source (up to 4K), 30 fps' },
    { value: 'source60', label: 'Source (up to 4K), 60 fps' },
  ])('names $value and gives the browser’s estimate beside it', ({ value, label }) => {
    strip(fake().companion);
    const option = Array.from(screen.getByLabelText<HTMLSelectElement>('Share quality').options).find((o) => o.value === value);
    const quality = companionQualities.find((q) => `${q.resolution}${String(q.frameRate)}` === value);
    expect(quality).toBeDefined();
    expect(option?.textContent).toBe(`${label} · ${estimateWords(quality ?? companionDefaultQuality)}`);
  });

  it.each(['1080p30', '1080p60', 'source30', 'source60'])('chooses %s', (value) => {
    const t = fake();
    strip(t.companion);
    fireEvent.change(screen.getByLabelText('Share quality'), { target: { value } });
    expect(t.calls).toEqual([`quality ${value}`]);
  });

  it('shows the 4K60 caution for Source at 60 fps and for no other choice', () => {
    for (const q of companionQualities) {
      const view = strip(fake({ shareQuality: q }).companion);
      const caution = screen.queryByRole('note');
      if (q.resolution === 'source' && q.frameRate === 60) {
        expect(caution?.textContent).toBe(sourceSixtyNote);
        expect(screen.getByLabelText('Share quality').getAttribute('aria-describedby')).toBe(caution?.id);
      } else {
        expect(caution).toBeNull();
      }
      view.unmount();
    }
  });

  it.each([
    { what: 'one that is not offered', quality: { resolution: '720p', frameRate: 60 } },
    { what: 'a stored Auto (issue #1659)', quality: { resolution: 'auto', frameRate: 30 } },
  ] as const)('shows $what as the default, and never offers Auto', ({ quality }) => {
    strip(fake({ shareQuality: quality }).companion);
    const select = screen.getByLabelText<HTMLSelectElement>('Share quality');
    expect(select.value).toBe('720p30');
    expect(Array.from(select.options).map((o) => o.textContent)).not.toContain('Auto');
    expect(screen.queryByRole('menu', { hidden: true })).toBeNull();
  });

  it('is not offered while sharing', () => {
    strip(fake({ share: { kind: 'sharing', audio: false } }).companion);
    expect(screen.queryByLabelText('Share quality')).toBeNull();
  });
});

// The sharer's Sending line (#1772): the browser's ShareReadout, fed from the
// companion's own publication stats (readShareStats), asked next to sending.
describe('the sharer’s readout', () => {
  const sample: ShareSample = { width: 1920, height: 1080, fps: 60, bytes: 1000, timestamp: 1000 };

  it.each(companionQualities.map((q) => ({ q })))('says what was asked of $q.resolution at $q.frameRate fps next to what is sent', async ({ q }) => {
    const t = fake({ shareQuality: q, phase: 'connected', channel: 'general', companion: true, share: { kind: 'sharing', audio: false } }, { ...sample, fps: q.frameRate });
    strip(t.companion);
    const readout = await screen.findByRole('status', { name: 'Share quality' });
    expect(readout.textContent).toContain(`${askedText(q as SettledQuality)}. `);
    expect(readout.textContent).toContain('Sending: ');
    expect(readout.textContent).toContain(`${String(q.frameRate)} fps`);
    expect(t.reads).toContainEqual(['sent', undefined]);
  });

  it('reads the companion’s own stats, not a watched stream’s', async () => {
    const t = fake({ phase: 'connected', channel: 'general', companion: true, share: { kind: 'sharing', audio: false } }, sample);
    strip(t.companion);
    await screen.findByRole('status', { name: 'Share quality' });
    expect(t.reads.every(([direction]) => direction === 'sent')).toBe(true);
  });

  it.each([
    { kind: 'idle' as const },
    { kind: 'picking' as const },
    { kind: 'publishing' as const },
  ])('is not shown while the share is $kind', ({ kind }) => {
    const t = fake({ share: kind === 'idle' ? idleShare() : { kind } }, sample);
    strip(t.companion);
    expect(screen.queryByRole('status', { name: 'Share quality' })).toBeNull();
  });
});

describe('the others’ streams', () => {
  const people: ReadonlyMap<number, Person> = new Map([[2, { name: 'Ada' } as Person]]);
  function presenceWith(...sharers: readonly number[]): VoicePresenceV2 {
    return {
      schema: 'conch.voice_presence.v2',
      channel_id: 1,
      configured: true,
      available: true,
      rooms: [
        {
          participants: [1, ...sharers].map((id) => ({
            principal_id: id,
            can_publish: true,
            transmitting: false,
            joined_at: '2026-01-01T00:00:00Z',
            ...(id === 1 && !sharers.includes(1) ? {} : { sharing: true, share_started_at: '2026-01-01T00:00:01Z', share_quality: { resolution: '720p', frame_rate: 30 } }),
          })),
        },
      ],
    };
  }
  const tiles = (c: Companion, p: VoicePresenceV2 | null, over: { joined?: string | null; live?: boolean } = {}) =>
    render(<NativeStreams companion={c} presence={p} viewer={1} people={people} joined={over.joined === undefined ? 'general' : over.joined} live={over.live ?? true} />);

  it('offers Watch on a tile for each of the others, which starts the companion', () => {
    const t = fake();
    tiles(t.companion, presenceWith(2));
    const tile = screen.getByRole('region', { name: "Ada's stream" });
    expect(within(tile).getByText(/Ada · LIVE · 720p · 30 fps/)).toBeTruthy();
    fireEvent.click(within(tile).getByRole('button', { name: 'Watch' }));
    expect(t.calls).toEqual(['watch general 2']);
  });

  it('draws no tile for the person’s own stream, nor out of voice', () => {
    const own = tiles(fake().companion, presenceWith(1));
    expect(own.container.textContent).toBe('');
    own.unmount();
    expect(tiles(fake().companion, presenceWith(2), { joined: null }).container.textContent).toBe('');
  });

  it('Watch is off while the helper is not connected', () => {
    tiles(fake().companion, presenceWith(2), { live: false });
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Watch' }).disabled).toBe(true);
  });

  it('a watched stream has focus, volume, full screen and Stop watching, and stopping keeps voice', () => {
    const t = fake({ phase: 'connected', companion: true, watched: new Set([2]) });
    tiles(t.companion, presenceWith(2));
    expect(screen.getByRole('region', { name: 'Shared screen' })).toBeTruthy();
    expect(screen.getByLabelText('Share volume')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Full screen' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: "Focus Ada's screen" }));
    fireEvent.click(screen.getByRole('button', { name: 'Stop watching' }));
    expect(t.calls).toEqual(['focus 2', 'watchStream 2 false']);
  });

  // Issue #1512 (voice-multi-share.md §7.4): the companion is the one playback
  // owner of every watched stream's sound, so each watched tile has the sound
  // controls of its own, and one tile's mute leaves the other's alone.
  it('several watched streams each have their own mute and volume, and the unwatched one only Watch', () => {
    const many: ReadonlyMap<number, Person> = new Map([[2, { name: 'Ada' } as Person], [3, { name: 'Bo' } as Person], [4, { name: 'Cy' } as Person]]);
    const t = fake({ phase: 'connected', companion: true, watched: new Set([2, 3]), focusedShare: 2 });
    render(<NativeStreams companion={t.companion} presence={presenceWith(2, 3, 4)} viewer={1} people={many} joined="general" live />);
    expect(screen.getAllByRole('region', { name: 'Shared screen' })).toHaveLength(2);
    expect(screen.getAllByLabelText('Share volume')).toHaveLength(2);
    const mutes = screen.getAllByRole<HTMLButtonElement>('button', { name: 'Mute share' });
    expect(mutes).toHaveLength(2);
    fireEvent.click(mutes[0] as HTMLButtonElement);
    expect(screen.getAllByRole('button', { name: 'Unmute share' })).toHaveLength(1);
    expect(screen.getAllByRole('button', { name: 'Mute share' })).toHaveLength(1);
    const cy = screen.getByRole('region', { name: "Cy's stream" });
    expect(within(cy).getByRole('button', { name: 'Watch' })).toBeTruthy();
    expect(within(cy).queryByLabelText('Share volume')).toBeNull();
  });

  it('focus on the focused stream lets it go', () => {
    const t = fake({ phase: 'connected', companion: true, watched: new Set([2]), focusedShare: 2 });
    tiles(t.companion, presenceWith(2));
    fireEvent.click(screen.getByRole('button', { name: "Focus Ada's screen" }));
    expect(t.calls).toEqual(['focus null']);
  });
});

describe('no raw HTML or inline style', () => {
  it('renders the notes as text', () => {
    const spy = vi.spyOn(console, 'error');
    const { container } = strip(fake({ share: idleShare({ ended: 'failed' }) }).companion);
    expect(container.querySelector('[style]')).toBeNull();
    expect(spy).not.toHaveBeenCalled();
  });
});
