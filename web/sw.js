/* Service worker: makes this installable and survivable, nothing more.
 *
 * The shell (HTML, CSS, JS, icons) is cached so opening the app on a phone with no
 * route to the host still paints something instead of Chrome's offline page.
 *
 * API responses are NEVER cached. A cached session list is worse than no session
 * list: it shows an agent as running when it finished an hour ago, which is exactly
 * the class of bug this app exists to avoid.
 */

const SHELL = 'xirp-shell-v9';
const SHELL_FILES = [
  './',
  './index.html',
  './style.css',
  './app.js',
  './nav.js',
  './markdown.js',
  './ansi.js',
  './icon.svg',
  './icon-192.png',
  './icon-512.png',
  './manifest.json',
  './robots.txt',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(SHELL).then((cache) => cache.addAll(SHELL_FILES)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== SHELL).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);
  if (event.request.method !== 'GET') return;
  // Never cached, and /api/events in particular must not be wrapped at all: an event
  // stream does not end, so a worker that waits for the response holds it open forever.
  if (url.pathname.startsWith('/api/') || url.pathname === '/healthz') return;

  // Network first, cache as fallback. Cache-first was the obvious choice and the
  // wrong one: the shell is served by a binary that gets rebuilt constantly, and
  // cache-first meant a new build only appeared on the *second* launch. An app that
  // shows you yesterday's code is worse than one that needs a network round trip.
  event.respondWith(
    // `cache: 'no-cache'` forces revalidation rather than letting the HTTP cache
    // answer. Without it a stale entry from before assets carried ETags was still
    // being served through this worker, which looked exactly like a caching bug in
    // the worker itself.
    fetch(event.request, { cache: 'no-cache' })
      .then((res) => {
        if (res && res.ok && url.origin === self.location.origin) {
          const copy = res.clone();
          caches.open(SHELL).then((cache) => cache.put(event.request, copy));
        }
        return res;
      })
      .catch(() => caches.match(event.request))
  );
});

/* ---- push ----
 *
 * The point of the whole feature: the page is closed, the agent finishes, the phone
 * buzzes. Only the service worker can run then, so this is where a notification is
 * shown and where tapping it decides what to open.
 */

self.addEventListener('push', (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    data = { title: 'Remote For Xirp', body: event.data ? event.data.text() : '' };
  }
  const title = data.title || 'Remote For Xirp';
  event.waitUntil(
    self.registration.showNotification(title, {
      body: data.body || '',
      icon: './icon-192.png',
      badge: './icon-192.png',
      // Tagging by session means a second update replaces the first rather than
      // stacking another line about the same session.
      tag: data.tag || 'xirp',
      renotify: false,
      data: { sessionId: data.sessionId || '' },
    })
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const sessionId = (event.notification.data && event.notification.data.sessionId) || '';
  const url = sessionId ? `./?session=${encodeURIComponent(sessionId)}` : './';
  event.waitUntil(
    (async () => {
      // Prefer a window that is already open: opening a second copy of the app loses
      // whatever was on screen.
      const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
      for (const c of clients) {
        if (c.url.includes(self.location.origin)) {
          await c.focus();
          if (sessionId && 'postMessage' in c) c.postMessage({ openSession: sessionId });
          return;
        }
      }
      await self.clients.openWindow(url);
    })()
  );
});
