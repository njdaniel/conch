// The conch-app: scheme (linux-desktop.md §3): the packaged web client, served
// at conch-app://ui/ from a build-time manifest and from nothing else.
//
// Input: a request URL and method, and the manifest (path -> SHA-256 and
// content type) that build/manifest.ts wrote beside the files. Output: a
// Response. This module owns what may be served; it owns no Electron object,
// so the unit tests run it without Electron.
import { createHash } from 'node:crypto';

import { CONTENT_SECURITY_POLICY } from './csp.ts';

export const SCHEME = 'conch-app';
export const HOST = 'ui';
export const INITIAL_URL = `${SCHEME}://${HOST}/`;

// Exactly these two and nothing else: no bypassCSP, corsEnabled,
// allowServiceWorkers, supportFetchAPI or stream. The scheme gets no privilege
// that skips CSP, CORS or Fetch checks.
export const SCHEME_PRIVILEGES = { standard: true, secure: true } as const;

export interface ManifestEntry {
  readonly sha256: string;
  readonly type: string;
}
export type Manifest = ReadonlyMap<string, ManifestEntry>;

// Every path the manifest may name, and every path a request may ask for:
// plain segments of letters, digits, dot, underscore and dash. No percent
// escape, backslash or control character is ever part of a name, so none is
// ever decoded.
const PATH_RE = /^\/[A-Za-z0-9._\-/]*$/;
const SHA256_RE = /^[0-9a-f]{64}$/;
const TYPE_RE = /^[a-z]+\/[a-z0-9.+-]+(; charset=utf-8)?$/;

function plainPath(path: string): boolean {
  if (!PATH_RE.test(path)) return false;
  const segments = path.split('/').slice(1);
  if (path === '/') return true;
  return segments.every((s) => s !== '' && s !== '.' && s !== '..');
}

// parseManifest reads build/manifest.ts's output, and refuses anything that is
// not exactly that shape.
export function parseManifest(text: string): Manifest {
  const data: unknown = JSON.parse(text);
  if (typeof data !== 'object' || data === null || Array.isArray(data)) {
    throw new Error('manifest is not an object');
  }
  const record = data as Record<string, unknown>;
  const files = record['files'];
  if (record['version'] !== 1 || typeof files !== 'object' || files === null || Array.isArray(files)) {
    throw new Error('manifest is not version 1 with a files object');
  }
  const manifest = new Map<string, ManifestEntry>();
  for (const [path, entry] of Object.entries(files)) {
    if (!plainPath(path) || path === '/') throw new Error(`manifest path ${JSON.stringify(path)} is not a plain path`);
    if (typeof entry !== 'object' || entry === null) throw new Error(`manifest entry for ${path} is not an object`);
    const { sha256, type } = entry as Record<string, unknown>;
    if (typeof sha256 !== 'string' || !SHA256_RE.test(sha256)) throw new Error(`manifest sha256 for ${path} is invalid`);
    if (typeof type !== 'string' || !TYPE_RE.test(type)) throw new Error(`manifest content type for ${path} is invalid`);
    manifest.set(path, { sha256, type });
  }
  if (!manifest.has('/index.html')) throw new Error('manifest has no /index.html');
  return manifest;
}

const URL_RE = /^conch-app:\/\/([^/?#]*)([^?#]*)(\?[^#]*)?(#.*)?$/s;

export type Resolution =
  | { readonly kind: 'file'; readonly path: string; readonly entry: ManifestEntry }
  | { readonly kind: 'missing' };

// resolve decides, from the URL text exactly as it arrived, which manifest
// file it names. It looks at the text and not at a parsed URL: a parser
// normalises ".." and "%2e" away, and this must refuse them. Anything it does
// not recognise is missing: another host, a port, userinfo (all of which make
// the authority differ from "ui"), a query, a fragment, a dot segment, a
// percent escape, a backslash, a control character, an empty segment, or a
// path the manifest does not list.
export function resolve(requested: string, manifest: Manifest): Resolution {
  // A fragment names no file and is never part of what a document is fetched
  // by, but Chromium hands it to the handler when a page that has one is
  // reloaded (the client routes by "#/c/<channel>"), and refusing it made the
  // reload of a signed-in window a 404 (found by the packaged REST smoke test,
  // issue #942). So it is dropped here; a query is still refused.
  const rawUrl = requested.replace(/#.*$/su, '');
  const m = URL_RE.exec(rawUrl);
  if (m === null) return { kind: 'missing' };
  const [, authority, rawPath = '', query, fragment] = m;
  if (authority !== HOST || query !== undefined || fragment !== undefined) return { kind: 'missing' };
  const path = rawPath === '' ? '/' : rawPath;
  if (!plainPath(path)) return { kind: 'missing' };
  const key = path === '/' ? '/index.html' : path;
  const entry = manifest.get(key);
  return entry === undefined ? { kind: 'missing' } : { kind: 'file', path: key, entry };
}

function headers(type: string): Headers {
  return new Headers({
    'Content-Type': type,
    'Content-Security-Policy': CONTENT_SECURITY_POLICY,
    'X-Content-Type-Options': 'nosniff',
  });
}

function text(status: number, body: string, extra?: Record<string, string>): Response {
  const h = headers('text/plain; charset=utf-8');
  for (const [k, v] of Object.entries(extra ?? {})) h.set(k, v);
  return new Response(body, { status, headers: h });
}

export interface SchemeDeps {
  readonly manifest: Manifest;
  // Reads a manifest path (always one of the manifest's keys) from the
  // packaged files.
  readonly readFile: (path: string) => Promise<Uint8Array>;
  // One line, never the file's bytes.
  readonly log: (line: string) => void;
}

// handle answers one request: GET and HEAD for a file the manifest lists whose
// bytes still have the listed hash; 404 for everything else; 500 when a
// listed file is gone or its hash no longer matches. Every response, error
// ones included, carries the CSP and nosniff.
export async function handle(request: { readonly url: string; readonly method: string }, deps: SchemeDeps): Promise<Response> {
  if (request.method !== 'GET' && request.method !== 'HEAD') {
    return text(405, 'method not allowed', { Allow: 'GET, HEAD' });
  }
  const found = resolve(request.url, deps.manifest);
  if (found.kind === 'missing') return text(404, 'not found');
  let bytes: Uint8Array;
  try {
    bytes = await deps.readFile(found.path);
  } catch {
    deps.log(`conch-desktop: ${found.path} is listed in the manifest but could not be read`);
    return text(500, 'internal error');
  }
  if (createHash('sha256').update(bytes).digest('hex') !== found.entry.sha256) {
    deps.log(`conch-desktop: ${found.path} no longer has the hash the manifest lists`);
    return text(500, 'internal error');
  }
  return new Response(request.method === 'HEAD' ? null : bytes, { status: 200, headers: headers(found.entry.type) });
}
