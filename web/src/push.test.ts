import { afterEach, describe, expect, it, vi } from "vitest";
import {
  parsePushConfig,
  pushStatusLabel,
  pushSubscriptionRequest,
  subscribePush,
  unsubscribePush,
  urlBase64ToUint8Array,
} from "./push";

afterEach(() => vi.unstubAllGlobals());

describe("push configuration boundary", () => {
  it("accepts only coherent enabled and disabled configurations", () => {
    expect(parsePushConfig({ enabled: true, public_key: "BA_test" })).toEqual({
      enabled: true,
      publicKey: "BA_test",
    });
    expect(parsePushConfig({ enabled: false, public_key: "" })).toEqual({
      enabled: false,
      publicKey: "",
    });
    expect(parsePushConfig({ enabled: true, public_key: "" })).toBeNull();
    expect(parsePushConfig({ enabled: false, public_key: "unexpected" })).toBeNull();
    expect(parsePushConfig({ enabled: "yes", public_key: "key" })).toBeNull();
  });

  it("decodes unpadded URL-safe application server keys", () => {
    expect([...urlBase64ToUint8Array("AQID-vs")]).toEqual([1, 2, 3, 250, 251]);
    expect(() => urlBase64ToUint8Array("not+url/base64=")).toThrow(/URL-safe/);
  });

  it("strips browser-only fields from the strict subscription request", () => {
    expect(
      pushSubscriptionRequest({
        endpoint: "https://push.example/capability",
        expirationTime: null,
        keys: { p256dh: "public", auth: "secret" },
      }),
    ).toEqual({
      endpoint: "https://push.example/capability",
      keys: { p256dh: "public", auth: "secret" },
    });
    expect(() => pushSubscriptionRequest({ endpoint: "https://push.example/capability" })).toThrow(/incomplete/);
  });

  it("provides distinct operator-facing states", () => {
    expect(pushStatusLabel("subscribed")).toBe("Alerts on");
    expect(pushStatusLabel("denied")).toBe("Alerts blocked");
    expect(pushStatusLabel("disabled")).toBe("Alerts off");
  });

  it("requests permission before any await that can consume user activation", async () => {
    const order: string[] = [];
    const subscription = {
      endpoint: "https://push.example/capability",
      toJSON: () => ({
        endpoint: "https://push.example/capability",
        keys: { p256dh: "public", auth: "secret" },
      }),
    } as unknown as PushSubscription;
    const worker = {
      pushManager: {
        getSubscription: async () => {
          order.push("subscription");
          return subscription;
        },
      },
    } as unknown as ServiceWorkerRegistration;
    const notification = {
      permission: "default" as NotificationPermission,
      requestPermission: async () => {
        order.push("permission");
        return "granted" as NotificationPermission;
      },
    };
    vi.stubGlobal("Notification", notification);
    vi.stubGlobal("window", { isSecureContext: true, PushManager: {}, Notification: notification });
    vi.stubGlobal("navigator", {
      serviceWorker: {
        register: async () => {
          order.push("register");
        },
        ready: Promise.resolve(worker),
      },
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        order.push(`fetch:${String(input)}`);
        return String(input).endsWith("/config")
          ? new Response(JSON.stringify({ enabled: true, public_key: "AQID" }), {
              status: 200,
              headers: { "Content-Type": "application/json" },
            })
          : new Response(null, { status: 204 });
      }),
    );

    await expect(subscribePush()).resolves.toBe("subscribed");
    expect(order).toEqual([
      "permission",
      "fetch:/api/push/config",
      "register",
      "subscription",
      "fetch:/api/push/subscriptions",
    ]);
  });

  it("keeps the browser capability when durable unsubscribe fails", async () => {
    const unsubscribe = vi.fn(async () => true);
    const subscription = {
      endpoint: "https://push.example/capability",
      unsubscribe,
    } as unknown as PushSubscription;
    const worker = {
      pushManager: { getSubscription: async () => subscription },
    } as unknown as ServiceWorkerRegistration;
    const notification = { permission: "granted" as NotificationPermission };
    vi.stubGlobal("Notification", notification);
    vi.stubGlobal("window", { isSecureContext: true, PushManager: {}, Notification: notification });
    vi.stubGlobal("navigator", {
      serviceWorker: { register: async () => worker, ready: Promise.resolve(worker) },
    });
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 503 })));

    await expect(unsubscribePush()).rejects.toThrow(/HTTP 503/);
    expect(unsubscribe).not.toHaveBeenCalled();
  });
});
