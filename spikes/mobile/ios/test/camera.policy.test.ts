// Copied from spikes/mobile/android at 9515c1f5 (#1392, M4-D, with its review fixes) for the iOS spike (#1396, M5-D).
/// <reference types="node" />
import type { VoiceParticipant } from '../src/logic/schema/generated';
import test from 'node:test';
import assert from 'node:assert/strict';
import { claimedIn, noCameras, principalOf, wantTrack, type CameraWants } from '../src/app/camera/policy';

const open: CameraWants = { viewing: true, shown: true, claimed: new Set([3]) };

test('principalOf reads p<id> and nothing else', () => {
  assert.equal(principalOf('p3'), 3);
  for (const bad of ['', 'p', 'p3.view', 'p3.screen', 'x3', 'p-1', 'p3 ', 'p99999999999999999999']) assert.equal(principalOf(bad), null, bad);
});

test('wantTrack: a camera only from a claim holder, shown, in the Voice view; microphones always', () => {
  assert.equal(wantTrack('camera', 'video', 'p3', open), true);
  assert.equal(wantTrack('camera', 'video', 'p4', open), false, 'no claim');
  assert.equal(wantTrack('camera', 'video', 'p3.view', open), false, 'a companion identity is never shown');
  assert.equal(wantTrack('camera', 'video', 'p3', { ...open, viewing: false }), false);
  assert.equal(wantTrack('camera', 'video', 'p3', { ...open, shown: false }), false);
  assert.equal(wantTrack('camera', 'video', 'p3', noCameras), false);
  assert.equal(wantTrack('camera', 'audio', 'p3', open), false, 'a sound published as a camera');
  assert.equal(wantTrack('microphone', 'video', 'p3', open), false, 'a picture published as a microphone');
  assert.equal(wantTrack('microphone', 'audio', 'p4', noCameras), true);
  assert.equal(wantTrack('screen_share', 'video', 'p3', open), false);
});

test('claimedIn lists the principals whose presence shows a camera', () => {
  const person = (principal_id: number, camera?: boolean): VoiceParticipant => ({
    principal_id,
    can_publish: true,
    transmitting: false,
    joined_at: '2026-11-05T10:00:00Z',
    ...(camera === undefined ? {} : { camera }),
  });
  assert.deepEqual([...claimedIn([person(1, true), person(2), person(3, false)])], [1]);
});
