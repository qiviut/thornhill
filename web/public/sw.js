/* Thornhill service worker: notification delivery only. Durable job and
 * attention state remains server-side; this worker intentionally has no
 * authoritative cache or background job state. */
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

function safePayload(event) {
  let value;
  try {
    value = event.data?.json();
  } catch {
    return null;
  }
  if (!value || typeof value !== "object") return null;
  const title = typeof value.title === "string" ? value.title.slice(0, 120) : "Thornhill needs attention";
  const body = typeof value.body === "string" ? value.body.slice(0, 240) : "Open Thornhill for details.";
  const tag = typeof value.tag === "string" ? value.tag.slice(0, 120) : "thornhill-attention";
  let url = "/";
  if (typeof value.url === "string") {
    try {
      const candidate = new URL(value.url, self.location.origin);
      if (candidate.origin === self.location.origin) url = candidate.pathname + candidate.search + candidate.hash;
    } catch {
      url = "/";
    }
  }
  return { title, body, tag, url };
}

self.addEventListener("push", (event) => {
  const payload = safePayload(event);
  if (!payload) return;
  event.waitUntil(
    self.registration.showNotification(payload.title, {
      body: payload.body,
      tag: payload.tag,
      renotify: true,
      icon: "/icon.svg",
      badge: "/icon.svg",
      data: { url: payload.url },
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const rawURL = event.notification.data?.url;
  let target = new URL("/", self.location.origin);
  if (typeof rawURL === "string") {
    try {
      const candidate = new URL(rawURL, self.location.origin);
      if (candidate.origin === self.location.origin) target = candidate;
    } catch {
      target = new URL("/", self.location.origin);
    }
  }
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((clients) => {
      for (const client of clients) {
        if (new URL(client.url).origin === self.location.origin) {
          void client.navigate(target.href);
          return client.focus();
        }
      }
      return self.clients.openWindow(target.href);
    }),
  );
});
