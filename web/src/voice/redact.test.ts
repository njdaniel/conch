import { describe, expect, it } from 'vitest';
import { LogRing, Redactor, placeholder } from './redact';

const jwt = 'eyJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJ4In0.c2lnbmF0dXJl';
const owner = {};

describe('the log hook', () => {
  it('replaces the token and the room name in use, and anything shaped like a signed token', () => {
    const r = new Redactor();
    r.hold(owner, 'join-token-value', 'conch-room-abc123');
    const out = r.text(`joining conch-room-abc123 with join-token-value and ${jwt} ok`);
    expect(out).toBe(`joining ${placeholder} with ${placeholder} and ${placeholder} ok`);
  });

  it('reaches into the context object', () => {
    const r = new Redactor();
    r.hold(owner, 'conch-room-abc123', 'xxxx');
    expect(r.any({ room: 'conch-room-abc123', nested: [{ url: `wss://h/?access_token=${jwt}` }], n: 3 })).toEqual({
      room: placeholder,
      nested: [{ url: `wss://h/?access_token=${placeholder}` }],
      n: 3,
    });
  });

  it('keeps a bounded ring that holds neither', () => {
    const r = new Redactor();
    r.hold(owner, 'conch-room-abc123', 'tok-secret-1');
    const ring = new LogRing(r, 3);
    for (let i = 0; i < 5; i++) ring.add('warn', `room conch-room-abc123 try ${String(i)}`, { token: 'tok-secret-1' });
    const lines = ring.recent();
    expect(lines).toHaveLength(3);
    expect(lines.join('\n')).not.toMatch(/conch-room-abc123|tok-secret-1/);
    expect(lines[2]).toContain('try 4');
  });

  it('does not register a short value, which would mangle text', () => {
    const r = new Redactor();
    r.hold(owner, 'a', 'ab');
    expect(r.text('a bad cab')).toBe('a bad cab');
  });

  it('adds to what is held: a second hold does not unhide the first', () => {
    const r = new Redactor();
    r.hold(owner, 'first-token-value');
    r.hold(owner, 'second-room-name');
    expect(r.text('first-token-value second-room-name')).toBe(`${placeholder} ${placeholder}`);
  });

  it('a late release by an old connection keeps a new connection hidden', () => {
    const r = new Redactor();
    const old = {};
    const next = {};
    r.hold(old, 'old-token-value', 'shared-room-name');
    r.hold(next, 'new-token-value', 'shared-room-name');
    // The old adapter's close() finishes after the new one has connected.
    r.release(old);
    expect(r.text('old-token-value new-token-value shared-room-name')).toBe(`old-token-value ${placeholder} ${placeholder}`);
    r.release(next);
    expect(r.text('new-token-value')).toBe('new-token-value');
  });

  it('hides a value that contains another one whole', () => {
    const r = new Redactor();
    r.hold(owner, 'room', 'room-and-more');
    expect(r.text('x room-and-more y')).toBe(`x ${placeholder} y`);
  });
});
