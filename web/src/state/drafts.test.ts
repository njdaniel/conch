import { describe, expect, it } from 'vitest';
import { draftFor, draftsReducer, initialDrafts, initialTargets, targetFor, targetsReducer } from './drafts';

describe('drafts', () => {
  it('keeps one per channel, so switching channels loses none', () => {
    let state = draftsReducer(initialDrafts, { type: 'edit', channel: 'ops', text: 'half a thou' });
    state = draftsReducer(state, { type: 'edit', channel: 'dev', text: 'other' });
    expect(draftFor(state, 'ops')).toBe('half a thou');
    expect(draftFor(state, 'dev')).toBe('other');
    expect(draftFor(state, 'general')).toBe('');
  });
  it('forgets a draft that is emptied, and all of them at reset', () => {
    let state = draftsReducer(initialDrafts, { type: 'edit', channel: 'ops', text: 'x' });
    state = draftsReducer(state, { type: 'edit', channel: 'ops', text: '' });
    expect(state).toEqual({});
    state = draftsReducer(draftsReducer(state, { type: 'edit', channel: 'ops', text: 'x' }), { type: 'reset' });
    expect(state).toEqual({});
  });
});

describe('targets', () => {
  it('keeps a net per channel until changed', () => {
    let state = targetsReducer(initialTargets, { type: 'net', channel: 'ops', net: 5 });
    state = targetsReducer(state, { type: 'net', channel: 'dev', net: 9 });
    expect(targetFor(state, 'ops').net).toBe(5);
    expect(targetFor(state, 'dev').net).toBe(9);
    expect(targetFor(state, 'general')).toMatchObject({ net: null, whisper: null });
    expect(targetFor(targetsReducer(state, { type: 'channel', channel: 'ops' }), 'ops').net).toBeNull();
  });
  it('toggles recipients, and a whisper ends the net and itself when accepted', () => {
    let state = targetsReducer(initialTargets, { type: 'net', channel: 'ops', net: 5 });
    state = targetsReducer(state, { type: 'whisper', channel: 'ops' });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 3 });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 4 });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 3 });
    expect(targetFor(state, 'ops')).toMatchObject({ net: 5, whisper: [4] });
    state = targetsReducer(state, { type: 'posting', channel: 'ops', requestId: 'r1' });
    state = targetsReducer(state, { type: 'whispered', channel: 'ops', requestId: 'r1', note: 'n' });
    expect(targetFor(state, 'ops')).toMatchObject({ net: null, whisper: null, note: 'n' });
  });
  it('closing the picker returns to the net chosen before', () => {
    let state = targetsReducer(initialTargets, { type: 'net', channel: 'ops', net: 5 });
    state = targetsReducer(targetsReducer(state, { type: 'whisper', channel: 'ops' }), { type: 'closePicker', channel: 'ops' });
    expect(targetFor(state, 'ops')).toMatchObject({ net: 5, whisper: null });
  });
  it('clears the last refusal and note when a post starts', () => {
    let state = targetsReducer(initialTargets, { type: 'refused', channel: 'ops', line: 'no' });
    state = targetsReducer(state, { type: 'posting', channel: 'ops' });
    expect(targetFor(state, 'ops')).toMatchObject({ refusal: null, note: null });
  });
  it('leaves a second whisper being composed alone when the first is confirmed', () => {
    let state = targetsReducer(initialTargets, { type: 'whisper', channel: 'ops' });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 3 });
    // The first (to 3) is sent; another to 4 is chosen while it is on its way.
    state = targetsReducer(state, { type: 'posting', channel: 'ops', requestId: 'r1' });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 3 });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 4 });
    state = targetsReducer(state, { type: 'whispered', channel: 'ops', requestId: 'r1', note: 'n' });
    expect(targetFor(state, 'ops')).toMatchObject({ whisper: [4], note: 'n' });
  });

  // Issue #1066: the answer to a send is tied to the draft it took, never to
  // its recipients.
  it('leaves a newer draft to the same people alone when the first is confirmed', () => {
    let state = targetsReducer(initialTargets, { type: 'whisper', channel: 'ops' });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 3 });
    state = targetsReducer(state, { type: 'posting', channel: 'ops', requestId: 'r1' });
    // Something is typed to the same whisper while r1 is on its way.
    state = targetsReducer(state, { type: 'edited', channel: 'ops' });
    state = targetsReducer(state, { type: 'whispered', channel: 'ops', requestId: 'r1', note: null });
    expect(targetFor(state, 'ops')).toMatchObject({ whisper: [3] });
    // The same whisper chosen again is a newer draft too.
    state = targetsReducer(state, { type: 'posting', channel: 'ops', requestId: 'r2' });
    state = targetsReducer(state, { type: 'channel', channel: 'ops' });
    state = targetsReducer(state, { type: 'whisper', channel: 'ops' });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 3 });
    state = targetsReducer(state, { type: 'whispered', channel: 'ops', requestId: 'r2', note: null });
    expect(targetFor(state, 'ops')).toMatchObject({ whisper: [3] });
  });
  it('a Retry, or a send nobody took the composer with, never ends the whisper', () => {
    let state = targetsReducer(initialTargets, { type: 'whisper', channel: 'ops' });
    state = targetsReducer(state, { type: 'toggle', channel: 'ops', principal: 3 });
    // A Retry from a pending line takes no draft.
    state = targetsReducer(state, { type: 'posting', channel: 'ops' });
    state = targetsReducer(state, { type: 'whispered', channel: 'ops', requestId: 'r0', note: null });
    expect(targetFor(state, 'ops')).toMatchObject({ whisper: [3] });
    // A kept audience given back after a sign-in holds no send's draft.
    state = targetsReducer(state, { type: 'posting', channel: 'ops', requestId: 'r1' });
    state = targetsReducer(state, { type: 'restore', channel: 'ops', net: null, whisper: [3] });
    state = targetsReducer(state, { type: 'whispered', channel: 'ops', requestId: 'r1', note: null });
    expect(targetFor(state, 'ops')).toMatchObject({ whisper: [3], protected: true });
  });
  it('clears the refusal when the picker closes', () => {
    let state = targetsReducer(initialTargets, { type: 'whisper', channel: 'ops' });
    state = targetsReducer(state, { type: 'refused', channel: 'ops', line: 'no' });
    expect(targetFor(targetsReducer(state, { type: 'closePicker', channel: 'ops' }), 'ops').refusal).toBeNull();
  });
});

describe('a lost channel (issue #1027)', () => {
  it('takes its draft and its audience with it, and nothing else', () => {
    let drafts = draftsReducer(initialDrafts, { type: 'edit', channel: 'ops', text: 'for ops' });
    drafts = draftsReducer(drafts, { type: 'edit', channel: 'dev', text: 'for dev' });
    drafts = draftsReducer(drafts, { type: 'forget', channel: 'ops' });
    expect(draftFor(drafts, 'ops')).toBe('');
    expect(draftFor(drafts, 'dev')).toBe('for dev');
    expect(draftsReducer(drafts, { type: 'forget', channel: 'nowhere' })).toBe(drafts);

    let targets = targetsReducer(initialTargets, { type: 'net', channel: 'ops', net: 4 });
    targets = targetsReducer(targets, { type: 'whisper', channel: 'dev' });
    targets = targetsReducer(targets, { type: 'forget', channel: 'ops' });
    expect(targetFor(targets, 'ops')).toEqual(targetFor(initialTargets, 'ops'));
    expect(targetFor(targets, 'dev').whisper).toEqual([]);
  });
});
