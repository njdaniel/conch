import { createHash } from 'node:crypto';

import { describe, expect, it } from 'vitest';

import { CONTENT_SECURITY_POLICY } from '../../main/csp.ts';
import { handle, parseManifest, resolve, SCHEME_PRIVILEGES, type Manifest } from '../../main/scheme.ts';

const sha = (s: string): string => createHash('sha256').update(s).digest('hex');
const INDEX = '<!doctype html><div id="root"></div>';
const JS = 'console.log(1)';

function manifest(): Manifest {
  return parseManifest(
    JSON.stringify({
      version: 1,
      files: {
        '/index.html': { sha256: sha(INDEX), type: 'text/html; charset=utf-8' },
        '/assets/app-AbC_1-2.js': { sha256: sha(JS), type: 'text/javascript; charset=utf-8' },
        '/favicon-32.png': { sha256: sha('png'), type: 'image/png' },
      },
    }),
  );
}

describe('resolve', () => {
  const m = manifest();
  const served: [string, string][] = [
    ['conch-app://ui/', '/index.html'],
    ['conch-app://ui', '/index.html'],
    ['conch-app://ui/index.html', '/index.html'],
    ['conch-app://ui/assets/app-AbC_1-2.js', '/assets/app-AbC_1-2.js'],
    ['conch-app://ui/favicon-32.png', '/favicon-32.png'],
    // The client routes by fragment, and a reload hands it to the handler.
    ['conch-app://ui/#/c/general', '/index.html'],
    ['conch-app://ui/index.html#x', '/index.html'],
  ];
  it.each(served)('serves %s as %s', (url, path) => {
    const r = resolve(url, m);
    expect(r.kind).toBe('file');
    if (r.kind === 'file') expect(r.path).toBe(path);
  });

  const refused: [string, string][] = [
    ['another host', 'conch-app://other/index.html'],
    ['an upper-case host', 'conch-app://UI/index.html'],
    ['a port', 'conch-app://ui:8080/index.html'],
    ['userinfo', 'conch-app://user@ui/index.html'],
    ['userinfo with a password', 'conch-app://user:pw@ui/index.html'],
    ['a host that only starts with ui', 'conch-app://ui.evil/index.html'],
    ['a query', 'conch-app://ui/index.html?x=1'],
    ['an empty query', 'conch-app://ui/index.html?'],
    ['a query before a fragment', 'conch-app://ui/index.html?x=1#a'],
    ['a fragment on a file the manifest lacks', 'conch-app://ui/nope.js#x'],
    ['a fragment on another host', 'conch-app://other/#x'],
    ['a dot-dot segment', 'conch-app://ui/assets/../index.html'],
    ['a leading dot-dot', 'conch-app://ui/../index.html'],
    ['a dot segment', 'conch-app://ui/./index.html'],
    ['an encoded dot', 'conch-app://ui/%2e%2e/index.html'],
    ['an upper-case encoded dot', 'conch-app://ui/assets/%2E%2E/index.html'],
    ['an encoded slash', 'conch-app://ui/assets%2fapp-AbC_1-2.js'],
    ['an upper-case encoded slash', 'conch-app://ui/assets%2Fapp-AbC_1-2.js'],
    ['an encoded backslash', 'conch-app://ui/assets%5capp-AbC_1-2.js'],
    ['a backslash', 'conch-app://ui/assets\\app-AbC_1-2.js'],
    ['an encoded NUL', 'conch-app://ui/index.html%00.png'],
    ['a NUL', 'conch-app://ui/index.html\u0000'],
    ['an encoded letter', 'conch-app://ui/%69ndex.html'],
    ['an empty segment', 'conch-app://ui/assets//app-AbC_1-2.js'],
    ['a trailing slash', 'conch-app://ui/assets/'],
    ['a directory', 'conch-app://ui/assets'],
    ['a path not in the manifest', 'conch-app://ui/secret.txt'],
    ['a case that differs', 'conch-app://ui/INDEX.HTML'],
    ['another scheme', 'https://ui/index.html'],
    ['no scheme', 'ui/index.html'],
    ['a newline', 'conch-app://ui/index.html\n'],
    ['the manifest itself', 'conch-app://ui/ui-manifest.json'],
  ];
  it.each(refused)('answers missing for %s', (_name, url) => {
    expect(resolve(url, m).kind).toBe('missing');
  });
});

describe('parseManifest', () => {
  const good = { version: 1, files: { '/index.html': { sha256: sha('x'), type: 'text/html; charset=utf-8' } } };
  const bad: [string, unknown][] = [
    ['not an object', []],
    ['the wrong version', { ...good, version: 2 }],
    ['no files', { version: 1 }],
    ['no index.html', { version: 1, files: { '/a.js': { sha256: sha('x'), type: 'text/javascript' } } }],
    ['a dot-dot path', { version: 1, files: { ...good.files, '/../x': { sha256: sha('x'), type: 'text/plain' } } }],
    ['a short hash', { version: 1, files: { '/index.html': { sha256: 'abc', type: 'text/html' } } }],
    ['an upper-case hash', { version: 1, files: { '/index.html': { sha256: sha('x').toUpperCase(), type: 'text/html' } } }],
    ['a content type with a newline', { version: 1, files: { '/index.html': { sha256: sha('x'), type: 'text/html\r\nX: y' } } }],
    ['a root entry', { version: 1, files: { ...good.files, '/': { sha256: sha('x'), type: 'text/plain' } } }],
  ];
  it('accepts the good shape', () => {
    expect(parseManifest(JSON.stringify(good)).size).toBe(1);
  });
  it.each(bad)('refuses %s', (_name, value) => {
    expect(() => parseManifest(JSON.stringify(value))).toThrow();
  });
});

describe('handle', () => {
  const files: Record<string, string> = { '/index.html': INDEX, '/assets/app-AbC_1-2.js': JS, '/favicon-32.png': 'png' };
  function deps(overrides: Partial<Record<string, string | Error>> = {}): { logs: string[]; d: Parameters<typeof handle>[1] } {
    const logs: string[] = [];
    return {
      logs,
      d: {
        manifest: manifest(),
        readFile: (path) => {
          const v = overrides[path] ?? files[path];
          if (v instanceof Error) return Promise.reject(v);
          return Promise.resolve(new TextEncoder().encode(v));
        },
        log: (l) => logs.push(l),
      },
    };
  }
  const expectHeaders = (r: Response): void => {
    expect(r.headers.get('content-security-policy')).toBe(CONTENT_SECURITY_POLICY);
    expect(r.headers.get('x-content-type-options')).toBe('nosniff');
  };

  it('serves a listed file with the manifest type, the CSP and nosniff', async () => {
    const { d } = deps();
    const r = await handle({ url: 'conch-app://ui/', method: 'GET' }, d);
    expect(r.status).toBe(200);
    expect(r.headers.get('content-type')).toBe('text/html; charset=utf-8');
    expectHeaders(r);
    expect(await r.text()).toBe(INDEX);
  });

  it('answers HEAD with the headers and no body', async () => {
    const { d } = deps();
    const r = await handle({ url: 'conch-app://ui/assets/app-AbC_1-2.js', method: 'HEAD' }, d);
    expect(r.status).toBe(200);
    expect(r.headers.get('content-type')).toBe('text/javascript; charset=utf-8');
    expect(await r.text()).toBe('');
  });

  it.each(['POST', 'PUT', 'DELETE', 'PATCH', 'OPTIONS'])('refuses %s', async (method) => {
    const { d } = deps();
    const r = await handle({ url: 'conch-app://ui/', method }, d);
    expect(r.status).toBe(405);
    expectHeaders(r);
  });

  it('answers 404, with the CSP, for what is not served', async () => {
    const { d } = deps();
    for (const url of ['conch-app://other/', 'conch-app://ui/%2e%2e/index.html', 'conch-app://ui/nope.js', 'conch-app://ui/?q']) {
      const r = await handle({ url, method: 'GET' }, d);
      expect(r.status, url).toBe(404);
      expectHeaders(r);
    }
  });

  it('answers 500 and logs one line without the bytes when a hash no longer matches', async () => {
    const { d, logs } = deps({ '/assets/app-AbC_1-2.js': 'tampered();' });
    const r = await handle({ url: 'conch-app://ui/assets/app-AbC_1-2.js', method: 'GET' }, d);
    expect(r.status).toBe(500);
    expectHeaders(r);
    expect(await r.text()).not.toContain('tampered');
    expect(logs).toHaveLength(1);
    expect(logs[0]).toContain('/assets/app-AbC_1-2.js');
    expect(logs[0]).not.toContain('tampered');
  });

  it('answers 500 when a listed file cannot be read', async () => {
    const { d, logs } = deps({ '/favicon-32.png': new Error('ENOENT') });
    const r = await handle({ url: 'conch-app://ui/favicon-32.png', method: 'GET' }, d);
    expect(r.status).toBe(500);
    expect(logs).toHaveLength(1);
  });
});

describe('the scheme', () => {
  it('is registered with exactly standard and secure', () => {
    expect(SCHEME_PRIVILEGES).toEqual({ standard: true, secure: true });
  });
});
