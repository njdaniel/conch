// The voice fence over the real activationFactory and a real Session
// (issue #1373, S1): the fence is the generation the credential was read
// under, so a sign-in inside an activation does not break later joins, and a
// sign-out or 401 after the join trips it.
import { afterEach, describe, expect, it, vi } from 'vitest';

vi.mock('electron', () => ({}));
vi.mock('../../main/window.ts', () => ({
  partitionFor: () => 'memory',
  createWindow: () => ({ isDestroyed: () => false, isMaximized: () => false, isFullScreen: () => false, webContents: {} }),
}));

import { activationFactory, Live } from '../../main/profiles/activation.ts';
import type { ActivationEnv } from '../../main/profiles/activation.ts';
import { sendJson, startHttp, who, type Fake } from './support.ts';

const servers: Fake[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});

describe('the voice fence', () => {
  it('opened signed out, signed in, then joined: current; after a forget: the fence trips', async () => {
    const server = await startHttp((_req, res) => {
      sendJson(res, 200, who);
    });
    servers.push(server);
    const live = new Live();
    const env = { packaged: false, appPath: '/x', manifest: {}, readUi: () => Promise.resolve(new Uint8Array()), connect: () => ({}), guard: {}, makeFiles: () => ({}), now: Date.now, log: () => undefined } as unknown as ActivationEnv;
    const open = activationFactory(env, live);
    const activation = await open({ profile: { id: 'a'.repeat(32), label: 'a', origin: server.origin, principal: { id: 7, name: 'ada' } }, generation: 1, token: null, onInvalid: () => undefined });
    expect(activation.voiceCredential()).toBeNull();
    // Sign in inside the same activation (a pasted credential, kept).
    const service = live.service;
    expect(service).not.toBeNull();
    const outcome = await service?.signInWithCredential('conch_pasted_token');
    const handle = (outcome?.ok === true && 'handle' in outcome ? outcome.handle : undefined) ?? '';
    expect(service?.keepCredential(handle)).toBe(true);
    // Join: the credential is read under the generation now in force.
    const read = activation.voiceCredential();
    expect(read?.token).toBe('conch_pasted_token');
    expect(read?.origin).toBe(server.origin);
    expect(activation.voiceCurrent(read?.generation ?? -1)).toBe(true);
    // Sign-out or a 401: the identity ends.
    activation.endRequests();
    expect(activation.voiceCurrent(read?.generation ?? -1)).toBe(false);
    expect(activation.voiceCredential()).toBeNull();
  });
});
