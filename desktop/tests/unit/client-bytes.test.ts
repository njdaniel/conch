import { afterEach, describe, expect, it } from 'vitest';

import { sendJson, startHttp, stack, type Fake } from './support.ts';

const servers: Fake[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});
const serve = async (...args: Parameters<typeof startHttp>): Promise<Fake> => {
  const fake = await startHttp(...args);
  servers.push(fake);
  return fake;
};

describe('bytes in and out (attachments, issue #947)', () => {
  it('sends an upload as the raw body with only the declared type, and the bearer, to the operation’s route', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 201, { schema: 'conch.file.v1', id: 1, channel_id: 1, uploader_id: 7, filename: 'a.txt', media_type: 'text/plain', size: 5, sha256: 'a'.repeat(64), created_at: '2026-10-29T10:00:00Z' });
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('bearer-canary-2');
    const answer = await client.send({
      op: 'uploadFile',
      params: { channel: 'general' },
      query: 'filename=a.txt',
      upload: { mediaType: 'text/plain', bytes: new TextEncoder().encode('hello') },
    });
    expect(answer).toMatchObject({ kind: 'answer', status: 201, ok: true });
    const [seen] = fake.seen;
    expect(seen?.method).toBe('POST');
    expect(seen?.url).toBe('/v2/channels/general/files?filename=a.txt');
    expect(seen?.body).toBe('hello');
    expect(seen?.headers['content-type']).toBe('text/plain');
    expect(seen?.headers['authorization']).toBe('Bearer bearer-canary-2');
    expect(seen?.headers['cookie']).toBeUndefined();
  });

  it('returns a download’s bytes, untouched, on a 2xx answer', async () => {
    const fake = await serve((_r, res) => {
      res.writeHead(200, { 'Content-Type': 'application/octet-stream', 'X-Content-Type-Options': 'nosniff' });
      res.end(Buffer.from([0, 1, 2, 255]));
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    const answer = await client.send({ op: 'fileBytes', params: { id: 4 }, download: { maxBytes: 1024 } });
    expect(answer).toMatchObject({ kind: 'answer', status: 200, ok: true });
    expect(answer.kind === 'answer' ? [...(answer.bytes ?? [])] : []).toEqual([0, 1, 2, 255]);
    expect(fake.seen[0]?.url).toBe('/v2/files/4');
  });

  it('refuses a download larger than its cap', async () => {
    const fake = await serve((_r, res) => {
      res.writeHead(200);
      res.end(Buffer.alloc(2048));
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    expect(await client.send({ op: 'fileBytes', params: { id: 4 }, download: { maxBytes: 1024 } })).toEqual({ kind: 'none', reason: 'oversize' });
  });

  it('does not follow a redirect of a download, and sends the credential nowhere else', async () => {
    const other = await serve((_r, res) => {
      res.end('x');
    });
    const fake = await serve((_r, res) => {
      res.writeHead(302, { Location: `${other.origin}/steal` });
      res.end();
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    expect(await client.send({ op: 'fileBytes', params: { id: 4 }, download: { maxBytes: 1024 } })).toEqual({ kind: 'none', reason: 'redirect' });
    expect(other.seen).toHaveLength(0);
  });

  it('reads a refusal of a download as the error document, with no bytes', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 404, { code: 'file_not_found', message: 'no such file' });
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    const answer = await client.send({ op: 'fileBytes', params: { id: 4 }, download: { maxBytes: 1024 } });
    expect(answer).toMatchObject({ kind: 'answer', status: 404, ok: false, body: { code: 'file_not_found' } });
    expect(answer.kind === 'answer' ? answer.bytes : 1).toBeUndefined();
  });
});
