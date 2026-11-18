// Video PiP keeps the same muted video and its separate in-page audio sink.
// Never detach or replace the video on enter/leave: LiveKit observes these
// events to keep receiving while the page is in the background. Unmounting
// the view closes PiP: that is the watch ending (Stop watching, the share
// gone, leaving voice). A renewed room does not unmount it (VoicePanel's
// heldTiles), since PiP opens again only from a tap.
import { useEffect, useState, type RefObject } from 'react';

export function usePictureInPicture(video: RefObject<HTMLVideoElement | null>) {
  const [active, setActive] = useState(false);
  const [failed, setFailed] = useState(false);
  const supported = document.pictureInPictureEnabled &&
    typeof HTMLVideoElement.prototype.requestPictureInPicture === 'function';
  useEffect(() => {
    const el = video.current;
    if (el === null || !supported) return;
    let live = true;
    const enter = () => {
      if (!live) { closePictureInPicture(el); return; }
      setActive(true);
      setFailed(false);
    };
    const leave = () => { setActive(false); };
    el.addEventListener('enterpictureinpicture', enter);
    el.addEventListener('leavepictureinpicture', leave);
    return () => {
      live = false;
      el.removeEventListener('enterpictureinpicture', enter);
      el.removeEventListener('leavepictureinpicture', leave);
      closePictureInPicture(el);
    };
  }, [video, supported]);
  const toggle = async () => {
    const el = video.current;
    if (el === null) return;
    setFailed(false);
    try {
      if (document.pictureInPictureElement === el) await document.exitPictureInPicture();
      else {
        await el.requestPictureInPicture();
        // A stop-watch/leave may have unmounted the tile during the request.
        if (video.current !== el) closePictureInPicture(el);
      }
    } catch { setFailed(true); }
  };
  return { supported, active, failed, toggle };
}

function closePictureInPicture(video: HTMLVideoElement) {
  if (document.pictureInPictureElement !== video) return;
  void document.exitPictureInPicture().catch(() => undefined);
}
