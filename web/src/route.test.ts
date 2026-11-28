import { describe, expect, it } from 'vitest';
import { channelFromHash, dmFromHash, hashForChannel, hashForDm, hashForMessage, messageFromHash } from './route';

describe('message routes', () => {
  for (const channel of ['general', 'a/b', '% café 日本語']) {
    it(`round-trips ${channel} and its old channel link`, () => {
      expect(channelFromHash(hashForChannel(channel))).toBe(channel);
      expect(messageFromHash(hashForChannel(channel))).toBeNull();
      expect(messageFromHash(hashForMessage(channel, 42))).toEqual({ channel, id: 42 });
      expect(channelFromHash(hashForMessage(channel, 42))).toBe(channel);
    });
  }
  for (const id of ['', '1/extra', 'abc', '0', '-1', '1.2', '9007199254740992', '999999999999999999999999']) {
    it(`rejects id ${id} without treating it as a channel name`, () => {
      expect(messageFromHash(`#/c/general/m/${id}`)).toEqual({ channel: 'general', id: null });
      expect(channelFromHash(`#/c/general/m/${id}`)).toBe('general');
      expect(() => hashForMessage('general', Number(id))).toThrow(RangeError);
    });
  }
  it('rejects malformed escapes and unrelated routes', () => {
    expect(messageFromHash('#/c/%zz/m/42')).toBeNull();
    expect(channelFromHash('#/c/%zz')).toBeNull();
    expect(messageFromHash('#/signin/code')).toBeNull();
  });
});

describe('direct message routes', () => {
  it('round-trips a conversation id and is never a channel or a message link', () => {
    expect(dmFromHash(hashForDm(42))).toBe(42);
    expect(channelFromHash(hashForDm(42))).toBeNull();
    expect(messageFromHash(hashForDm(42))).toBeNull();
    // The same number as a channel message names nothing in a DM route.
    expect(dmFromHash(hashForMessage('general', 42))).toBeNull();
  });
  for (const id of ['', '0', '-1', 'abc', '1.5', '1/m/2', '9007199254740992']) {
    it(`rejects id ${id}`, () => {
      expect(dmFromHash(`#/dm/${id}`)).toBeNull();
    });
  }
  it('refuses to make a link for an id that is not a positive integer', () => {
    expect(() => hashForDm(0)).toThrow(RangeError);
    expect(() => hashForDm(1.5)).toThrow(RangeError);
  });
});
