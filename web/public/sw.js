// Conch's service worker (issue #1826): background Web Push, and nothing else.
//
// It holds no credential and makes no request. conchd sends a wake that
// carries no text, and the worker answers every one with the same fixed
// notification: the browser (Safari above all) withdraws the permission of a
// site whose push shows nothing, so exactly one is shown, always. A single tag
// collapses a run of wakes into one notification. Which message, which channel
// and who wrote it are never known here; a click opens the client, which
// catches up through its own backfill.
//
// While a Conch tab is visible the page shows its own alerts and this one may
// show as well: Safari does not allow a push to be skipped, so the limit is
// documented rather than worked around.
//
// Served from /app/sw.js with no-cache, so an updated worker is picked up on
// the next load; it takes over at once (skipWaiting, claim).

self.addEventListener('install', () => {
  void self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('push', (event) => {
  event.waitUntil(
    self.registration.showNotification('New activity in Conch', {
      body: 'Open Conch to see what you missed.',
      tag: 'conch-wake',
      renotify: true,
    }),
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((windows) => {
      const open = windows.find((client) => client.url.startsWith(self.registration.scope));
      return open === undefined ? self.clients.openWindow(self.registration.scope) : open.focus();
    }),
  );
});
