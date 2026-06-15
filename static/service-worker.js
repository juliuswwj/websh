/* websh service worker: network-first app shell + Web Push */
'use strict';

const CACHE = 'websh-shell-v10';
// Scope-relative URLs (the SW is registered with the app's BASE scope).
const SHELL = [
  './',
  'index.html',
  'app.js',
  'manifest.json',
  'vendor/xterm.js',
  'vendor/xterm.css',
  'vendor/addon-fit.js',
];

self.addEventListener('install', (e) => {
  // Take over immediately so cache-strategy / code updates land quickly.
  self.skipWaiting();
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).catch(() => {}));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

function cachePut(req, resp) {
  if (resp && resp.ok && resp.type === 'basic') {
    const copy = resp.clone();
    caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
  }
}

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;
  const p = new URL(req.url).pathname;
  // Never touch API / websockets / the SW itself.
  if (p.includes('/api/') || p.endsWith('/ws') || p.includes('/ws/') || p.endsWith('/service-worker.js')) return;

  // Immutable vendor assets + icons: cache-first (big, rarely change).
  if (p.includes('/vendor/') || p.includes('/icons/')) {
    e.respondWith(caches.match(req).then((hit) => hit || fetch(req).then((resp) => { cachePut(req, resp); return resp; })));
    return;
  }

  // App shell (HTML, app.js, manifest): NETWORK-FIRST so a reload always gets the
  // latest when online; fall back to cache only when offline. This is what makes
  // F5 actually pick up a new app.js.
  e.respondWith(
    fetch(req).then((resp) => { cachePut(req, resp); return resp; })
      .catch(() => caches.match(req).then((hit) => hit || caches.match('index.html')))
  );
});

// ---- Web Push --------------------------------------------------------------
self.addEventListener('push', (e) => {
  let d = { title: 'websh', body: '终端需要你的关注' };
  try { if (e.data) d = e.data.json(); } catch (_) {}
  e.waitUntil(self.registration.showNotification(d.title || 'websh', {
    body: d.body || '',
    tag: d.tabId || 'websh',
    data: { url: d.url || './', tabId: d.tabId },
    badge: 'icons/icon-192.png',
    icon: 'icons/icon-192.png',
  }));
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  e.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const c of all) { if ('focus' in c) { await c.focus(); return; } }
    await self.clients.openWindow((e.notification.data && e.notification.data.url) || './');
  })());
});
