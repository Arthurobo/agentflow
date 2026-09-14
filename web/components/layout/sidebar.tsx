"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { Activity, LayoutDashboard, Repeat, ShieldCheck } from "lucide-react";
import { ThemeToggle } from "./theme-toggle";
import { normalizePath } from "@/lib/paths";
import { cn } from "@/lib/utils";

// also lists other routes an entry stays lit on: a session's terminal is
// reached from the Sessions list, but "/" only matches itself.
export const nav: { href: string; label: string; icon: typeof Repeat; also?: string[] }[] = [
  { href: "/", label: "Sessions", icon: LayoutDashboard, also: ["/session/"] },
  { href: "/loop/", label: "Loops", icon: Repeat },
  { href: "/pair/", label: "Pair device", icon: ShieldCheck },
];

// navActive reports whether a nav entry is the current section.
export function navActive(pathname: string, entry: { href: string; also?: string[] }): boolean {
  return [entry.href, ...(entry.also ?? [])].some((h) => isActive(pathname, h));
}

// isActive matches on a path BOUNDARY, never a bare startsWith: a sibling
// route that merely begins with a nav href (/loopback next to /loop) must not
// light it. Trailing slashes are ignored on both sides, because the static
// export serves every page at "<route>/".
export function isActive(pathname: string, href: string): boolean {
  const p = normalizePath(pathname);
  const h = normalizePath(href);
  if (h === "/") return p === "/";
  return p === h || p.startsWith(h + "/");
}

export function Sidebar() {
  const pathname = usePathname();
  return (
    <aside className="border-sidebar-border bg-sidebar/90 supports-[backdrop-filter]:bg-sidebar/74 sticky top-0 z-30 flex h-14 w-full shrink-0 items-center gap-1 border-b px-2 shadow-[0_1px_0_color-mix(in_oklch,var(--foreground)_6%,transparent)_inset] backdrop-blur-xl md:h-dvh md:w-60 md:flex-col md:items-stretch md:gap-0 md:border-r md:border-b-0 md:px-3 md:py-4">
      {/* Brand wordmark, desktop only. On a phone the Sessions tab IS the
          home link, and a boxed logo beside it read as a second active tab
          (same destination, different height). Plain text, no box, no glow,
          so it can never be mistaken for navigation. */}
      <Link
        href="/"
        className="text-sidebar-foreground hidden items-center gap-2 rounded-lg px-2 py-2 font-semibold md:flex"
      >
        <Activity className="text-primary size-4" aria-hidden />
        <span className="md:text-sm">Agent Flow</span>
      </Link>
      <nav className="af-no-scrollbar flex flex-1 items-center gap-1 overflow-x-auto px-1 md:mt-5 md:flex-col md:items-stretch md:gap-1.5 md:px-0">
        {nav.map((entry) => (
          <NavItem
            key={entry.href}
            href={entry.href}
            label={entry.label}
            active={navActive(pathname, entry)}
            icon={<entry.icon className="size-4" />}
          />
        ))}
      </nav>
      <div className="hidden md:block">
        <ThemeToggle />
      </div>
      <div className="md:hidden">
        <ThemeToggle />
      </div>
    </aside>
  );
}

function NavItem({
  href,
  label,
  icon,
  active,
}: {
  href: string;
  label: string;
  icon: ReactNode;
  active: boolean;
}) {
  return (
    <Link
      href={href}
      aria-label={label}
      title={label}
      aria-current={active ? "page" : undefined}
      className={cn(
        "group relative flex h-9 min-w-9 items-center justify-center gap-2 rounded-md px-2 text-sm transition-[background-color,color,box-shadow,transform] duration-150 md:justify-start md:py-1.5",
        // Contained active state: a filled pill the same height as its
        // siblings. af-selected is the card-selection ring; its 2px outer
        // box-shadow spilled past the 36px row and looked misaligned.
        active
          ? "bg-sidebar-accent text-sidebar-accent-foreground font-semibold"
          : "text-sidebar-foreground/68 hover:-translate-y-px hover:bg-sidebar-accent/70 hover:text-sidebar-accent-foreground",
      )}
    >
      <span
        className={cn(
          "absolute left-0 hidden h-4 w-0.5 rounded-full bg-primary transition-opacity md:block",
          active ? "opacity-100" : "opacity-0",
        )}
      />
      <span className={cn(active && "text-primary")}>{icon}</span>
      <span className="hidden md:inline">{label}</span>
    </Link>
  );
}
