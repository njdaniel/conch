// The real SDK, not a stub: quieting its loggers writes nothing the browser
// keeps. loglevel reads a remembered level from localStorage and cookies when
// a logger is created, and writes one there when a level is set with persist
// on (the SDK's setLogLevel does that); the client never does. jsdom, no network.
import { describe, expect, it } from 'vitest';
import { quietSdk } from './sdk';

function remembered(): string[] {
  const found: string[] = [];
  for (let i = 0; i < localStorage.length; i++) {
    const key = localStorage.key(i);
    if (key !== null && /loglevel/i.test(key)) found.push(`localStorage ${key}`);
  }
  if (/loglevel/i.test(document.cookie)) found.push(`cookie ${document.cookie}`);
  return found;
}

describe('quieting the SDK', () => {
  it('leaves no loglevel key in localStorage or a cookie', () => {
    // The module quieted the SDK when it loaded; run it again to be sure a
    // second call writes nothing either.
    quietSdk();
    expect(remembered()).toEqual([]);
  });

  it('would be caught: a persisted level shows in the check', async () => {
    const { setLogLevel, LogLevel } = await import('livekit-client');
    setLogLevel(LogLevel.warn);
    try {
      expect(remembered().length).toBeGreaterThan(0);
    } finally {
      localStorage.clear();
      document.cookie.split(';').forEach((c) => {
        const name = c.split('=')[0]?.trim() ?? '';
        if (name !== '') document.cookie = `${name}=; expires=Thu, 01 Jan 1970 00:00:00 GMT`;
      });
    }
    expect(remembered()).toEqual([]);
  });
});
