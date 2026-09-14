// notify.ts — service worker registration and in-tab notifications. The daemon
// has no push service, so run lifecycle notifications come from an open tab
// (or the installed app) through the browser Notification API.

const SW_PATH = "/sw.js";

let swPromise: Promise<ServiceWorkerRegistration | null> | null = null;

export function registerServiceWorker(): Promise<ServiceWorkerRegistration | null> {
  if (typeof window === "undefined" || !("serviceWorker" in navigator)) {
    return Promise.resolve(null);
  }
  if (!swPromise) {
    swPromise = navigator.serviceWorker.register(SW_PATH).catch((e) => {
      console.warn("sw register failed", e);
      return null;
    });
  }
  return swPromise;
}

// notifyLocal shows a browser Notification from an open tab, asking for
// permission the first time.
export async function notifyLocal(title: string, body: string, tag?: string): Promise<boolean> {
  if (typeof window === "undefined" || !("Notification" in window)) return false;
  if (typeof Notification !== "function" && !("requestPermission" in Notification)) return false;
  if (Notification.permission === "denied") return false;
  if (Notification.permission === "default") {
    const p = await Notification.requestPermission();
    if (p !== "granted") return false;
  }
  new Notification(title, { body: body.slice(0, 200), tag: tag ?? "agentflow", icon: "/icons/icon-192.png" });
  return true;
}
