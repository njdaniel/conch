import { afterEach, describe, expect, it, vi } from 'vitest';
import { capped, clearStored, draftBudgetBytes, forgetChannelStored, loadStored, pruneStored, saveDrafts, saveEdit } from './draftStore';

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe('the drafts kept across a reload', () => {
  it('saves by channel id and restores for the same person', () => {
    saveDrafts(2, { 7: 'half a thou', 8: 'other' });
    expect(loadStored(2).drafts).toEqual({ '7': 'half a thou', '8': 'other' });
  });

  it('drops a draft that is emptied (sent), and everything once none is left', () => {
    saveDrafts(2, { 7: 'x', 8: 'y' });
    saveDrafts(2, { 8: 'y' });
    expect(loadStored(2).drafts).toEqual({ '8': 'y' });
    saveDrafts(2, {});
    expect(window.sessionStorage.length).toBe(0);
  });

  it('is gone at sign-out', () => {
    saveDrafts(2, { 7: 'secret thought' });
    saveEdit(2, 7, { message: 1, text: 'edit' });
    clearStored();
    expect(window.sessionStorage.length).toBe(0);
  });

  it('is given to nobody else, and removed when another person signs in', () => {
    saveDrafts(2, { 7: 'first person’s' });
    expect(loadStored(3)).toEqual({ principal: 3, drafts: {}, targets: {}, edits: {} });
    expect(window.sessionStorage.length).toBe(0);
  });

  it('is under the channel id, so a rename keeps it, and a lost channel forgets it', () => {
    saveDrafts(2, { 7: 'x', 8: 'y' });
    saveEdit(2, 7, { message: 4, text: 'e' });
    expect(loadStored(2).drafts['7']).toBe('x');
    forgetChannelStored(2, 7);
    expect(loadStored(2)).toMatchObject({ drafts: { '8': 'y' }, edits: {} });
  });

  it('keeps an open edit by message id beside the drafts', () => {
    saveEdit(2, 7, { message: 4, text: 'e' });
    saveDrafts(2, { 7: 'x' });
    expect(loadStored(2)).toMatchObject({ drafts: { '7': 'x' }, edits: { '7': { message: 4, text: 'e' } } });
    saveEdit(2, 7, null);
    expect(loadStored(2).edits).toEqual({});
  });

  it('keeps each draft to what a post request can carry beside its envelope', () => {
    const jsonBytes = (text: string) => new TextEncoder().encode(JSON.stringify(text)).length;
    // Plain text: JSON adds the two quotes.
    const exact = 'a'.repeat(draftBudgetBytes - 2);
    expect(capped(exact)).toBe(exact);
    expect(capped(`${exact}a`)).toBe(exact);
    expect(capped('short')).toBe('short');
    // Escaping grows the text: quotes cost two bytes each.
    const quotes = '"'.repeat(draftBudgetBytes);
    expect(jsonBytes(capped(quotes))).toBeLessThanOrEqual(draftBudgetBytes);
    expect(capped(quotes).length).toBeGreaterThan(draftBudgetBytes / 2 - 3);
    // Multi-byte text is measured in bytes.
    expect(jsonBytes(capped('é'.repeat(draftBudgetBytes)))).toBeLessThanOrEqual(draftBudgetBytes);
  });

  it('never cuts an emoji in two', () => {
    const emoji = '😀';
    // Room for the quotes and one byte short of the last emoji.
    const fill = 'a'.repeat(draftBudgetBytes - 2 - 4 + 1);
    const cut = capped(fill + emoji + emoji);
    expect(cut).toBe(fill);
    const fits = capped('a'.repeat(draftBudgetBytes - 2 - 4) + emoji + emoji);
    expect(fits.endsWith(emoji)).toBe(true);
    // One whole emoji fits and the second would not: it is left out, not halved.
    expect(fits).toBe('a'.repeat(draftBudgetBytes - 2 - 4) + emoji);
  });

  it('keeps an audience beside the draft, and prunes channels the person lost', () => {
    saveDrafts(2, { 7: 'x', 8: 'y' }, { 7: { net: null, whisper: [3, 4] }, 8: { net: 5, whisper: null } });
    saveEdit(2, 8, { message: 1, text: 'e' });
    expect(loadStored(2).targets).toEqual({ '7': { net: null, whisper: [3, 4] }, '8': { net: 5, whisper: null } });
    pruneStored(2, (id) => id === 7);
    expect(loadStored(2)).toEqual({ principal: 2, drafts: { '7': 'x' }, targets: { '7': { net: null, whisper: [3, 4] } }, edits: {} });
  });

  it('removes the old snapshot when a write fails, so sent text does not come back', () => {
    saveDrafts(2, { 7: 'already sent' });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('full', 'QuotaExceededError');
    });
    saveDrafts(2, { 7: 'newer text' });
    expect(window.sessionStorage.getItem('conch.web.drafts')).toBeNull();
    saveDrafts(2, {});
    expect(loadStored(2).drafts).toEqual({});
  });

  it('ignores what is stored that is not its own shape', () => {
    window.sessionStorage.setItem('conch.web.drafts', '{"principal":"x"}');
    expect(loadStored(2).drafts).toEqual({});
    window.sessionStorage.setItem('conch.web.drafts', 'not json');
    expect(loadStored(2).drafts).toEqual({});
  });

  it('works, in memory only, when storage throws', () => {
    for (const method of ['getItem', 'setItem', 'removeItem'] as const) {
      vi.spyOn(Storage.prototype, method).mockImplementation(() => {
        throw new Error('blocked');
      });
    }
    expect(() => {
      saveDrafts(2, { 7: 'x' });
      saveEdit(2, 7, { message: 1, text: 'e' });
      forgetChannelStored(2, 7);
      clearStored();
    }).not.toThrow();
    expect(loadStored(2).drafts).toEqual({});
  });
});
