import { describe, expect, it } from 'vitest';
import { LogRing, Redactor, placeholder } from './redact';

const jwt = 'eyJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJ4In0.c2lnbmF0dXJl';

describe('the log hook', () => {
  it('replaces the token and the room name in use, and anything shaped like a signed token', () => {
    const r = new Redactor();
    r.hold('join-token-value', 'conch-room-abc123');
    const out = r.text(`joining conch-room-abc123 with join-token-value and ${jwt} ok`);
    expect(out).toBe(`joining ${placeholder} with ${placeholder} and ${placeholder} ok`);
  });

  it('reaches into the context object', () => {
    const r = new Redactor();
    r.hold('conch-room-abc123', 'xxxx');
    expect(r.any({ room: 'conch-room-abc123', nested: [{ url: `wss://h/?access_token=${jwt}` }], n: 3 })).toEqual({
      room: placeholder,
      nested: [{ url: `wss://h/?access_token=${placeholder}` }],
      n: 3,
    });
  });

  it('keeps a bounded ring that holds neither', () => {
    const r = new Redactor();
    r.hold('conch-room-abc123', 'tok-secret-1');
    const ring = new LogRing(r, 3);
    for (let i = 0; i < 5; i++) ring.add('warn', `room conch-room-abc123 try ${String(i)}`, { token: 'tok-secret-1' });
    const lines = ring.recent();
    expect(lines).toHaveLength(3);
    expect(lines.join('\n')).not.toMatch(/conch-room-abc123|tok-secret-1/);
    expect(lines[2]).toContain('try 4');
  });

  it('does not register a short value, which would mangle text', () => {
    const r = new Redactor();
    r.hold('a', 'ab');
    expect(r.text('a bad cab')).toBe('a bad cab');
  });
});
