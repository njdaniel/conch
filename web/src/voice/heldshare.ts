// A screen share of this tab's held open in its picker, for the tests of the
// pages that refuse to reveal a secret while the tab shares (issue #714). No
// SDK, no network, no device: the picker never answers until the share is
// stopped. Imported by tests only.
import { FakeClock, settle } from './fakes';
import { ScreenShare, type ShareRoom } from './share';

export async function holdShare(): Promise<{ readonly stop: () => Promise<void> }> {
  const share = new ScreenShare(
    {
      claim: () => Promise.resolve({ ok: true, value: { principal_id: 1, started_at: '2026-10-23T00:00:00.000Z', audio: false } }),
      release: () => Promise.resolve(),
    },
    () => undefined,
    new FakeClock(),
  );
  const room: ShareRoom = {
    pickScreen: () => new Promise(() => undefined),
    publishScreen: () => Promise.resolve(),
    stopScreen: () => Promise.resolve(),
    keepScreen: () => null,
    adoptScreen: () => undefined,
  };
  void share.start('general', room);
  await settle();
  return {
    stop: async () => {
      await share.stop('self');
      await settle();
    },
  };
}
