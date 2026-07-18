import { readFile } from "node:fs/promises";
import vm from "node:vm";
import { describe, expect, it, vi } from "vitest";

type Listener = (event: Record<string, unknown>) => void;
type NotificationShape = {
  body: string;
  tag: string;
  data: { url: string };
  [key: string]: unknown;
};

async function serviceWorkerHarness(options?: { clients?: unknown[] }) {
  const listeners = new Map<string, Listener>();
  let shown: { title: string; notification: NotificationShape } | undefined;
  const showNotification = vi.fn(async (title: string, notification: NotificationShape) => {
    shown = { title, notification };
  });
  const openWindow = vi.fn(async () => undefined);
  const claim = vi.fn(async () => undefined);
  const matchAll = vi.fn(async () => options?.clients ?? []);
  const self = {
    location: { origin: "https://thornhill.example" },
    skipWaiting: vi.fn(),
    registration: { showNotification },
    clients: { claim, matchAll, openWindow },
    addEventListener: (type: string, listener: Listener) => listeners.set(type, listener),
  };
  const source = await readFile(new URL("../public/sw.js", import.meta.url), "utf8");
  vm.runInNewContext(source, { self, URL });
  return { listeners, showNotification, shown: () => shown, openWindow, matchAll };
}

async function dispatchWaitable(listener: Listener, event: Record<string, unknown>): Promise<void> {
  let pending: Promise<unknown> | undefined;
  listener({
    ...event,
    waitUntil: (value: Promise<unknown>) => {
      pending = value;
    },
  });
  if (!pending) throw new Error("service-worker event did not register waitUntil work");
  await pending;
}

describe("notification service worker", () => {
  it("shows bounded generic notifications and rejects cross-origin deep links", async () => {
    const harness = await serviceWorkerHarness();
    const listener = harness.listeners.get("push");
    if (!listener) throw new Error("push listener not registered");

    await dispatchWaitable(listener, {
      data: {
        json: () => ({
          title: "t".repeat(200),
          body: "b".repeat(300),
          tag: "tag".repeat(100),
          url: "https://evil.example/phish",
        }),
      },
    });

    expect(harness.showNotification).toHaveBeenCalledOnce();
    const shown = harness.shown();
    if (!shown) throw new Error("notification was not captured");
    expect(shown.title).toHaveLength(120);
    expect(shown.notification.body).toHaveLength(240);
    expect(shown.notification.tag).toHaveLength(120);
    expect(shown.notification.data.url).toBe("/");
  });

  it("focuses an existing Thornhill window at the notification deep link", async () => {
    const navigate = vi.fn(async () => undefined);
    const focus = vi.fn(async () => undefined);
    const client = { url: "https://thornhill.example/", navigate, focus };
    const harness = await serviceWorkerHarness({ clients: [client] });
    const listener = harness.listeners.get("notificationclick");
    if (!listener) throw new Error("notificationclick listener not registered");
    const close = vi.fn();

    await dispatchWaitable(listener, {
      notification: { close, data: { url: "/?attention=42" } },
    });

    expect(close).toHaveBeenCalledOnce();
    expect(navigate).toHaveBeenCalledWith("https://thornhill.example/?attention=42");
    expect(focus).toHaveBeenCalledOnce();
    expect(harness.openWindow).not.toHaveBeenCalled();
  });

  it("opens Thornhill when no controlled window exists", async () => {
    const harness = await serviceWorkerHarness();
    const listener = harness.listeners.get("notificationclick");
    if (!listener) throw new Error("notificationclick listener not registered");

    await dispatchWaitable(listener, {
      notification: { close: vi.fn(), data: { url: "/?attention=9" } },
    });

    expect(harness.openWindow).toHaveBeenCalledWith("https://thornhill.example/?attention=9");
  });
});
