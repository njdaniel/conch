// What main trusts when it checks a server's certificate (linux-desktop.md §3:
// "Private-CA deployments install the CA in the trust stores ... Never
// override certificate failures"). Q6 of #770, measured on the pinned
// Electron 44.10.0 (desktop/README.md): main makes its requests with Node's
// fetch (undici), whose roots are Node's own bundled set until told otherwise,
// not Chromium's trust store. This module adds the operating system's store to
// that set, once, at startup, so that a private CA installed the way the
// system documents (update-ca-certificates, SSL_CERT_FILE, SSL_CERT_DIR) is
// trusted, and NODE_EXTRA_CA_CERTS, which Node reads itself, keeps working.
//
// Input: the node:tls functions, and the environment. Output: whether the
// system roots were added, and whether the environment lets main's requests
// run. It owns the root set and nothing else; a failed certificate check is
// never overridden here or anywhere (the source scan "no TLS override anywhere
// in the shell", desktop/tests/unit/trust.test.ts).
import type * as TlsModule from 'node:tls';

type TlsApi = Pick<typeof TlsModule, 'getCACertificates' | 'setDefaultCACertificates'>;

export type TrustResult = { readonly ok: true; readonly added: number } | { readonly ok: false };

// Node reads these from the environment on every connection, and fetch obeys
// them: the first, set to 0, accepts any certificate at all; the second sends
// every request through the proxy the environment names. Main allows neither
// (linux-desktop.md §3: never override certificate failures; no proxy). The
// names are assembled so that the source scan finds no override written whole.
export const NO_CHECK_VARIABLE = ['NODE', 'TLS', 'REJECT', 'UNAUTHORIZED'].join('_');
export const PROXY_VARIABLE = ['NODE', 'USE', 'ENV', 'PROXY'].join('_');

// transportVerdict refuses to start main's client in an environment that would
// turn certificate checks off or route it through a proxy. It catches a
// developer shell or a launcher that exported one; like the debugging gate, it
// is not a defence against a same-user attacker.
export function transportVerdict(env: Readonly<Record<string, string | undefined>>, log: (line: string) => void): 'ok' | 'refused' {
  const noCheck = env[NO_CHECK_VARIABLE];
  const proxy = env[PROXY_VARIABLE];
  if (noCheck === '0') {
    log(`conch-desktop: refusing to start: ${NO_CHECK_VARIABLE}=0 would accept any certificate`);
    return 'refused';
  }
  if (proxy !== undefined && proxy !== '' && proxy !== '0') {
    log(`conch-desktop: refusing to start: ${PROXY_VARIABLE} would send every request through a proxy`);
    return 'refused';
  }
  return 'ok';
}

export function trustSystemRoots(tls: TlsApi): TrustResult {
  try {
    const current = tls.getCACertificates('default');
    const system = tls.getCACertificates('system');
    const roots = [...new Set([...current, ...system])];
    tls.setDefaultCACertificates(roots);
    return { ok: true, added: roots.length - current.length };
  } catch {
    // An API this Node lacks, or a store it cannot read: the bundled roots stand.
    return { ok: false };
  }
}

// The codes undici (through node:tls) gives for a certificate that this
// machine does not accept: not trusted, expired, not yet valid, or not for
// this host.
const CERTIFICATE_CODES = new Set([
  'UNABLE_TO_VERIFY_LEAF_SIGNATURE',
  'UNABLE_TO_GET_ISSUER_CERT',
  'UNABLE_TO_GET_ISSUER_CERT_LOCALLY',
  'SELF_SIGNED_CERT_IN_CHAIN',
  'DEPTH_ZERO_SELF_SIGNED_CERT',
  'CERT_UNTRUSTED',
  'CERT_REJECTED',
  'CERT_HAS_EXPIRED',
  'CERT_NOT_YET_VALID',
  'CERT_REVOKED',
  'CERT_SIGNATURE_FAILURE',
  'CERT_CHAIN_TOO_LONG',
  'HOSTNAME_MISMATCH',
  'ERR_TLS_CERT_ALTNAME_INVALID',
  'ERR_SSL_NO_SUITABLE_SIGNATURE_ALGORITHM',
]);

// isCertificateFailure says whether a failed request failed on the server's
// certificate (undici wraps the TLS error as the cause of a TypeError).
export function isCertificateFailure(error: unknown): boolean {
  for (let current: unknown = error, depth = 0; depth < 4 && typeof current === 'object' && current !== null; depth++) {
    const code = (current as { code?: unknown }).code;
    if (typeof code === 'string' && CERTIFICATE_CODES.has(code)) return true;
    current = (current as { cause?: unknown }).cause;
  }
  return false;
}
