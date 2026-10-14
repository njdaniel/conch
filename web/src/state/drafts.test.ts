import { describe, expect, it } from 'vitest';
import { draftFor, draftsReducer, initialDrafts } from './drafts';

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
