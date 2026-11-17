// The two routes only main calls (issue #1854): the consent PUT and its
// DELETE, through the real request executor against a loopback server. The
// bearer rides on main's request alone; the body is the typed
// VoiceCompanionAudioRequestV1; the answer is validated; and the page can name
// neither route (they are not in the shared table the renderer's request reads).
import { afterEach, describe, expect, it } from 'vitest';

import { mainOperations, operationOf } from '../../main/api/main-operations.ts';
import { parseRequest } from '../../main/ipc.ts';
import { invocable } from '../../main/api/service.ts';
import { isOperationId, targetOf } from '../../main/api/target.ts';
import type { VoiceCompanionAudioRequestV1, VoiceCompanionAudioV1 } from '../../../web/src/schema/generated.ts';
import { sendJson, startHttp, stack, type Fake } from './support.ts';

const servers: Fake[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});

const body: VoiceCompanionAudioRequestV1 = { scope: 'system_excluding_call', publisher: 'person' };
const recorded: VoiceCompanionAudioV1 = { scope: 'system_excluding_call', publisher: 'person', started_at: '2026-11-16T12:00:00Z' };

describe('the consent routes', () => {
  it('PUT carries the bearer and the typed body to the fixed route, and the answer is validated', async () => {
    const fake = await startHttp((_r, res) => {
      sendJson(res, 200, recorded);
    });
    servers.push(fake);
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('bearer-canary-1');
    const answer = await client.send({ op: 'voiceShareAudio', params: { channel: 'general' }, body });
    expect(answer).toMatchObject({ kind: 'answer', status: 200, ok: true, body: recorded });
    const seen = fake.seen[0];
    expect(seen?.method).toBe('PUT');
    expect(seen?.url).toBe('/v1/channels/general/voice/companion/audio');
    expect(seen?.headers['authorization']).toBe('Bearer bearer-canary-1');
    expect(seen?.headers['origin']).toBeUndefined();
    expect(JSON.parse(seen?.body ?? '')).toEqual(body);
  });

  it('an answer that is not a consent is no answer; one with a key more is tolerated, as the table’s replies are', async () => {
    let next: unknown = { scope: 'everything', started_at: 'x' };
    const fake = await startHttp((_r, res) => {
      sendJson(res, 200, next);
    });
    servers.push(fake);
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    expect(await client.send({ op: 'voiceShareAudio', params: { channel: 'general' }, body })).toEqual({ kind: 'none', reason: 'invalid' });
    next = { ...recorded, later_field: 1 };
    expect(await client.send({ op: 'voiceShareAudio', params: { channel: 'general' }, body })).toMatchObject({ kind: 'answer', ok: true });
  });

  it('a 400 on the field is an answer main reads as an older server', async () => {
    const fake = await startHttp((_r, res) => {
      sendJson(res, 400, { code: 'invalid_request', message: 'no' });
    });
    servers.push(fake);
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    expect(await client.send({ op: 'voiceShareAudio', params: { channel: 'general' }, body })).toMatchObject({ kind: 'answer', status: 400, ok: false });
  });

  it('DELETE withdraws on the same route, answered 204', async () => {
    const fake = await startHttp((_r, res) => {
      res.writeHead(204);
      res.end();
    });
    servers.push(fake);
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    expect(await client.send({ op: 'voiceShareAudioEnd', params: { channel: 'general' } })).toMatchObject({ kind: 'answer', status: 204, ok: true });
    expect(fake.seen[0]).toMatchObject({ method: 'DELETE', url: '/v1/channels/general/voice/companion/audio' });
  });

  it('a channel is one encoded path segment', () => {
    expect(targetOf('voiceShareAudio', { channel: 'a b' }, undefined)).toBe('/v1/channels/a%20b/voice/companion/audio');
    expect(targetOf('voiceShareAudio', { channel: '../x' }, undefined)).toBeNull();
    expect(targetOf('voiceShareAudioEnd', {}, undefined)).toBeNull();
  });

  it('neither route is the page’s: not in the shared table, not a renderer request, not invocable', () => {
    for (const id of Object.keys(mainOperations)) {
      expect(isOperationId(id)).toBe(false);
      expect(parseRequest({ operation: id, params: { channel: 'general' }, body })).toBeNull();
      expect(operationOf(id as keyof typeof mainOperations).renderer).toBe(false);
    }
    // A name of the shared table still resolves to the shared table's own entry.
    expect(operationOf('whoami').path).toBe('/v1/whoami');
    expect(invocable('whoami')).toBe(true);
  });
});
