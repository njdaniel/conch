// Profile ids: 128 random bits as 32 lowercase hex characters (issue #945).
// They fit the vault's id rule, cannot collide by case folding and are never
// a reserved Windows name. An id is never made from user text.
import { randomBytes } from 'node:crypto';

const PROFILE_ID = /^[0-9a-f]{32}$/;

export function newProfileId(): string {
  return randomBytes(16).toString('hex');
}

export function isProfileId(value: unknown): value is string {
  return typeof value === 'string' && PROFILE_ID.test(value);
}
