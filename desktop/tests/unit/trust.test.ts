// The private-CA procedure of desktop/README.md, exercised with the mechanism
// the chosen stack supports (Q6): Node's fetch trusts the system store once
// main has added it. A child process stands for a fresh app start, because the
// root set is process-wide.
import { execFile } from 'node:child_process';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';

import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { isCertificateFailure } from '../../main/api/trust.ts';
import { makePki, sendJson, startHttps, who, type Fake, type Pki } from './support.ts';

const child = resolve(import.meta.dirname, 'trust-child.ts');

describe('trusting a private CA', () => {
  let pki: Pki;
  let https: Fake;
  beforeAll(async () => {
    pki = makePki();
    https = await startHttps(pki, (_r, res) => {
      sendJson(res, 200, who);
    });
  });
  afterAll(async () => {
    await https.close();
    pki.cleanup();
  });

  // Asynchronous: the server lives in this process and must keep answering.
  const run = (env: Record<string, string>) =>
    new Promise<{ stdout: string }>((done) => {
      execFile(process.execPath, [child, https.origin], { env: { PATH: process.env['PATH'] ?? '', ...env }, encoding: 'utf8', timeout: 30_000 }, (_error, stdout) => {
        done({ stdout });
      });
    });

  it('does not trust the test CA by default: a certificate failure, not a network error', async () => {
    const result = await run({});
    expect(result.stdout.trim()).toBe('certificate');
  });

  it('trusts it once it is in the system store main loads (SSL_CERT_FILE stands for the installed CA)', async () => {
    const result = await run({ SSL_CERT_FILE: pki.caPath });
    expect(result.stdout.trim()).toBe('ok 7');
  });

  it('trusts it through NODE_EXTRA_CA_CERTS, which Node reads at start', async () => {
    const result = await run({ NODE_EXTRA_CA_CERTS: pki.caPath, CONCH_SKIP_SYSTEM_ROOTS: '1' });
    expect(result.stdout.trim()).toBe('ok 7');
  });

  it('classifies certificate errors and nothing else', () => {
    expect(isCertificateFailure(new TypeError('fetch failed', { cause: Object.assign(new Error('x'), { code: 'CERT_HAS_EXPIRED' }) }))).toBe(true);
    expect(isCertificateFailure(new TypeError('fetch failed', { cause: Object.assign(new Error('x'), { code: 'ECONNREFUSED' }) }))).toBe(false);
    expect(isCertificateFailure('x')).toBe(false);
    expect(isCertificateFailure(null)).toBe(false);
  });
});

// No code turns certificate checking off (linux-desktop.md §3). The patterns
// are assembled here so this file does not name them whole.
describe('no TLS override anywhere in the shell', () => {
  const banned = [
    ['set', 'CertificateVerifyProc'],
    ['NODE_TLS_REJECT', '_UNAUTHORIZED'],
    ['reject', 'Unauthorized'],
    ['certificate', '-error'],
    ['ignore', 'CertificateErrors'],
    ['allow-insecure', '-localhost'],
    ['ignore-certificate', '-errors'],
    ['setDefaultCA', 'Certificates\\(\\[\\]'],
  ].map((parts) => new RegExp(parts.join(''), 'u'));

  function sources(dir: string): string[] {
    return readdirSync(dir).flatMap((name) => {
      const path = join(dir, name);
      return statSync(path).isDirectory() ? sources(path) : /\.(ts|tsx|cjs|js|json)$/u.test(name) ? [path] : [];
    });
  }

  it('finds the source it is checking', () => {
    const files = [...sources(resolve(import.meta.dirname, '../../main')), ...sources(resolve(import.meta.dirname, '../../preload')), ...sources(resolve(import.meta.dirname, '../../../web/src/platform/desktop'))];
    expect(files.length).toBeGreaterThan(10);
    for (const file of files) {
      const text = readFileSync(file, 'utf8');
      for (const pattern of banned) expect(pattern.test(text), `${file}: ${pattern.source}`).toBe(false);
    }
  });
});
