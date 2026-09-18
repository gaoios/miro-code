// Weiran PWA Service Worker
// Strategy: Network-first with cache fallback (chat app needs real-time data)

// Bump CACHE_NAME whenever SHELL_ASSETS/CDN_ASSETS change so old caches get evicted.
const CACHE_NAME = 'weiran-v3-hook-err-cards';
const SHELL_ASSETS = [
  '/',
  '/manifest.json',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/apple-touch-icon.png',
  // Local app assets actually loaded by index.html — must match exactly so
  // offline / CDN-down fallback keeps the UI usable.
  '/css/app.css',
  '/js/components.js',
  '/js/drawer-tmux.js',
];
// CDN_ASSETS must mirror the <script>/<link> tags in index.html. Out-of-sync
// entries here = pre-cached files we never use, and missing entries = blank UI
// when the CDN is unreachable.
const CDN_ASSETS = [
  'https://cdn.jsdelivr.net/npm/highlight.js@11.9.0/styles/github-dark.min.css',
  'https://cdn.jsdelivr.net/npm/highlight.js@11.9.0/styles/github.min.css',
  'https://cdn.jsdelivr.net/npm/highlight.js@11.9.0/highlight.min.js',
  'https://cdn.jsdelivr.net/npm/marked@12.0.1/marked.min.js',
  'https://cdn.jsdelivr.net/npm/alpinejs@3.14.1/dist/cdn.min.js',
];

// Install: pre-cache shell assets
self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => {
      // Cache local assets (these will succeed)
      const localPromise = cache.addAll(SHELL_ASSETS);
      // CDN assets: best-effort, don't block install if CDN is down
      const cdnPromise = Promise.allSettled(
        CDN_ASSETS.map((url) => cache.add(url).catch(() => {}))
      );
      return Promise.all([localPromise, cdnPromise]);
    })
  );
  self.skipWaiting();
});

// Activate: clean up old caches
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(
        keys
          .filter((key) => key !== CACHE_NAME)
          .map((key) => caches.delete(key))
      )
    )
  );
  self.clients.claim();
});

// Fetch: network-first for everything, cache fallback for shell
self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);

  // Skip non-GET, WebSocket, API calls, uploads
  if (
    event.request.method !== 'GET' ||
    url.pathname.startsWith('/api/') ||
    url.pathname.startsWith('/uploads/')
  ) {
    return;
  }

  event.respondWith(
    fetch(event.request)
      .then((response) => {
        // Cache successful responses for shell assets and CDN
        if (response.ok) {
          const isShell = SHELL_ASSETS.includes(url.pathname);
          const isCDN = CDN_ASSETS.some((cdnUrl) => event.request.url.startsWith(cdnUrl));
          if (isShell || isCDN) {
            const clone = response.clone();
            caches.open(CACHE_NAME).then((cache) => cache.put(event.request, clone));
          }
        }
        return response;
      })
      .catch(() => {
        // Network failed: try cache
        return caches.match(event.request).then((cached) => {
          if (cached) return cached;
          // For navigation requests, return cached shell
          if (event.request.mode === 'navigate') {
            return caches.match('/');
          }
          return new Response('Offline', {
            status: 503,
            statusText: 'Service Unavailable',
          });
        });
      })
  );
});
