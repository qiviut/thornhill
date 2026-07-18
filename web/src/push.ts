export type PushStatus =
  | "checking"
  | "unavailable"
  | "disabled"
  | "prompt"
  | "denied"
  | "subscribed";

interface PushConfig {
  enabled: boolean;
  publicKey: string;
}

export function parsePushConfig(value: unknown): PushConfig | null {
  if (typeof value !== "object" || value === null) return null;
  const record = value as Record<string, unknown>;
  if (typeof record.enabled !== "boolean" || typeof record.public_key !== "string") return null;
  if (record.enabled && record.public_key.length === 0) return null;
  if (!record.enabled && record.public_key.length !== 0) return null;
  return { enabled: record.enabled, publicKey: record.public_key };
}

export function urlBase64ToUint8Array(value: string): Uint8Array<ArrayBuffer> {
  if (!/^[A-Za-z0-9_-]+$/.test(value)) throw new Error("invalid URL-safe base64 key");
  const padding = "=".repeat((4 - (value.length % 4)) % 4);
  const base64 = (value + padding).replace(/-/g, "+").replace(/_/g, "/");
  const decoded = globalThis.atob(base64);
  const bytes = new Uint8Array(new ArrayBuffer(decoded.length));
  for (let index = 0; index < decoded.length; index += 1) bytes[index] = decoded.charCodeAt(index);
  return bytes;
}

function supported(): boolean {
  return window.isSecureContext && "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
}

async function loadConfig(signal?: AbortSignal): Promise<PushConfig> {
  const response = await fetch("/api/push/config", {
    signal,
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (!response.ok) throw new Error(`notification configuration: HTTP ${response.status}`);
  const config = parsePushConfig(await response.json());
  if (!config) throw new Error("notification configuration was malformed");
  return config;
}

async function registration(): Promise<ServiceWorkerRegistration> {
  await navigator.serviceWorker.register("/sw.js", { scope: "/" });
  return navigator.serviceWorker.ready;
}

export function pushSubscriptionRequest(value: PushSubscriptionJSON): {
  endpoint: string;
  keys: { p256dh: string; auth: string };
} {
  const p256dh = value.keys?.p256dh;
  const auth = value.keys?.auth;
  if (typeof value.endpoint !== "string" || typeof p256dh !== "string" || typeof auth !== "string") {
    throw new Error("browser returned an incomplete push subscription");
  }
  return { endpoint: value.endpoint, keys: { p256dh, auth } };
}

async function persistSubscription(subscription: PushSubscription): Promise<void> {
  const response = await fetch("/api/push/subscriptions", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(pushSubscriptionRequest(subscription.toJSON())),
  });
  if (!response.ok) throw new Error(`notification enrollment: HTTP ${response.status}`);
}

export async function getPushStatus(signal?: AbortSignal): Promise<PushStatus> {
  if (!supported()) return "unavailable";
  const worker = await registration();
  const config = await loadConfig(signal);
  const existing = await worker.pushManager.getSubscription();
  if (existing) {
    // Refresh the durable server record without prompting when delivery is
    // enabled. If the server is disabled, still report the browser subscription
    // so the operator can explicitly remove it.
    if (config.enabled) await persistSubscription(existing);
    return "subscribed";
  }
  if (!config.enabled) return "disabled";
  if (Notification.permission === "denied") return "denied";
  return "prompt";
}

export async function subscribePush(): Promise<PushStatus> {
  if (!supported()) return "unavailable";
  // Ask while the button click still carries transient user activation. Safari
  // and installed iOS PWAs may reject a permission request after a network await.
  let permission = Notification.permission;
  if (permission === "default") permission = await Notification.requestPermission();
  if (permission !== "granted") return permission === "denied" ? "denied" : "prompt";
  const config = await loadConfig();
  if (!config.enabled) return "disabled";
  const worker = await registration();
  let subscription = await worker.pushManager.getSubscription();
  if (!subscription) {
    subscription = await worker.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(config.publicKey),
    });
  }
  try {
    await persistSubscription(subscription);
  } catch (error) {
    await subscription.unsubscribe().catch(() => false);
    throw error;
  }
  return "subscribed";
}

export async function unsubscribePush(): Promise<PushStatus> {
  if (!supported()) return "unavailable";
  const worker = await registration();
  const subscription = await worker.pushManager.getSubscription();
  if (!subscription) return Notification.permission === "denied" ? "denied" : "prompt";
  const response = await fetch("/api/push/subscriptions", {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ endpoint: subscription.endpoint }),
  });
  if (!response.ok) throw new Error(`notification removal: HTTP ${response.status}`);
  await subscription.unsubscribe();
  return "prompt";
}

export function pushStatusLabel(status: PushStatus): string {
  switch (status) {
    case "checking":
      return "Alerts…";
    case "subscribed":
      return "Alerts on";
    case "prompt":
      return "Enable alerts";
    case "denied":
      return "Alerts blocked";
    case "disabled":
      return "Alerts off";
    case "unavailable":
      return "Alerts unavailable";
  }
}
