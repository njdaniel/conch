/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { Diagnostics, scrub } from '../src/logic/diagnostics';

const bearer = 'conch_live_9f8e7d6c5b4a39281716';
const address = 'conch.example.net';
const device = '3f9a2b7c1d4e5f60';
const jwt = 'eyJhbGciOiJIUzI1NiJ9.eyJyb29tIjoiZ2VuZXJhbCJ9.c2lnbmF0dXJl';

const lines: readonly { name: string; text: string; leaks: readonly string[] }[] = [
  { name: 'bearer header', text: `fetch failed: Authorization: Bearer ${bearer}`, leaks: [bearer] },
  { name: 'server url', text: `GET https://${address}/v1/whoami 401`, leaks: [address] },
  { name: 'ticket in a socket url', text: 'connecting wss://10.0.2.2:7777/v1/voice/ws?ticket=abcDEF123456', leaks: ['abcDEF123456', '10.0.2.2'] },
  { name: 'bare ipv4 and port', text: 'connect 192.168.1.20:7880 refused', leaks: ['192.168.1.20'] },
  { name: 'ipv6', text: 'peer 2001:db8:85a3::8a2e:370:7334 gone', leaks: ['2001:db8:85a3'] },
  { name: 'compressed ipv6', text: 'peer 2001:db8::1 gone', leaks: ['2001:db8'] },
  { name: 'compressed ipv6, short', text: 'route 2600:1700::abcd up', leaks: ['2600:1700', 'abcd'] },
  { name: 'link-local ipv6', text: 'iface fe80::1c2a:3b4c:5d6e:7f80 up', leaks: ['fe80', '1c2a'] },
  { name: 'bracketed ipv6 with a port', text: 'connect [2001:db8::1]:7880', leaks: ['2001:db8'] },
  { name: 'livekit host and port', text: 'connecting livekit.example.me:7880', leaks: ['livekit.example.me'] },
  { name: 'a host under co.uk', text: 'GET conch.example.co.uk failed', leaks: ['conch.example.co.uk'] },
  { name: 'android id', text: `device ${device} started`, leaks: [device] },
  { name: 'uuid', text: 'install 123e4567-e89b-12d3-a456-426614174000', leaks: ['123e4567'] },
  { name: 'mac', text: 'bt 00:1A:2B:3C:4D:5E', leaks: ['00:1A:2B'] },
  { name: 'signed join token', text: `token ${jwt}`, leaks: [jwt] },
];

for (const c of lines) {
  test(`diagnostics export holds no ${c.name}`, () => {
    const d = new Diagnostics();
    d.hold(bearer, address, device);
    d.add('warn', c.text, { detail: c.text });
    const out = d.export();
    for (const leak of c.leaks) assert.ok(!out.includes(leak), `${leak} leaked: ${out}`);
    assert.match(out, /\[redacted\]/);
  });
}

test('registered secrets are hidden in whatever shape they appear', () => {
  const d = new Diagnostics();
  d.hold(bearer, address, device);
  d.add('info', `odd:${bearer}:${address}:${device}`);
  const out = d.export();
  for (const s of [bearer, address, device]) assert.ok(!out.includes(s));
});

test('ordinary text survives', () => {
  assert.equal(scrub('gate open at 1200 ms'), 'gate open at 1200 ms');
});

test('the ring is bounded', () => {
  const d = new Diagnostics(3);
  for (let i = 0; i < 10; i++) d.add('info', `line ${i}`);
  assert.equal(d.export().split('\n').length, 3);
});

test('a held bare host and host:port are hidden, as a LiveKit grant registers them', () => {
  const d = new Diagnostics();
  d.hold('mybox:7880', 'mybox');
  d.add('warn', 'room connect to mybox:7880 failed; mybox unreachable');
  const out = d.export();
  assert.ok(!out.includes('mybox'), out);
});

test('scrub leaves ordinary log lines readable', () => {
  assert.equal(scrub('lifecycle: app active'), 'lifecycle: app active');
  assert.equal(scrub('http: voicePresence: 200'), 'http: voicePresence: 200');
});
