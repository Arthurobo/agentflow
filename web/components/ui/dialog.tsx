"use client";

// Dialog — a minimal accessible modal (no radix-dialog dependency). Mobile
// first: a bottom sheet with a drag handle that centers as a card on sm+.
// The header stays pinned (blurred) while the body scrolls, and the close
// button is a proper circular touch target.
import * as React from "react";
import { createPortal } from "react-dom";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";

export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  className,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  children: React.ReactNode;
  className?: string;
}) {
  React.useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onOpenChange(false);
    };
    document.addEventListener("keydown", onKey);
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = "";
    };
  }, [open, onOpenChange]);

  if (!open || typeof document === "undefined") return null;
  return createPortal(
    <div
      className="af-modal-overlay fixed inset-0 z-50 flex items-end justify-center p-0 backdrop-blur-xl sm:items-center sm:p-6"
      onClick={() => onOpenChange(false)}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className={cn(
          "af-modal-card bg-card/95 flex max-h-[92dvh] w-full max-w-2xl flex-col overflow-hidden rounded-t-2xl border shadow-[0_30px_120px_color-mix(in_oklch,black_55%,transparent)] sm:max-h-[88dvh] sm:rounded-2xl",
          className,
        )}
      >
        {/* sheet affordance on phones */}
        <div className="flex justify-center pt-2 sm:hidden" aria-hidden>
          <div className="bg-border h-1 w-10 rounded-full" />
        </div>

        <div className="sticky top-0 z-10 flex items-start justify-between gap-3 border-b border-border/60 bg-card/80 px-5 pt-3 pb-3 backdrop-blur-xl sm:pt-4">
          <div className="min-w-0 pt-0.5">
            <h2 className="truncate text-base font-semibold leading-tight">
              {title}
            </h2>
            {description && (
              <p className="text-muted-foreground mt-0.5 line-clamp-2 text-[13px] leading-5">
                {description}
              </p>
            )}
          </div>
          <button
            type="button"
            onClick={() => onOpenChange(false)}
            aria-label="Close"
            title="Close"
            className="grid size-10 shrink-0 place-items-center rounded-full border border-border/70 bg-surface-raised/60 text-muted-foreground shadow-sm transition-all hover:border-primary/40 hover:bg-accent hover:text-foreground active:scale-95"
          >
            <X className="size-4.5" />
          </button>
        </div>

        <div
          className="min-h-0 flex-1 overflow-x-hidden overflow-y-auto overscroll-contain px-5 py-4"
          style={{ paddingBottom: "max(env(safe-area-inset-bottom), 1rem)" }}
        >
          {children}
        </div>
      </div>
    </div>,
    document.body,
  );
}
