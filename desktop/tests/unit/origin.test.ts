import { describe, expect, it } from 'vitest';

import { parseServerAddress } from '../../main/api/origin.ts';
import { targetOf } from '../../main/api/target.ts';

describe('the server address', () => {
  it.each([
    ['https://chat.example.org', 'https://chat.example.org'],
    ['https://chat.example.org/', 'https://chat.example.org'],
    ['HTTPS://Chat.Example.ORG', 'https://chat.example.org'],
    ['https://chat.example.org:8443', 'https://chat.example.org:8443'],
    ['https://chat.example.org:443', 'https://chat.example.org'],
    ['https://bücher.example', 'https://xn--bcher-kva.example'],
    ['http://localhost:8080', 'http://localhost:8080'],
    ['http://LOCALHOST', 'http://localhost'],
    ['http://127.0.0.1:9000/', 'http://127.0.0.1:9000'],
    ['http://127.8.9.10', 'http://127.8.9.10'],
    ['http://127.1', 'http://127.0.0.1'],
    ['http://[::1]:3000', 'http://[::1]:3000'],
    ['https://[2001:db8::1]', 'https://[2001:db8::1]'],
  ])('accepts %s as %s', (input, origin) => {
    expect(parseServerAddress(input)).toEqual({ ok: true, origin });
  });

  it.each([
    ['', 'malformed'],
    ['chat.example.org', 'malformed'],
    ['https://', 'malformed'],
    ['https:example.org', 'malformed'],
    ['https://chat.example.org\\@evil.example', 'malformed'],
    [' https://chat.example.org', 'malformed'],
    ['https://chat.example.org\n', 'malformed'],
    ['https://chat .example.org', 'malformed'],
    ['https://chat.example.org/\u0000', 'malformed'],
    [`https://${'a'.repeat(3000)}.example`, 'malformed'],
    ['ftp://chat.example.org', 'scheme'],
    ['file:///etc/passwd', 'scheme'],
    ['javascript:alert(1)', 'scheme'],
    ['ws://chat.example.org', 'scheme'],
    ['http://chat.example.org', 'scheme'],
    ['http://192.168.1.5', 'scheme'],
    ['http://10.0.0.1:8080', 'scheme'],
    ['http://128.0.0.1', 'scheme'],
    ['http://localhost.example.org', 'scheme'],
    ['http://[::2]', 'scheme'],
    ['https://user@chat.example.org', 'userinfo'],
    ['https://user:pw@chat.example.org', 'userinfo'],
    ['https://:pw@chat.example.org', 'userinfo'],
    ['https://@chat.example.org', 'userinfo'],
    ['https://chat.example.org/app', 'path'],
    ['https://chat.example.org/app/', 'path'],
    ['https://chat.example.org//', 'path'],
    ['https://chat.example.org/?x=1', 'path'],
    ['https://chat.example.org?x=1', 'path'],
    ['https://chat.example.org/?', 'path'],
    ['https://chat.example.org#frag', 'path'],
    ['https://chat.example.org/#', 'path'],
  ])('refuses %j for %s', (input, refusal) => {
    expect(parseServerAddress(input)).toEqual({ ok: false, refusal });
  });
});

describe('the request target', () => {
  it('is the template with encoded values, then the canonical query', () => {
    expect(targetOf('listMessages', { channel: 'general' }, 'limit=5&before=9')).toBe('/v2/channels/general/messages?limit=5&before=9');
    expect(targetOf('listMessages', { channel: 'a b#c?d' }, undefined)).toBe('/v2/channels/a%20b%23c%3Fd/messages');
    expect(targetOf('fileBytes', { id: 12 }, '')).toBe('/v2/files/12');
    expect(targetOf('whoami', undefined, undefined)).toBe('/v1/whoami');
    expect(targetOf('searchMessages', { channel: 'x' }, 'q=a%20b')).toBe('/v2/channels/x/messages/search?q=a+b');
  });

  it.each([
    ['a/b'],
    ['..'],
    ['.'],
    [''],
    ['a%2fb'],
    ['a%2Fb'],
    ['%2e%2e'],
    ['a\\b'],
    ['a\nb'],
    ['a\u0000'],
    ['x'.repeat(300)],
  ])('refuses the path value %j', (value) => {
    expect(targetOf('listMessages', { channel: value }, undefined)).toBeNull();
  });

  it('refuses missing, extra and mistyped values', () => {
    expect(targetOf('listMessages', {}, undefined)).toBeNull();
    expect(targetOf('listMessages', undefined, undefined)).toBeNull();
    expect(targetOf('listMessages', { channel: 'a', extra: 'b' }, undefined)).toBeNull();
    expect(targetOf('whoami', { channel: 'a' }, undefined)).toBeNull();
    expect(targetOf('fileBytes', { id: -1 }, undefined)).toBeNull();
    expect(targetOf('fileBytes', { id: 1.5 }, undefined)).toBeNull();
    expect(targetOf('fileBytes', { id: { n: 1 } }, undefined)).toBeNull();
  });

  it.each([['?x=1'], ['x=1#a'], ['x=1 y=2'], ['X=1'], ['a b=1'], ['x=%zz'], [`x=${'1'.repeat(3000)}`], ['a=1&b=2&c=3&d=4&e=5&f=6&g=7&h=8&i=9&j=10&k=11&l=12&m=13&n=14&o=15&p=16&q=17']])(
    'refuses the query %j',
    (query) => {
      expect(targetOf('listMessages', { channel: 'a' }, query)).toBeNull();
    },
  );
});
