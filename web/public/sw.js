// Agent Flow service worker.
//
// It caches exactly two things: Next's content-hashed build assets
// (/_next/static/*) and the app icons (/icons/*). Both are public and never
// change under the same URL. Everything else, pages, the manifest and above
// all /api/, goes straight to the network and is never stored: API responses
// carry authenticated data, and a cached page could outlive the build it was
// served with.
//
// The cache name is stamped with the build id by scripts/stamp-sw.mjs after
// `next build`, so each release starts a fresh cache and drops the old one.
const CACHE_PREFIX = "agentflow-";
const CACHE = CACHE_PREFIX + "__BUILD_ID__";

// cacheableRequest decides whether a request may be answered from, and stored
// in, the cache. Anything it rejects is left entirely to the browser.
function cacheableRequest(request, origin) {
  if (request.method !== "GET") return false;
  let url;
  try {
    url = new URL(request.url);
  } catch {
    return false;
  }
  if (url.origin !== origin) return false;
  const path = url.pathname;
  if (path === "/api" || path.startsWith("/api/")) return false;
  return path.startsWith("/_next/static/") || path.startsWith("/icons/");
}

self.addEventListener("install", (event) => {
  // Nothing is precached: the pages are cheap to fetch and the assets they
  // need are cached the first time they are loaded.
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys
            .filter((k) => k.startsWith(CACHE_PREFIX) && k !== CACHE)
            .map((k) => caches.delete(k)),
        ),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  if (!cacheableRequest(event.request, self.location.origin)) return;
  event.respondWith(
    caches.open(CACHE).then((cache) =>
      cache.match(event.request).then((hit) => {
        if (hit) return hit;
        return fetch(event.request).then((resp) => {
          // Only complete same-origin successes: a cached 404 from a
          // half-finished deploy would be served forever.
          if (resp && resp.status === 200 && resp.type === "basic") {
            cache.put(event.request, resp.clone()).catch(() => {});
          }
          return resp;
        });
      }),
    ),
  );
});
