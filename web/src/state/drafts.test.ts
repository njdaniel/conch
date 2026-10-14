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

describe('draft restore', () => {
  it('puts text back only into an empty draft', () => {
    let state = draftsReducer(initialDrafts, { type: 'restore', channel: 'ops', text: 'refused' });
    expect(draftFor(state, 'ops')).toBe('refused');
    state = draftsReducer(state, { type: 'edit', channel: 'ops', text: 'newer' });
    expect(draftFor(draftsReducer(state, { type: 'restore', channel: 'ops', text: 'refused' }), 'ops')).toBe('newer');
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
    state = targetsReducer(state, { type: 'whispered', channel: 'ops', note: 'n' });
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
});
