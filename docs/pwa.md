# Install Conch from Chrome on Android

1. Open the HTTPS address of your Conch server in Chrome and go to `/app/`.
2. Open Chrome's menu and choose **Install app**, or **Add to home screen**
   followed by **Install** if Chrome offers both. The exact labels vary by
   Chrome version.
3. Open Conch from the app launcher. Confirm it opens in its own window
   without the browser address bar. If an older home-screen icon still opens
   a Chrome tab, remove that shortcut before installing again.
4. Sign in and allow microphone access when prompted if you want to join
   voice. Keep Conch in the foreground for voice and screen sharing.

Opening and using a conversation requires a connection to the server. The
installed web app uses the online web client, including its existing sign-in
and notification settings. Its manifest holds only public installation metadata.
The existing opt-in web push worker holds no credentials or offline cache.

## Verify an updated server

- On a physical Android device, record the Android and Chrome versions.
  Install from the updated server, open from the launcher, check the icon
  and confirm the address bar is absent.
- In the installed window, check sign-in across a close and reopen, microphone
  permission, join and leave, reconnect after a brief network loss, and resume
  after putting the app in the background. Record background limits that occur.
- Verify access after a server update. Reload the app and confirm it continues
  to use the current web build. The installed manifest has a stable ID and
  start address. The existing push worker has no fetch handler. conchd serves
  the HTML without storing it and uses immutable URLs for hashed assets.

Physical Android installation and voice acceptance for [#1970](https://github.com/njdaniel/conch/issues/1970)
remain pending.
