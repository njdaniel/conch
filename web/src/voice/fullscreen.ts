// Full screen belongs to the tile on Android and desktop. Orientation is a
// best effort: browsers may omit the API or refuse a lock even in full screen.
// Safari on iPhone has no requestFullscreen for other elements, but can put
// the video itself full screen through webkitEnterFullscreen.
type AppleVideo = HTMLVideoElement & { readonly webkitEnterFullscreen?: () => void };
type OptionalOrientation = {
  readonly lock?: (orientation: 'landscape') => Promise<void>;
  readonly unlock?: () => void | Promise<void>;
};

export async function fullscreenVideo(container: HTMLElement | null, video: AppleVideo | null): Promise<void> {
  if (container === null || !document.fullscreenEnabled) {
    video?.webkitEnterFullscreen?.();
    return;
  }

  const orientation = screen.orientation as OptionalOrientation | undefined;
  const onChange = () => {
    if (document.fullscreenElement !== null) return;
    document.removeEventListener('fullscreenchange', onChange);
    // Leaving full screen restores the phone's normal rotation, even if the
    // lock was refused. Some browsers also throw when asked to unlock.
    try {
      void Promise.resolve(orientation?.unlock?.()).catch(() => undefined);
    } catch { /* Orientation must never prevent leaving full screen. */ }
  };
  document.addEventListener('fullscreenchange', onChange);
  try {
    await container.requestFullscreen();
  } catch {
    document.removeEventListener('fullscreenchange', onChange);
    return;
  }

  // The user may have left while the request was settling. Never lock later,
  // or rotate a portrait stream (including one whose size is not known yet).
  if (document.fullscreenElement !== container || video === null || video.videoWidth <= video.videoHeight) return;
  try {
    await orientation?.lock?.('landscape');
  } catch { /* Full screen still works when orientation locking is refused. */ }
}
