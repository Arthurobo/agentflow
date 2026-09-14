"use client";

// AuthGate — every page except /pair/ requires a stored device token
// (agentdReady). The UI is a static export, so there is no server-side check
// in front of the pages: this gate is what keeps protected UI from rendering,
// and the daemon's bearer-token check is what actually protects the data. It
// reacts instantly when a token is evicted on 401 (TOKEN_CLEARED_EVENT).
// Readiness flows through useSyncExternalStore so the store IS the state —
// no setState-in-effect cascades.
import { useEffect, useSyncExternalStore } from "react";
import { usePathname, useRouter } from "next/navigation";
import {
  agentdReady,
  TOKEN_CLEARED_EVENT,
  TOKEN_SET_EVENT,
} from "@/lib/agentd";
import { isRoute, PAIR_PATH, pairUrlFor } from "@/lib/paths";

type AuthState = "checking" | "public" | "in" | "out";

function subscribeAuth(onChange: () => void) {
  window.addEventListener(TOKEN_CLEARED_EVENT, onChange);
  // A token being STORED is the other half of the pair. The `storage` event
  // does not fire in the tab that wrote it, so without this the gate learned
  // about a fresh pairing only because the route changed underneath it.
  window.addEventListener(TOKEN_SET_EVENT, onChange);
  window.addEventListener("storage", onChange);
  return () => {
    window.removeEventListener(TOKEN_CLEARED_EVENT, onChange);
    window.removeEventListener(TOKEN_SET_EVENT, onChange);
    window.removeEventListener("storage", onChange);
  };
}

export function AuthGate({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  // The pair page is the only page that renders before a token exists.
  const isPublic = isRoute(pathname, PAIR_PATH);

  const state = useSyncExternalStore(
    subscribeAuth,
    (): AuthState => (isPublic ? "public" : agentdReady() ? "in" : "out"),
    (): AuthState => "checking", // server render: never flash protected UI
  );

  useEffect(() => {
    if (state !== "out") return;
    // Remember where the visitor was headed (query string included, so a
    // shared /session/?id=… link survives pairing).
    router.replace(pairUrlFor(window.location.pathname + window.location.search));
  }, [state, router]);

  if (state === "in" || state === "public") return <>{children}</>;
  return null; // checking or bouncing — never flash protected content
}
