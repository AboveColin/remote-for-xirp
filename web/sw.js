/* Service worker: makes this installable and survivable, nothing more.
 *
 * The shell (HTML, CSS, JS, icons) is cached so opening the app on a phone with no
 * route to the host still paints something instead of Chrome's offline page.
 *
 * API responses are NEVER cached. A cached session list is worse than no session
 * list: it shows an agent as running when it finished an hour ago, which is exactly
 * the class of bug this app exists to avoid.
 */

const SHELL = 'xirp-shell-v4';
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
  if (url.pathname.startsWith('/api/') || url.pathname === '/healthz') return; // never cached

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
