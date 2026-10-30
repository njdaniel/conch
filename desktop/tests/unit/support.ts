// Test support: a local HTTP(S) server standing in for conchd, and a test CA.
// No network beyond loopback, no conchd.
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createServer as createHttp, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createServer as createHttps } from 'node:https';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { Client } from '../../main/api/client.ts';
import { Service } from '../../main/api/service.ts';
import { Session } from '../../main/api/session.ts';

export interface Seen {
  readonly method: string;
  readonly url: string;
  readonly headers: IncomingMessage['headers'];
  readonly body: string;
}

export type Handler = (request: Seen, response: ServerResponse) => void;

export interface Fake {
  readonly origin: string;
  readonly port: number;
  readonly seen: Seen[];
  close(): Promise<void>;
}

function listen(server: Server, scheme: 'http' | 'https', host: string, seen: Seen[]): Promise<Fake> {
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address() as AddressInfo;
      resolve({
        origin: `${scheme}://${host}:${String(port)}`,
        port,
        seen,
        close: () =>
          new Promise<void>((done) => {
            server.closeAllConnections();
            server.close(() => {
              done();
            });
          }),
      });
    });
  });
}

function wrap(handler: Handler, seen: Seen[]) {
  return (request: IncomingMessage, response: ServerResponse): void => {
    const chunks: Buffer[] = [];
    request.on('data', (chunk: Buffer) => chunks.push(chunk));
    request.on('end', () => {
      const entry = { method: request.method ?? '', url: request.url ?? '', headers: request.headers, body: Buffer.concat(chunks).toString('utf8') };
      seen.push(entry);
      handler(entry, response);
    });
  };
}

export function startHttp(handler: Handler): Promise<Fake> {
  const seen: Seen[] = [];
  return listen(createHttp(wrap(handler, seen)), 'http', '127.0.0.1', seen);
}

export interface Pki {
  readonly dir: string;
  readonly caPath: string;
  readonly caPem: string;
  readonly keyPem: string;
  readonly certPem: string;
  cleanup(): void;
}

// A CA and a server certificate for localhost and 127.0.0.1, made with the
// openssl on the machine (the same tool a person would use for a private CA).
export function makePki(): Pki {
  const dir = mkdtempSync(join(tmpdir(), 'conch-desktop-pki-'));
  const run = (...args: string[]): void => {
    execFileSync('openssl', args, { cwd: dir, stdio: 'ignore' });
  };
  run('req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', 'ca.key', '-out', 'ca.pem', '-days', '2', '-subj', '/CN=Conch Test CA', '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign');
  run('req', '-newkey', 'rsa:2048', '-nodes', '-keyout', 'srv.key', '-out', 'srv.csr', '-subj', '/CN=localhost');
  execFileSync('sh', ['-c', 'printf "subjectAltName=DNS:localhost,IP:127.0.0.1\\nbasicConstraints=CA:FALSE\\nkeyUsage=digitalSignature,keyEncipherment\\nextendedKeyUsage=serverAuth\\n" > ext.cnf'], { cwd: dir });
  run('x509', '-req', '-in', 'srv.csr', '-CA', 'ca.pem', '-CAkey', 'ca.key', '-CAcreateserial', '-out', 'srv.pem', '-days', '2', '-extfile', 'ext.cnf');
  const read = (name: string): string => readFileSync(join(dir, name), 'utf8');
  return {
    dir,
    caPath: join(dir, 'ca.pem'),
    caPem: read('ca.pem'),
    keyPem: read('srv.key'),
    certPem: read('srv.pem'),
    cleanup: () => {
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

export function startHttps(pki: Pki, handler: Handler): Promise<Fake> {
  const seen: Seen[] = [];
  return listen(createHttps({ key: pki.keyPem, cert: pki.certPem }, wrap(handler, seen)), 'https', 'localhost', seen);
}

export function sendJson(response: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  response.writeHead(status, { 'Content-Type': 'application/json', ...headers });
  response.end(JSON.stringify(body));
}

export const STAMP = '2026-10-29T10:00:00Z';
export const principal = { id: 7, kind: 'human', name: 'ada', created_at: STAMP } as const;
export const credential = { id: 3, principal_id: 7, label: 'login', created_at: STAMP } as const;
export const who = { id: 7, kind: 'human', name: 'ada', role: 'member' } as const;

export function stack(log: (line: string) => void = () => undefined) {
  const session = new Session();
  const client = new Client({ fetch: globalThis.fetch, session });
  const service = new Service({ client, session, now: Date.now, log });
  return { session, client, service };
}
