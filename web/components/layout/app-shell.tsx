"use client";

// AppShell — decides how much app chrome surrounds the page. /pair is the
// pre-auth surface: no sidebar, no notification center, no install gate (it
// must work in a plain browser tab). Every other page gets the full shell.
//
// NOTE: the InstallGate (PWA install enforcement) is currently OFF — it
// added friction for first-time users and we can re-enable it once we have
// a smoother first-run story. See install-gate.tsx for the dormant code.
import { usePathname } from "next/navigation";
import { useEffect, useState } from "react";
import { Sidebar } from "@/components/layout/sidebar";
import { NotificationCenter } from "@/components/layout/notifications";
import { isRoute, normalizePath, PAIR_PATH } from "@/lib/paths";

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const isPair = isRoute(pathname, PAIR_PATH);
  // The terminal route locks page scroll and fills the screen; it is the one
  // place the iOS address-bar offset matters (see VisualViewportShell).
  const isTerminal = normalizePath(pathname) === "/session";

  if (isPair) return <>{children}</>;

  return (
    <>
      <NotificationCenter />
      <VisualViewportShell pin={isTerminal}>
        <Sidebar />
        <div className="min-w-0 flex-1">{children}</div>
      </VisualViewportShell>
    </>
  );
}

// VisualViewportShell keeps the phone's terminal screen aligned to what is
// actually visible. A normal-flow (or plain `fixed; top:0`) shell is anchored to
// the LAYOUT viewport top, which on iOS Safari sits behind the address bar
// whenever it is expanded (the initial state, and again after it re-expands):
// `visualViewport.offsetTop` is then > 0, so the whole shell is shifted up — the
// top nav is clipped behind the chrome and the terminal, sized to the visible
// height, ends short of the bottom and leaves dead space. Pinning a `fixed` box
// to `top: offsetTop; height: visualViewport.height` lands it exactly on the
// visible band, and it tracks the keyboard opening/closing for free.
//
// Only engaged on a phone-width terminal route: desktop keeps the sidebar-left
// flow, and scrolling pages (home, loops, config) keep normal document flow.
function VisualViewportShell({
  pin,
  children,
}: {
  pin: boolean;
  children: React.ReactNode;
}) {
  const [box, setBox] = useState<{ top: number; height: number } | null>(null);

  useEffect(() => {
    if (typeof window === "undefined") return undefined;
    const mq = window.matchMedia("(max-width: 767px)");
    const vv = window.visualViewport;
    const update = () => {
      // Only pin a phone-width terminal route; desktop/tablet and every other
      // route keep normal document flow (box === null).
      if (!pin || !mq.matches) {
        setBox(null);
        return;
      }
      setBox({
        top: vv?.offsetTop ?? 0,
        height: vv?.height ?? window.innerHeight,
      });
    };
    update();
    vv?.addEventListener("resize", update);
    vv?.addEventListener("scroll", update);
    window.addEventListener("resize", update);
    window.addEventListener("orientationchange", update);
    mq.addEventListener?.("change", update);
    return () => {
      vv?.removeEventListener("resize", update);
      vv?.removeEventListener("scroll", update);
      window.removeEventListener("resize", update);
      window.removeEventListener("orientationchange", update);
      mq.removeEventListener?.("change", update);
    };
  }, [pin]);

  if (box) {
    return (
      <div
        className="fixed left-0 right-0 z-0 flex flex-col overflow-hidden"
        style={{ top: box.top, height: box.height }}
      >
        {children}
      </div>
    );
  }
  return (
    <div className="flex min-h-dvh flex-col md:flex-row">{children}</div>
  );
}
