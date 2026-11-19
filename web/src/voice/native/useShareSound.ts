// The share's sound in the desktop strip (issue #1854; linux-share-audio.md
// §9): what the share control needs of main's share sound, and the two
// moments the page owes it. The page never captures or publishes sound and
// holds no scope, name or identifier of its own: it asks main, after the share
// is live ("start") and again when the share ends ("end", naming the share main
// gave), and on the person's click ("choose", "mute"). Everything shown is
// main's view: the choice and a fixed reason (view.shareSound) and the helper's
// confirmed state (view.helper.shareAudio). Nothing is optimistic.
//
// Present only when main's switch is on (the view carries a share sound) and
// the bridge has the calls: otherwise undefined, and the strip says what it
// said before the route existed.
import { useEffect, useRef, useState } from 'react';
import type { NativeShareAudio, NativeVoiceView, ShareSoundChoice, ShareSoundView } from '../../platform/desktop/bridge';
import type { NativeVoice } from '../../platform/types';
import type { Companion } from './useCompanion';
import { shareSoundMuteFailed, shareSoundNotKept } from './words';

export interface ShareSoundUi {
  // Main's part of the view: the profile's choice, and at most one reason.
  readonly view: ShareSoundView;
  // The helper's confirmed state of the share's sound, when it has one (v3).
  readonly audio: NativeShareAudio | undefined;
  // The page's share is live (or resuming): the sound is shared with it.
  readonly live: boolean;
  // A choice or a mute waits for main's answer.
  readonly busy: boolean;
  // One line for a call that did not take, or null.
  readonly note: string | null;
  // The person's click on the toggle, and on the mute.
  choose(choice: ShareSoundChoice): void;
  mute(muted: boolean): void;
}

export function useShareSound(source: NativeVoice | undefined, companion: Companion | null, view: NativeVoiceView): ShareSoundUi | undefined {
  const actions = source?.shareSound;
  const sound = view.shareSound;
  const offered = actions !== undefined && sound !== undefined && companion !== null;
  const kind = companion?.state.share.kind;
  // The one definition of a live share for the share's sound: the page's share
  // is claimed and sending (or resuming). Starting, claiming, picking and
  // publishing are not live yet, and the toggle is not shown in them either
  // (NativeShare uses this `live`), so a choice can never be made for a share
  // the hook would not hold.
  const live = kind === 'sharing' || kind === 'resuming';
  const switched = sound !== undefined;

  // The number main gave for the share's sound now live, to end it with; and
  // whether the share is still live when a late answer comes in.
  const owned = useRef<number | null>(null);
  const liveNow = useRef(live);
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);

  useEffect(() => {
    liveNow.current = live;
  }, [live]);

  // The share is live: ask main to add its sound (consent, then the helper;
  // main checks the choice, the route and that the share is live), and when it
  // ends, whoever ended it, say so, so that its sound ends too. The choice is
  // not a dependency: changing it during a share is the toggle's own call.
  useEffect(() => {
    if (actions === undefined || !offered || !live) return;
    let current = true;
    void actions.start().then((result) => {
      // A failed start may still name the record main kept for this share (its
      // words): it is ended with the share all the same.
      const share: number | undefined = result.share;
      if (share === undefined) return;
      // The share ended while main was asking: end what it made.
      if (current) owned.current = share;
      else void actions.end(share);
    });
    return () => {
      current = false;
      const share = owned.current;
      owned.current = null;
      if (share !== null) void actions.end(share);
    };
  }, [actions, offered, live]);

  // Whenever the live share ends (or the page goes), tell main, with no token:
  // a share that never had a sound record (the choice none, a start that made
  // none) also stops being a share main would add sound to. Runs after the
  // token's own end above (effects clean up in order). Only with main's
  // switch on: otherwise main would only refuse it.
  useEffect(() => {
    if (actions === undefined || !switched || !live) return;
    return () => {
      void actions.shareEnded();
    };
  }, [actions, switched, live]);

  if (actions === undefined || sound === undefined || companion === null) return undefined;

  const choose = (choice: ShareSoundChoice): void => {
    if (busy) return;
    setBusy(true);
    setNote(null);
    void actions
      .choose(choice)
      .then((result) => {
        if (!result.ok) {
          setNote(shareSoundNotKept);
          return;
        }
        if (result.share === null) return;
        // The choice put the sound on for the live share: end it with the share,
        // or now if the share has already ended.
        if (liveNow.current) owned.current = result.share;
        else void actions.end(result.share);
      })
      .finally(() => {
        setBusy(false);
      });
  };

  const mute = (muted: boolean): void => {
    if (busy) return;
    setBusy(true);
    setNote(null);
    void actions
      .mute(muted)
      .then((result) => {
        if (!result.ok) setNote(shareSoundMuteFailed);
      })
      .finally(() => {
        setBusy(false);
      });
  };

  return { view: sound, audio: view.helper?.shareAudio, live, busy, note, choose, mute };
}
