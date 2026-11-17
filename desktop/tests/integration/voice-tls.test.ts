// The real helper's SDK must speak wss://, as deployed servers do (#1864).
// Plain ws:// tests cannot catch a build without TLS. A disposable CA is
// trusted only by the test child; nothing enters the system trust store.
// Requires the real helper and openssl, but no microphone or real account.
import { execFileSync } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';
import { createServer as createHTTPServer, type Server as HTTPServer } from 'node:http';
import { createServer as createHTTPSServer, type Server as HTTPSServer } from 'node:https';
import type { AddressInfo, Socket } from 'node:net';
import { join, resolve } from 'node:path';

import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';

import type { VoiceSessionResponseV1 } from '../../../web/src/schema/generated.ts';
import { HELPER_NAME, defaultSpawn } from '../../main/voice/spawn.ts';
import { Supervisor } from '../../main/voice/supervisor.ts';
import { FAKE_TOKEN, readFixture, scratch, until, type Scratch } from '../unit/voice-support.ts';

const binary = process.env['CONCH_VOICE_BIN'] ?? resolve(import.meta.dirname, '../../../bin/conch-voice');
const have = existsSync(binary);
const ci = process.env['CI'] === 'true' || process.env['CI'] === '1';
if (!have) {
  const why = `the real conch-voice is not at ${binary}: run make rust-build, or set CONCH_VOICE_BIN`;
  if (ci) throw new Error(`voice TLS integration: ${why}`);
  console.warn(`voice TLS integration: SKIPPED: ${why}`);
}

describe.runIf(have)('the real helper with secure LiveKit signaling', () => {
  let certificates: Scratch;
  let ca: string;
  let key: Buffer;
  let cert: Buffer;
  beforeAll(() => {
    certificates = scratch();
    ca = join(certificates.root, 'ca.pem');
    const caKey = join(certificates.root, 'ca.key');
    const serverKey = join(certificates.root, 'server.key');
    const csr = join(certificates.root, 'server.csr');
    const serverCert = join(certificates.root, 'server.pem');
    const extensions = join(certificates.root, 'server.ext');
    const openssl = (args: string[]): void => { execFileSync('openssl', args, { stdio: 'ignore' }); };
    openssl(['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', caKey, '-out', ca,
      '-days', '1', '-subj', '/CN=Conch disposable test CA', '-addext', 'basicConstraints=critical,CA:TRUE']);
    openssl(['req', '-new', '-newkey', 'rsa:2048', '-nodes', '-keyout', serverKey, '-out', csr,
      '-subj', '/CN=localhost']);
    writeFileSync(extensions, 'subjectAltName=DNS:localhost\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n');
    openssl(['x509', '-req', '-in', csr, '-CA', ca, '-CAkey', caKey, '-CAcreateserial',
      '-out', serverCert, '-days', '1', '-sha256', '-extfile', extensions]);
    key = readFileSync(serverKey);
    cert = readFileSync(serverCert);
  });
  afterAll(() => { certificates.cleanup(); });

  let s: Scratch;
  let sup: Supervisor | null;
  let api: HTTPServer | null;
  let secure: HTTPSServer | null;
  let sockets: Set<Socket>;
  let upgrades: string[];
  let sessionRequests: number;
  let serverOrigin: string;
  let tlsOrigin: string;
  const whoami = readFixture<{ id: number }>('whoami-response-v1.json');
  const template = readFixture<VoiceSessionResponseV1>('voice-session-response-v1.json');

  beforeEach(async () => {
    s = scratch();
    sup = null;
    api = null;
    secure = null;
    sockets = new Set();
    upgrades = [];
    sessionRequests = 0;
    copyFileSync(binary, join(s.resources, HELPER_NAME));
    chmodSync(join(s.resources, HELPER_NAME), 0o755);
    secure = createHTTPSServer({ key, cert }, (req, res) => {
      req.resume();
      res.writeHead(401, { 'content-length': '0', connection: 'close' }).end();
    });
    secure.on('upgrade', (req, socket: Socket) => {
      upgrades.push(req.headers.authorization ?? '');
      socket.end('HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n');
    });
    secure.on('tlsClientError', () => undefined);
    secure.on('connection', (socket: Socket) => {
      sockets.add(socket);
      socket.on('error', () => undefined);
      socket.on('close', () => sockets.delete(socket));
    });
    await new Promise<void>((r) => secure?.listen(0, '127.0.0.1', r));
    tlsOrigin = `wss://localhost:${String((secure.address() as AddressInfo).port)}`;
    api = createHTTPServer((req, res) => {
      req.resume();
      if (req.url === '/v1/whoami') {
        res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(whoami));
      } else if (req.method === 'POST' && req.url === '/v1/channels/general/voice/session') {
        sessionRequests++;
        const session: VoiceSessionResponseV1 = {
          ...template, livekit_url: tlsOrigin,
          rooms: template.rooms.map((room) => ({ ...room, expires_at: new Date(Date.now() + 60_000).toISOString() })),
        };
        res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(session));
      } else {
        res.writeHead(503, { 'content-type': 'application/json' }).end(JSON.stringify(readFixture('error.json')));
      }
    });
    await new Promise<void>((r) => api?.listen(0, '127.0.0.1', r));
    serverOrigin = `http://127.0.0.1:${String((api.address() as AddressInfo).port)}`;
  });

  afterEach(async () => {
    try {
      await sup?.stop();
    } finally {
      for (const socket of sockets) socket.destroy();
      await Promise.all([api, secure].map((server) => new Promise<void>((r) => {
        if (server === null) r();
        else { server.closeAllConnections(); server.close(() => { r(); }); }
      })));
      s.cleanup();
    }
  });

  async function start(trusted: boolean): Promise<Supervisor> {
    const supervisor = new Supervisor({
      resources: s.resources,
      env: { HOME: s.root, XDG_RUNTIME_DIR: s.run },
      log: () => undefined,
      // Test-only CA injection after the production environment filter. The
      // production launcher and system trust store are left unchanged.
      spawn: (command, args, options) => defaultSpawn(command, args, {
        ...options, env: { ...options.env, ...(trusted ? { SSL_CERT_FILE: ca } : {}) },
      }),
    });
    sup = supervisor;
    expect((await supervisor.start({ profileId: 'tls-test', configDir: s.config, token: FAKE_TOKEN,
      server: serverOrigin, channel: 'general', principalId: whoami.id,
      extraArgs: ['--mic', 'none', '--sink', 'count'],
    })).status).toBe('running');
    return supervisor;
  }

  it('opens an authenticated secure WebSocket with a trusted certificate', async () => {
    const supervisor = await start(true);
    await until(() => upgrades.length > 0, 15_000, 'a TLS-protected WebSocket upgrade from the real SDK');
    expect(sessionRequests).toBeGreaterThan(0);
    expect(upgrades).toContain(`Bearer ${template.rooms[0]?.token ?? '?'}`);
    expect(supervisor.diagnostics().join('\n')).not.toContain('TLS support not compiled in');
  });

  it.each([
    { name: 'an untrusted certificate', trusted: false, wrongHost: false, reason: /UnknownIssuer|unknown issuer/i },
    { name: 'a certificate for the wrong hostname', trusted: true, wrongHost: true, reason: /NotValidForName|not valid for/i },
  ])('refuses $name before any WebSocket upgrade', async ({ trusted, wrongHost, reason }) => {
    if (wrongHost) tlsOrigin = tlsOrigin.replace('localhost', '127.0.0.1');
    const supervisor = await start(trusted);
    await until(() => reason.test(supervisor.diagnostics().join('\n')), 15_000, 'the SDK certificate-validation refusal');
    expect(sessionRequests).toBeGreaterThan(0);
    expect(upgrades).toEqual([]);
    expect(supervisor.diagnostics().join('\n')).not.toContain('TLS support not compiled in');
  });
});
