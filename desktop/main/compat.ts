// Server compatibility (issue #946; linux-desktop.md §9, decision D1-9).
// Input: the checked-in allowlist (build/compat.json), what /healthz said, and
// whether this is a packaged build with the development flag on the command
// line. Output: a verdict that says whether the app may sign in to the server
// (no automatic sign-in of a saved profile, and no sign-in form, otherwise).
//
// It owns: the manifest's shape, the exact-match rule (an allowlist of
// releases, never a `>=` range), and the one development override, which only
// an unpackaged build honours.
import type { CompatState } from '../../web/src/platform/desktop/bridge.ts';
import type { OperationId } from '../../web/src/platform/operations.ts';

export const OVERRIDE_FLAG = '--allow-unlisted-server';

export interface CompatManifest {
  readonly servers: readonly string[];
}

export interface CompatPolicy {
  readonly servers: readonly string[];
  // The development flag is in force (never true in a packaged build).
  readonly override: boolean;
}

// What /healthz said: an answer (with the version it named, or none), or no
// usable answer.
export type HealthResult = { readonly kind: 'answered'; readonly version: string | null } | { readonly kind: 'unreachable' };

export interface Verdict {
  readonly state: Exclude<CompatState, 'none'>;
  // The version to show: only ever made of safe characters.
  readonly server: string | null;
}

const RELEASE = /^v\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.]*)?$/u;
const SHOWABLE = /^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$/u;
const MAX_RELEASES = 64;

// parseCompat reads the manifest: `{ "version": 1, "servers": ["v1.2.3"] }`.
// Anything else (a range, a duplicate, an unknown key) is null.
export function parseCompat(text: string): CompatManifest | null {
  let doc: unknown;
  try {
    doc = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof doc !== 'object' || doc === null || Array.isArray(doc)) return null;
  const record = doc as Record<string, unknown>;
  if (!Object.keys(record).every((key) => key === 'version' || key === 'servers')) return null;
  const servers = record['servers'];
  if (record['version'] !== 1 || !Array.isArray(servers) || servers.length > MAX_RELEASES) return null;
  const list: string[] = [];
  for (const entry of servers as unknown[]) {
    if (typeof entry !== 'string' || !RELEASE.test(entry) || list.includes(entry)) return null;
    list.push(entry);
  }
  return { servers: list };
}

// allowUnlisted: the development flag, honoured only by an unpackaged build and
// only as exactly this argument.
export function allowUnlisted(packaged: boolean, argv: readonly string[]): boolean {
  return !packaged && argv.includes(OVERRIDE_FLAG);
}

export function judge(policy: CompatPolicy, health: HealthResult): Verdict {
  if (health.kind === 'unreachable') return { state: 'unreachable', server: null };
  const version = health.version;
  const server = version !== null && SHOWABLE.test(version) ? version : null;
  if (version !== null && policy.servers.includes(version)) return { state: 'supported', server };
  return { state: policy.override ? 'unlisted' : 'unsupported', server };
}

// permits: whether a sign-in may happen at all in this state.
export function permits(state: CompatState): boolean {
  return state === 'supported' || state === 'unlisted';
}

// The response families the client cannot work without, named for the person.
// An answer of one that is not the document this app was built for is an
// incompatibility, not a network error.
export const REQUIRED_FAMILIES: Readonly<Partial<Record<OperationId, string>>> = {
  whoami: 'whoami',
  listChannels: 'channels',
  listMessages: 'messages',
  listMembers: 'members',
  listReadState: 'read state',
  listNets: 'nets',
  voiceOccupancy: 'voice',
};
