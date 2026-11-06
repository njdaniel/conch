// The server address (linux-desktop.md §3: "Only the selected profile controls
// the base address: root-mounted HTTPS origins; plain HTTP only for explicit
// loopback development; reject userinfo, fragments, unexpected base paths and
// non-HTTP schemes").
//
// Input: the text a person typed. Output: the canonical origin, or the name of
// the rule it broke. This module owns the address rules and nothing else; it
// opens no connection.
import { isIP } from 'node:net';

export type AddressRefusal = 'malformed' | 'scheme' | 'userinfo' | 'path';

export type ParsedAddress = { readonly ok: true; readonly origin: string } | { readonly ok: false; readonly refusal: AddressRefusal };

const MAX_LENGTH = 2048;

// Whitespace, control characters and backslashes are never part of an address
// somebody means: the URL parser would quietly repair some of them ("https:\\"
// is "https://"), and this refuses instead of repairing.
const UNSAFE = /[\u0000- \u007f-\u009f\\]/u;

// loopback is the same rule as web/src/transport.ts: localhost, 127.0.0.0/8
// and ::1. hostname is as the URL parser gives it (lower-case, punycode, an
// IPv6 address in brackets).
export function loopback(hostname: string): boolean {
  const host = hostname.replace(/^\[|\]$/gu, '');
  if (host === 'localhost' || host === '::1') return true;
  return isIP(host) === 4 && /^127\./u.test(host);
}

export function parseServerAddress(input: string): ParsedAddress {
  if (input.length === 0 || input.length > MAX_LENGTH || UNSAFE.test(input)) return { ok: false, refusal: 'malformed' };
  let url: URL;
  try {
    url = new URL(input);
  } catch {
    return { ok: false, refusal: 'malformed' };
  }
  if (url.protocol !== 'https:' && url.protocol !== 'http:') return { ok: false, refusal: 'scheme' };
  // The parser accepts "https:example.com"; the address has "//" after it.
  if (!/^https?:\/\//iu.test(input)) return { ok: false, refusal: 'malformed' };
  if (url.username !== '' || url.password !== '' || /^https?:\/\/[^/?#]*@/iu.test(input)) return { ok: false, refusal: 'userinfo' };
  if (url.pathname !== '/' || url.search !== '' || url.hash !== '' || /[?#]/u.test(input)) return { ok: false, refusal: 'path' };
  if (url.hostname === '') return { ok: false, refusal: 'malformed' };
  if (url.protocol === 'http:' && !loopback(url.hostname)) return { ok: false, refusal: 'scheme' };
  return { ok: true, origin: url.origin };
}
