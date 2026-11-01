// The request target of a call: the operation's path template with its
// parameters encoded, then its query (linux-desktop.md §3: "Main maps
// reviewed operation ids to fixed public routes"). The renderer names an
// operation of the table and its values; it never names a URL.
//
// Input: an operation id, path values, an already encoded query. Output: the
// target (path and query), or null for anything that is not exactly a call of
// the table. This module owns what a value may be; the table owns the routes.
import { operations, type OperationId } from '../../../web/src/platform/operations.ts';

export const MAX_PARAM_LENGTH = 256;
export const MAX_QUERY_LENGTH = 2048;
const MAX_PAIRS = 16;

// A path value may be any text except what could change the path it sits in
// or be decoded into it: a separator, a backslash, a percent escape (a value
// with "%2f" is refused, not decoded), a control character, an empty value or
// a dot segment.
const BAD_VALUE = /[/\\%\u0000-\u001f\u007f-\u009f]/u;

export function isOperationId(value: string): value is OperationId {
  return Object.hasOwn(operations, value);
}

export function paramOk(value: unknown): value is string | number {
  if (typeof value === 'number') return Number.isSafeInteger(value) && value >= 0;
  if (typeof value !== 'string') return false;
  return value.length > 0 && value.length <= MAX_PARAM_LENGTH && value !== '.' && value !== '..' && !BAD_VALUE.test(value);
}

// queryOk re-encodes a query the way the platform's URLSearchParams does, so
// what is sent is what was parsed, or gives null: a key is lower-case letters,
// digits, "_" and "-"; there are at most sixteen pairs; the escapes decode.
function queryOk(query: string): string | null {
  if (query.length > MAX_QUERY_LENGTH || /[#?\s\\]/u.test(query)) return null;
  let pairs: [string, string][];
  try {
    pairs = [...new URLSearchParams(query)];
    decodeURIComponent(query.replace(/\+/gu, ' '));
  } catch {
    return null;
  }
  if (pairs.length > MAX_PAIRS) return null;
  if (!pairs.every(([key]) => /^[a-z0-9_-]{1,32}$/u.test(key))) return null;
  return new URLSearchParams(pairs).toString();
}

export function targetOf(id: OperationId, params: Readonly<Record<string, unknown>> | undefined, query: string | undefined): string | null {
  const template = operations[id].path;
  const names = [...template.matchAll(/\{(\w+)\}/gu)].map((m) => m[1] ?? '');
  const given = Object.keys(params ?? {});
  if (given.length !== names.length || !names.every((name) => given.includes(name))) return null;
  let path = template;
  for (const name of names) {
    const value = params?.[name];
    if (!paramOk(value)) return null;
    path = path.replace(`{${name}}`, encodeURIComponent(String(value)));
  }
  if (query === undefined || query === '') return path;
  const clean = queryOk(query);
  return clean === null || clean === '' ? null : `${path}?${clean}`;
}
