"use client";

// InstallGate — PWA install enforcement: the app must live on the home
// screen. A browser cannot literally force an install, so this is the
// standard gate — authenticated pages render an install overlay instead of
// the app until the PWA is launched in standalone mode. The overlay shows a
// CARD PER BROWSER PATH, with the visitor's detected browser first and
// highlighted; /pair is exempt, and a dev bypass keeps plain-tab development
// possible. Standalone/bypass state flows through useSyncExternalStore so
// external systems ARE the state — no setState-in-effect cascades.
import { useCallback, useEffect, useState, useSyncExternalStore } from "react";
import { usePathname } from "next/navigation";
import { Download, Globe, Menu, Plus, Share, Smartphone } from "lucide-react";
import { Button } from "@/components/ui/button";
import { isRoute, PAIR_PATH } from "@/lib/paths";

const BYPASS_KEY = "agentflow.bypassInstall";
const BYPASS_EVENT = "agentflow:bypass-install";

interface BeforeInstallPromptEvent extends Event {
  prompt: () => Promise<void>;
  userChoice: Promise<{ outcome: "accepted" | "dismissed" }>;
}

type Browser = "ios" | "android" | "chromium" | "firefox" | "safari" | "other";

function detectBrowser(): Browser {
  const ua = window.navigator.userAgent;
  if (
    /iphone|ipad|ipod/i.test(ua) ||
    (window.navigator.maxTouchPoints > 1 && /Macintosh/.test(ua))
  ) {
    return "ios"; // includes iPadOS 13+ pretending to be macOS
  }
  if (/android/i.test(ua)) {
    return /firefox/i.test(ua) ? "firefox" : "android";
  }
  if (/firefox/i.test(ua)) return "firefox";
  if (/safari/i.test(ua) && !/chrome|chromium|edg|opr/i.test(ua)) return "safari";
  if (/chrome|chromium|edg|opr/i.test(ua)) return "chromium";
  return "other";
}

function isStandalone(): boolean {
  const nav = window.navigator as Navigator & { standalone?: boolean };
  if (nav.standalone === true) return true; // iOS Safari home-screen launch
  return ["standalone", "fullscreen", "minimal-ui"].some(
    (mode) => window.matchMedia(`(display-mode: ${mode})`).matches,
  );
}

// standalone store: matchMedia + appinstalled both flip it
function subscribeStandalone(onChange: () => void) {
  const mq = window.matchMedia("(display-mode: standalone)");
  mq.addEventListener("change", onChange);
  window.addEventListener("appinstalled", onChange);
  return () => {
    mq.removeEventListener("change", onChange);
    window.removeEventListener("appinstalled", onChange);
  };
}
const getStandalone = () => isStandalone();

// bypass store: sticky localStorage flag (+ in-document event for instant lift)
function subscribeBypass(onChange: () => void) {
  window.addEventListener("storage", onChange);
  window.addEventListener(BYPASS_EVENT, onChange);
  return () => {
    window.removeEventListener("storage", onChange);
    window.removeEventListener(BYPASS_EVENT, onChange);
  };
}
const getBypass = () => window.localStorage.getItem(BYPASS_KEY) === "1";

export function InstallGate({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  // /pair and the error boundary must render in a plain browser tab.
  const exempt = isRoute(pathname, PAIR_PATH) || isRoute(pathname, "/_error");

  // optimistic server snapshot: never flash the overlay during hydration;
  // hydrated flips true on first client render (no effect needed)
  const hydrated = useSyncExternalStore(
    () => () => {},
    () => true,
    () => false,
  );
  const installed = useSyncExternalStore(subscribeStandalone, getStandalone, () => true);
  const bypassed = useSyncExternalStore(subscribeBypass, getBypass, () => true);
  const [promptEvent, setPromptEvent] = useState<BeforeInstallPromptEvent | null>(null);
  const [installing, setInstalling] = useState(false);

  const browser: Browser | null = hydrated ? detectBrowser() : null;

  useEffect(() => {
    // dev bypass via URL also sticks to localStorage
    const params = new URLSearchParams(window.location.search);
    if (params.get("bypassInstall") === "1") {
      window.localStorage.setItem(BYPASS_KEY, "1");
      window.dispatchEvent(new Event(BYPASS_EVENT));
    }

    // capture the one-shot Chromium install prompt (and suppress the
    // mini-infobar so the gate is the single install surface)
    const onBeforeInstall = (e: Event) => {
      e.preventDefault();
      setPromptEvent(e as BeforeInstallPromptEvent);
    };
    window.addEventListener("beforeinstallprompt", onBeforeInstall);
    return () => window.removeEventListener("beforeinstallprompt", onBeforeInstall);
  }, []);

  const install = useCallback(async () => {
    if (!promptEvent) return;
    setInstalling(true);
    try {
      await promptEvent.prompt();
      await promptEvent.userChoice;
    } finally {
      setInstalling(false);
    }
    // accepting fires appinstalled normally; re-check anyway in case a
    // quick relaunch beat the event
    window.setTimeout(() => window.dispatchEvent(new Event("appinstalled")), 300);
  }, [promptEvent]);

  if (exempt || installed || bypassed || !hydrated) return <>{children}</>;

  return (
    <div className="bg-background/95 fixed inset-0 z-[100] overflow-y-auto backdrop-blur-sm">
      <div className="mx-auto flex min-h-full max-w-lg flex-col justify-center p-4 sm:p-6">
        <div className="af-panel p-5 sm:p-6">
          <div className="flex items-center gap-3">
            <div className="bg-primary/10 text-primary flex size-12 shrink-0 items-center justify-center rounded-2xl">
              <Smartphone className="size-6" />
            </div>
            <div>
              <h1 className="text-base font-semibold leading-tight sm:text-lg">
                Install Agent Flow
              </h1>
              <p className="text-muted-foreground mt-0.5 text-xs sm:text-sm">
                Runs full-screen, offline-capable, with push — add it to your
                home screen to continue.
              </p>
            </div>
          </div>

          <div className="mt-4 flex flex-col gap-2.5">
            {/* Chrome / Edge / Android — one-tap native install */}
            <InstallCard
              detected={browser === "android" || browser === "chromium"}
              icon={<Download className="size-4" />}
              title={browser === "android" ? "Android · Chrome" : "Chrome, Edge, Brave"}
            >
              {promptEvent ? (
                <Button size="sm" className="w-full" onClick={() => void install()} disabled={installing}>
                  {installing ? "Installing…" : "Install Agent Flow"}
                </Button>
              ) : (
                <Step text="Menu ⋮ → “Install app” / “Add to Home screen”" />
              )}
            </InstallCard>

            {/* iOS Safari — no beforeinstallprompt, guided share sheet */}
            <InstallCard
              detected={browser === "ios"}
              icon={<Share className="size-4" />}
              title="iPhone · Safari"
            >
              <ol className="text-muted-foreground flex flex-col gap-1.5 text-xs sm:text-sm">
                <li className="flex items-center gap-2">
                  <span className="bg-muted flex size-5 shrink-0 items-center justify-center rounded-full text-[10px] font-semibold">1</span>
                  Tap <Share className="inline size-3.5" /> <b>Share</b> in the toolbar
                </li>
                <li className="flex items-center gap-2">
                  <span className="bg-muted flex size-5 shrink-0 items-center justify-center rounded-full text-[10px] font-semibold">2</span>
                  Choose <Plus className="inline size-3.5" /> <b>Add to Home Screen</b>
                </li>
                <li className="flex items-center gap-2">
                  <span className="bg-muted flex size-5 shrink-0 items-center justify-center rounded-full text-[10px] font-semibold">3</span>
                  Open <b>Agent Flow</b> from your home screen
                </li>
              </ol>
            </InstallCard>

            {/* Firefox & everything else — manual menu path */}
            <InstallCard
              detected={browser === "firefox" || browser === "safari" || browser === "other"}
              icon={browser === "firefox" || browser === "safari" ? <Globe className="size-4" /> : <Menu className="size-4" />}
              title={browser === "firefox" ? "Firefox" : browser === "safari" ? "Mac · Safari" : "Other browser"}
            >
              <Step
                text={
                  browser === "firefox"
                    ? "Menu ☰ → “Install” / “Add to Home screen”"
                    : browser === "safari"
                      ? "File → “Add to Dock”, then open from the Dock"
                      : "Browser menu → “Install app” / “Add to Home screen”"
                }
              />
            </InstallCard>
          </div>

          <button
            type="button"
            onClick={() => {
              window.localStorage.setItem(BYPASS_KEY, "1");
              window.dispatchEvent(new Event(BYPASS_EVENT));
            }}
            className="text-muted-foreground hover:text-foreground mt-5 w-full text-xs underline decoration-dotted underline-offset-4"
          >
            Skip for development (plain browser tab)
          </button>
        </div>
      </div>
    </div>
  );
}

function InstallCard({
  detected,
  icon,
  title,
  children,
}: {
  detected: boolean;
  icon: React.ReactNode;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div
      className={
        detected
          ? "border-primary/50 bg-primary/5 ring-primary/30 rounded-xl border p-3 ring-1"
          : "border-border/60 rounded-xl border p-3 opacity-80 transition-opacity hover:opacity-100"
      }
    >
      <div className="flex items-center gap-2">
        <span
          className={
            detected
              ? "bg-primary/15 text-primary flex size-7 shrink-0 items-center justify-center rounded-lg"
              : "bg-muted text-muted-foreground flex size-7 shrink-0 items-center justify-center rounded-lg"
          }
        >
          {icon}
        </span>
        <span className="flex min-w-0 flex-1 items-center gap-2 text-sm font-medium">
          <span className="truncate">{title}</span>
          {detected && (
            <span className="bg-primary/10 text-primary shrink-0 rounded-full px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide">
              your browser
            </span>
          )}
        </span>
      </div>
      <div className="mt-2">{children}</div>
    </div>
  );
}

function Step({ text }: { text: string }) {
  return (
    <p className="text-muted-foreground flex items-start gap-2 text-xs sm:text-sm">
      <Menu className="mt-0.5 size-3.5 shrink-0" />
      <span>{text}</span>
    </p>
  );
}
