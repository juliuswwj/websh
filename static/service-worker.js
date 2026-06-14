/* websh service worker: app-shell cache + Web Push */
'use strict';

const CACHE = 'websh-shell-v1';
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
  self.skipWaiting();
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).catch(() => {}));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  // Never touch the API or websockets — always go to the network.
  if (url.pathname.includes('/api/') || url.pathname.includes('/ws/') || url.pathname.endsWith('/service-worker.js')) return;
  // Cache-first for the app shell / vendor assets; fall back to network.
  e.respondWith(
    caches.match(req).then((hit) => hit || fetch(req).then((resp) => {
      if (resp && resp.ok && resp.type === 'basic') {
        const copy = resp.clone();
        caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
      }
      return resp;
    }).catch(() => hit))
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
  const tabId = e.notification.data && e.notification.data.tabId;
  e.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const c of all) {
      if ('focus' in c) { await c.focus(); if (tabId) c.postMessage({ tabId }); return; }
    }
    await self.clients.openWindow((e.notification.data && e.notification.data.url) || './');
  })());
});
