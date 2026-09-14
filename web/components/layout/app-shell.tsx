"use client";

// AppShell — decides how much app chrome surrounds the page. /pair is the
// pre-auth surface: no sidebar, no notification center, no install gate (it
// must work in a plain browser tab). Every other page gets the full shell.
//
// NOTE: the InstallGate (PWA install enforcement) is currently OFF — it
// added friction for first-time users and we can re-enable it once we have
// a smoother first-run story. See install-gate.tsx for the dormant code.
import { usePathname } from "next/navigation";
import { Sidebar } from "@/components/layout/sidebar";
import { NotificationCenter } from "@/components/layout/notifications";
import { isRoute, PAIR_PATH } from "@/lib/paths";

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const isPair = isRoute(pathname, PAIR_PATH);

  if (isPair) return <>{children}</>;

  return (
    <>
      <NotificationCenter />
      <div className="flex min-h-dvh flex-col md:flex-row">
        <Sidebar />
        <div className="min-w-0 flex-1">{children}</div>
      </div>
    </>
  );
}
