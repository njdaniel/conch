// The one place an attachment is written to disk (issue #947). Input: an
// absolute path the person chose in the native save dialog, and the file's
// bytes. Output: those bytes at that path, as the person's own plain file
// whose mode is 0600 every time, new or overwritten (never executable, never
// readable by others), and nothing else: it opens, runs and previews nothing.
// main/files.ts has already refused any path that the dialog did not give;
// the unit test that confines fs writes lists this module by name.
//
// How: the file is opened for writing without being truncated and without
// following a symlink at the path (O_NOFOLLOW: a link there is refused with an
// error, not written through to wherever it points), its mode is set to 0600
// on the open handle (so an existing 0644 or 0755 file does not keep its
// mode), and only then is it emptied and written. A symlink in a directory
// above the file is followed as the operating system does for any path the
// person chose.
import { constants, open } from 'node:fs/promises';

export const SAVED_MODE = 0o600;

// The part of an open file this uses; a test gives one that writes a byte at a
// time, or none.
export interface Handle {
  chmod(mode: number): Promise<void>;
  truncate(length: number): Promise<void>;
  write(bytes: Uint8Array, offset: number, length: number, position: number): Promise<{ readonly bytesWritten: number }>;
  close(): Promise<void>;
}

export type Opener = (path: string, flags: number, mode: number) => Promise<Handle>;

// A write may take fewer bytes than it was given (a full disk, a quota, a
// signal): it goes on from where it stopped, and gives up, with an error and
// not a "saved", when it makes no progress.
export async function saveChosenFile(path: string, bytes: Uint8Array, opener: Opener = open): Promise<void> {
  const handle = await opener(path, constants.O_WRONLY | constants.O_CREAT | constants.O_NOFOLLOW, SAVED_MODE);
  try {
    await handle.chmod(SAVED_MODE);
    await handle.truncate(0);
    let at = 0;
    while (at < bytes.byteLength) {
      const { bytesWritten } = await handle.write(bytes, at, bytes.byteLength - at, at);
      if (bytesWritten <= 0) throw new Error('short write');
      at += bytesWritten;
    }
  } finally {
    await handle.close();
  }
}
