import { describe, expect, it } from 'vitest';
import type { VoicePresenceV2 } from '../schema/voice';
import { parsePresence, personOf, principalOf, publisherOf, qualityLabel, sharers } from './presence';

const presence: VoicePresenceV2 = {
  schema: 'conch.voice_presence.v2',
  channel_id: 1,
  configured: true,
  available: true,
  rooms: [{
    participants: [
      { principal_id: 9, can_publish: true, transmitting: false, joined_at: '2026-11-07T10:00:00Z', sharing: true, share_started_at: '2026-11-07T10:02:00Z' },
      { principal_id: 8, can_publish: true, transmitting: false, joined_at: '2026-11-07T10:00:00Z', sharing: true, sharing_audio: true, share_started_at: '2026-11-07T10:01:00Z', share_quality: { resolution: '1080p', frame_rate: 60 } },
      { principal_id: 10, can_publish: true, transmitting: false, joined_at: '2026-11-07T10:00:00Z', sharing: true, share_started_at: '2026-11-07T10:03:00Z', share_quality: { resolution: 'source', frame_rate: 30 } },
      { principal_id: 7, can_publish: true, transmitting: false, joined_at: '2026-11-07T10:00:00Z' },
    ],
  }],
};

describe('sharers', () => {
  it('orders every live stream by share_started_at, newest last, with its quality', () => {
    expect(sharers(presence).map((s) => s.principal)).toEqual([8, 9, 10]);
    expect(sharers(presence).map((s) => qualityLabel(s.quality))).toEqual(['1080p · 60 fps', null, 'Source · 30 fps']);
  });

  it('parses v2 and refuses a v1 frame', () => {
    expect(parsePresence(JSON.stringify(presence))).toEqual(presence);
    expect(parsePresence(JSON.stringify({ ...presence, schema: 'conch.voice_presence.v1' }))).toBeNull();
  });
});

describe('identities (voice-control-plane.md §13)', () => {
  it.each([
    // identity, principalOf, publisherOf, personOf
    ['p8', 8, 8, 8],
    ['p8.screen', null, 8, 8],
    ['p8.view', null, null, 8],
    ['p8.Screen', null, null, null],
    ['p8.', null, null, null],
    ['p8.screen.screen', null, null, null],
    ['screen.p8', null, null, null],
    ['8', null, null, null],
  ])('%s: speaks as %s, streams as %s, is the person %s', (identity, speaker, publisher, person) => {
    expect(principalOf(identity)).toBe(speaker);
    expect(publisherOf(identity)).toBe(publisher);
    expect(personOf(identity)).toBe(person);
  });
});
