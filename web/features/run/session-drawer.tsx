"use client";

// session-drawer.tsx — the full session detail sheet, opened from the run
// header's single info button. Replaces the old header stop/terminate pair:
// destructive actions live HERE with proper labels + confirmations, alongside
// the complete run record and a Refresh-connection control that revives the
// PTY (ensureTTY) and redials the terminal socket.
//
// Phone-first contract: full width AND height on mobile; a right-side sheet
// capped at ~26rem on desktop.
import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import Link from "next/link";
import {
  AlertTriangle,
  CheckCircle2,
  FileText,
  Loader2,
  RefreshCw,
  Square,
  X,
  XCircle,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  startRunTty,
  isRunStopped,
  stopRun,
  terminateRun,
  type RunSession,
} from "@/lib/agentd";
import { currentTtyGeom } from "@/lib/tty";
import { sessionConfigPath } from "@/lib/paths";
import { relTime } from "@/lib/format";
import type { TerminalHandle } from "./terminal-pane";

export function SessionDrawer({
  run,
  open,
  onClose,
  ttyRef,
}: {
  run: RunSession;
  open: boolean;
  onClose: () => void;
  ttyRef?: { current: TerminalHandle | null };
}) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<"stop" | "terminate" | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Escape closes. Confirm/error state is fresh per open because the parent
  // only mounts this drawer while it is open.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;

  const refreshConn = async () => {
    setBusy("refresh");
    setError(null);
    try {
      // idempotent ensureTTY: registers the live PTY or RESUMES a dead run
      try {
        await startRunTty(run.id, currentTtyGeom());
      } catch (e) {
        if (isRunStopped(e)) {
          setError("This session was stopped. Use Restart session to start it again.");
          return;
        }
        // surface the failure so the user knows the hub refused — do not
        // silently swallow it (the old behavior masked real backend errors
        // and made the button look like it always succeeded)
        setError(e instanceof Error ? e.message : "ensureTTY failed");
        return;
      }
      // tell the pane to redial immediately so the user sees the terminal
      // come back without having to refresh the page
      ttyRef?.current?.reconnect();
      await qc.invalidateQueries({ queryKey: ["run-status", run.id] });
    } finally {
      setBusy(null);
    }
  };

  const act = async (kind: "stop" | "terminate") => {
    if (busy) return;
    if (confirm !== kind) {
      setConfirm(kind);
      return;
    }
    setBusy(kind);
    setError(null);
    try {
      if (kind === "stop") await stopRun(run.id);
      else await terminateRun(run.id);
      await qc.invalidateQueries({ queryKey: ["run-status", run.id] });
      setConfirm(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : `${kind} failed`);
    } finally {
      setBusy(null);
    }
  };

  const running = ["starting", "running", "awaiting"].includes(run.state);

  return (
    <div
      className="fixed inset-0 z-50"
      role="dialog"
      aria-label="Session details"
    >
      {/* backdrop */}
      <div
        className="animate-[af-fade-in_150ms_ease-out] absolute inset-0 bg-black/55 backdrop-blur-[2px]"
        onClick={onClose}
      />
      {/* sheet: FULL width+height on mobile, right-side panel on desktop */}
      <div className="bg-background animate-[af-drawer-in_220ms_cubic-bezier(0.16,1,0.3,1)] absolute inset-y-0 right-0 flex h-full w-full flex-col border-l shadow-2xl sm:w-[26rem]">
        <header className="border-border/50 flex h-14 shrink-0 items-center justify-between gap-2 border-b px-4">
          <h2 className="text-sm font-semibold">Session details</h2>
          <div className="flex items-center gap-1">
            <Button variant="ghost" size="sm" asChild>
              <Link
                href={sessionConfigPath(run.id)}
                aria-label="View config"
                title="What rules are in effect"
              >
                <FileText className="size-4" />
                <span className="hidden sm:inline">Config</span>
              </Link>
            </Button>
            <Button
              variant="ghost"
              size="icon"
              onClick={refreshConn}
              disabled={busy !== null}
              aria-label="Refresh connection"
              title="Refresh connection — revive the PTY and redial"
            >
              {busy === "refresh" ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <RefreshCw className="size-4" />
              )}
            </Button>
            <Button
              variant="ghost"
              size="icon"
              onClick={onClose}
              aria-label="Close details"
            >
              <X className="size-5" />
            </Button>
          </div>
        </header>

        <div
          className="min-h-0 flex-1 space-y-5 overflow-y-auto px-4 py-4"
          // The drawer's last action (Terminate) sat under the iPhone home
          // indicator without this — reachable only by over-scrolling.
          style={{ paddingBottom: "max(env(safe-area-inset-bottom), 1rem)" }}
        >
          {error && (
            <p className="text-destructive bg-destructive/10 flex items-center gap-2 rounded-lg px-3 py-2 text-xs">
              <AlertTriangle className="size-3.5 shrink-0" /> {error}
            </p>
          )}

          <section className="space-y-2">
            <h3 className="text-muted-foreground text-[11px] font-semibold uppercase tracking-wide">
              Connection
            </h3>
            <Row k="State" v={run.blocked ? "blocked" : run.state} mono />
            <Row k="Kind" v={run.kind} mono />
            <Row k="Model" v={run.model || "(default)"} mono />
            <Row
              k="Claude session"
              v={run.sessionId || "(discovering)"}
              mono
              wrap
            />
            <Row k="Run id" v={run.id} mono wrap />
          </section>

          <section className="space-y-2">
            <h3 className="text-muted-foreground text-[11px] font-semibold uppercase tracking-wide">
              Context
            </h3>
            <Row k="Working dir" v={run.cwd} mono wrap />
            <Row k="Project" v={run.project} />
            {run.prompt && <Row k="Prompt" v={run.prompt} wrap />}
            {run.resumeFrom ? (
              <Row k="Resumed from" v={run.resumeFrom} mono wrap />
            ) : null}
            <Row k="Created by" v={run.createdBy} />
          </section>

          <section className="space-y-2">
            <h3 className="text-muted-foreground text-[11px] font-semibold uppercase tracking-wide">
              Lifecycle & usage
            </h3>
            <Row k="Started" v={relTime(run.startedAt)} />
            {run.endedAt > 0 && <Row k="Ended" v={relTime(run.endedAt)} />}
            {run.exitCode !== 0 && (
              <Row k="Exit code" v={String(run.exitCode)} mono />
            )}
            <Row k="Events" v={String(run.eventCount)} mono />
            {run.permissionDenials > 0 && (
              <Row
                k="Permission denials"
                v={String(run.permissionDenials)}
                mono
              />
            )}
            {run.totalCostUsd > 0 && (
              <Row k="Cost" v={`$${run.totalCostUsd.toFixed(4)}`} mono />
            )}
            {run.terminalReason ? (
              <Row k="End reason" v={run.terminalReason} wrap />
            ) : null}
            {run.lastError ? (
              <Row k="Last error" v={run.lastError} wrap danger />
            ) : null}
          </section>

          {running && (
            <section className="space-y-2 border-t pt-4">
              <h3 className="text-muted-foreground text-[11px] font-semibold uppercase tracking-wide">
                Actions
              </h3>
              <ActionRow
                label={
                  confirm === "stop"
                    ? "Tap again to confirm STOP"
                    : "Stop (SIGINT)"
                }
                hint="Interrupts the current turn; the session stays continuable."
                icon={
                  confirm === "stop" ? (
                    <CheckCircle2 className="size-4" />
                  ) : busy === "stop" ? (
                    <Loader2 className="size-4 animate-spin" />
                  ) : (
                    <Square className="size-3.5" />
                  )
                }
                tone={confirm === "stop" ? "confirm" : "default"}
                disabled={busy !== null}
                onClick={() => void act("stop")}
                onBlur={() => setConfirm((c) => (c === "stop" ? null : c))}
              />
              <ActionRow
                label={
                  confirm === "terminate"
                    ? "Tap again to TERMINATE"
                    : "Terminate session"
                }
                hint="Kills the process now. The session stays resumable afterwards."
                icon={
                  busy === "terminate" ? (
                    <Loader2 className="size-4 animate-spin" />
                  ) : (
                    <XCircle className="size-4" />
                  )
                }
                tone={confirm === "terminate" ? "danger" : "default"}
                disabled={busy !== null}
                onClick={() => void act("terminate")}
                onBlur={() => setConfirm((c) => (c === "terminate" ? null : c))}
              />
            </section>
          )}
        </div>
      </div>
    </div>
  );
}

function Row({
  k,
  v,
  mono,
  wrap,
  danger,
}: {
  k: string;
  v?: string;
  mono?: boolean;
  wrap?: boolean;
  danger?: boolean;
}) {
  if (!v) return null;
  return (
    <div className="flex items-start justify-between gap-3 text-sm">
      <span className="text-muted-foreground shrink-0">{k}</span>
      <span
        className={cn(
          "min-w-0 text-right",
          mono && "font-mono text-xs",
          wrap ? "break-all" : "truncate",
          danger && "text-destructive",
        )}
      >
        {v}
      </span>
    </div>
  );
}

function ActionRow({
  label,
  hint,
  icon,
  tone,
  disabled,
  onClick,
  onBlur,
}: {
  label: string;
  hint: string;
  icon: React.ReactNode;
  tone: "default" | "confirm" | "danger";
  disabled?: boolean;
  onClick: () => void;
  onBlur: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      onBlur={onBlur}
      disabled={disabled}
      className={cn(
        "flex w-full items-center gap-3 rounded-xl border px-3 py-2.5 text-left transition-colors",
        tone === "confirm" && "border-warning/40 bg-warning/10",
        tone === "danger" &&
          "border-destructive bg-destructive text-destructive-foreground",
        tone === "default" &&
          "border-border/60 hover:border-border hover:bg-accent/60",
        disabled && "cursor-not-allowed opacity-60",
      )}
    >
      <span className="shrink-0">{icon}</span>
      <span className="min-w-0">
        <span className="block text-sm font-medium">{label}</span>
        <span className="text-muted-foreground block text-xs">{hint}</span>
      </span>
    </button>
  );
}
