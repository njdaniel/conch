import { describe, expect, it } from 'vitest';
import { parsePersonFrame, type PersonFrame } from './personStream';

// The frames are the Go golden fixtures, so the client reads what the server
// writes (pkg/schema/testdata, one of them per frame the stream carries).
const fixtures = import.meta.glob<unknown>('../../../pkg/schema/testdata/*.json', {
  eager: true,
  import: 'default',
});
const text = (file: string): string => {
  const doc = Object.entries(fixtures).find(([path]) => path.endsWith(`/${file}`))?.[1];
  if (doc === undefined) throw new Error(`no fixture ${file}`);
  return JSON.stringify(doc);
};

describe('parsePersonFrame', () => {
  const cases: readonly { readonly file: string; readonly kind: PersonFrame['kind'] }[] = [
    { file: 'person-stream-hello-v1.json', kind: 'hello' },
    { file: 'dm-snapshot-v1.json', kind: 'snapshot' },
    { file: 'dm-snapshot-v1-empty.json', kind: 'snapshot' },
    { file: 'dm-message-v1.json', kind: 'dm_message' },
    { file: 'dm-message-edited-v1.json', kind: 'dm_message_edited' },
    { file: 'dm-message-removed-v1.json', kind: 'dm_message_removed' },
    { file: 'dm-changed-v1.json', kind: 'dm_changed' },
  ];
  it.each(cases)('reads $file as $kind', ({ file, kind }) => {
    expect(parsePersonFrame(text(file))?.kind).toBe(kind);
  });

  it('carries the instance id of the hello', () => {
    const frame = parsePersonFrame(text('person-stream-hello-v1.json'));
    expect(frame?.kind === 'hello' ? frame.hello.instance_id : null).toBe('0f1e2d3c4b5a69788796a5b4c3d2e1f0');
  });

  it('ignores what it does not know or cannot trust', () => {
    expect(parsePersonFrame(text('channels-changed-v1.json'))).toBeNull(); // a channel frame is not this stream's
    expect(parsePersonFrame('{"schema":"conch.call_state.v1"}')).toBeNull(); // a later frame
    expect(parsePersonFrame('{"schema":"conch.person_stream_hello.v1"}')).toBeNull(); // fails its validator
    expect(parsePersonFrame('not json')).toBeNull();
    expect(parsePersonFrame('null')).toBeNull();
    expect(parsePersonFrame(new ArrayBuffer(2))).toBeNull();
  });

  it('keeps a field a later server adds', () => {
    const doc: Record<string, unknown> = { ...(JSON.parse(text('dm-changed-v1.json')) as Record<string, unknown>), later: 1 };
    expect(parsePersonFrame(JSON.stringify(doc))?.kind).toBe('dm_changed');
  });
});
