/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { stateAnnouncements, textToAnnounce } from '../src/app/announce';

type S = Parameters<typeof stateAnnouncements>[0];
const base: S = { phase: 'idle', listenOnly: false, gateOpen: false, mode: 'ptt' };

test('stateAnnouncements: changes only, table', () => {
  const cases: { name: string; prev: S; next: S; want: string[] }[] = [
    { name: 'nothing changed', prev: base, next: { ...base }, want: [] },
    { name: 'joining', prev: base, next: { ...base, phase: 'joining' }, want: ['Joining'] },
    { name: 'checking is silent', prev: base, next: { ...base, phase: 'checking' }, want: [] },
    { name: 'connected', prev: { ...base, phase: 'joining' }, next: { ...base, phase: 'connected' }, want: ['In voice'] },
    { name: 'listen only', prev: { ...base, phase: 'connected' }, next: { ...base, phase: 'connected', listenOnly: true }, want: ['In voice, listening only'] },
    { name: 'reconnecting', prev: { ...base, phase: 'connected' }, next: { ...base, phase: 'reconnecting' }, want: ['Reconnecting'] },
    { name: 'left', prev: { ...base, phase: 'connected' }, next: base, want: ['Not in voice'] },
    { name: 'gate opens (ptt)', prev: { ...base, phase: 'connected' }, next: { ...base, phase: 'connected', gateOpen: true }, want: ['Microphone open'] },
    { name: 'gate shuts (ptt)', prev: { ...base, phase: 'connected', gateOpen: true }, next: { ...base, phase: 'connected' }, want: ['Microphone closed'] },
    { name: 'open mode gate is a Note', prev: { ...base, phase: 'connected', mode: 'open' }, next: { ...base, phase: 'connected', mode: 'open', gateOpen: true }, want: [] },
    { name: 'gate shut by leaving is not news', prev: { ...base, phase: 'connected', gateOpen: true }, next: base, want: ['Not in voice'] },
  ];
  for (const c of cases) assert.deepEqual(stateAnnouncements(c.prev, c.next), c.want, c.name);
});

test('textToAnnounce: a note that appeared or changed, never an empty or repeated one', () => {
  assert.equal(textToAnnounce(null, 'Hold again'), 'Hold again');
  assert.equal(textToAnnounce('a', 'b'), 'b');
  assert.equal(textToAnnounce('a', 'a'), null);
  assert.equal(textToAnnounce('a', null), null);
  assert.equal(textToAnnounce(null, ''), null);
});
