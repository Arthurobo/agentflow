"use client";

// TerminalShell — the full-height frame around a run's terminal. The terminal
// takes everything above a pinned bottom composer that types into it. The
// shell is sized by the visual viewport when available: the on-screen
// keyboard shrinks it directly, so the composer stays pinned just above the
// keyboard and the terminal keeps the remaining space (no padding hacks).
// Height accounts for the mobile top nav (h-14) on small screens.
import { useEffect, useState } from "react";
import { RunView } from "./run-view";

export function TerminalShell({ runId }: { runId: string }) {
  const [vpHeight, setVpHeight] = useState<number | null>(null);

  useEffect(() => {
    const vv = window.visualViewport;
    const update = () => {
      const nav = window.innerWidth < 768 ? 56 : 0; // mobile top nav h-14
      // Fall back to innerHeight where visualViewport is unavailable, so the
      // shell is still JS-sized rather than left on the dvh fallback.
      const h = vv?.height ?? window.innerHeight;
      setVpHeight(Math.max(240, h - nav));
    };
    vv?.addEventListener("resize", update);
    vv?.addEventListener("scroll", update);
    // visualViewport does not fire on every Android browser, and a desktop
    // window resize or an orientation change can leave the visual viewport
    // identical while the layout viewport changes. Without these the shell
    // kept a stale height and the terminal stayed fitted to the old one.
    window.addEventListener("resize", update);
    window.addEventListener("orientationchange", update);
    update();
    return () => {
      vv?.removeEventListener("resize", update);
      vv?.removeEventListener("scroll", update);
      window.removeEventListener("resize", update);
      window.removeEventListener("orientationchange", update);
    };
  }, []);

  // Lock the DOCUMENT while the chat shell is mounted AND actively reset any
  // window scroll: iOS Safari scrolls the page (not the shell) to reveal the
  // focused input when the keyboard opens — overflow:hidden alone does not
  // stop it. Forcing scrollTop back to 0 keeps the shell exactly where the
  // visualViewport sync put it, so the composer stays pinned above the keys.
  useEffect(() => {
    const html = document.documentElement;
    const prevHtml = html.style.overflow;
    const prevBody = document.body.style.overflow;
    html.style.overflow = "hidden";
    document.body.style.overflow = "hidden";
    const resetScroll = () => {
      if (window.scrollY !== 0 || window.scrollX !== 0) window.scrollTo(0, 0);
    };
    window.addEventListener("scroll", resetScroll, { passive: true });
    return () => {
      window.removeEventListener("scroll", resetScroll);
      html.style.overflow = prevHtml;
      document.body.style.overflow = prevBody;
    };
  }, []);

  return (
    <main
      className="h-[calc(100dvh-3.5rem)] md:h-dvh"
      style={vpHeight !== null ? { height: vpHeight } : undefined}
    >
      <RunView key={runId} runId={runId} />
    </main>
  );
}
