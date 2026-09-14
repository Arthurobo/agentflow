"use client";

// app/error.tsx — the friendly crash boundary. Any client-side exception on
// any page renders this instead of the browser's dead "page couldn't load"
// screen, with a one-tap recovery path.
import { AlertTriangle, ArrowLeft, RefreshCcw } from "lucide-react";
import Link from "next/link";

export default function ErrorPage({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <div className="bg-background flex h-[100dvh] flex-col items-center justify-center gap-4 px-6 text-center">
      <span className="bg-destructive/10 text-destructive grid size-14 place-items-center rounded-2xl">
        <AlertTriangle className="size-6" />
      </span>
      <div>
        <p className="text-sm font-medium">Something went sideways</p>
        <p className="text-muted-foreground mt-1 max-w-xs text-xs">
          {error.message || "An unexpected error occurred."}
        </p>
      </div>
      <div className="flex gap-2">
        <button
          type="button"
          onClick={reset}
          className="bg-primary text-primary-foreground inline-flex h-11 items-center gap-2 rounded-full px-5 text-sm font-medium shadow transition-all active:scale-95"
        >
          <RefreshCcw className="size-4" /> Try again
        </button>
        <Link
          href="/"
          className="border-border/70 text-muted-foreground hover:text-foreground inline-flex h-11 items-center gap-2 rounded-full border px-5 text-sm transition-colors"
        >
          <ArrowLeft className="size-4" /> Sessions
        </Link>
      </div>
    </div>
  );
}
