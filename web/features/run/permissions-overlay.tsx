"use client";

// PermissionsOverlay — native permission + question prompts.
//
// Today the only way to approve a tool is to type `1` (or 2, 3, …) into
// the TUI menu. On a phone, with the key row, that's painful. This
// component subscribes to the engine's pending permission + question
// lists and renders the answers as floating buttons above the terminal.
// The user's choice is replayed through the engine's native API
// (OpenCode: POST /permission/{id}/reply, POST /question/{id}/reply).
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, ShieldAlert, X } from "lucide-react";
import {
  listEnginePermissions,
  listEngineQuestions,
  replyEnginePermission,
  replyEngineQuestion,
} from "@/lib/agentd";

interface PermissionsOverlayProps {
  runId: string;
  // caps gates whether we even poll — saves a round-trip for engines
  // that route permissions through the hook (claude).
  caps?: { replyPermission?: boolean; replyQuestion?: boolean };
}

export function PermissionsOverlay({ runId, caps }: PermissionsOverlayProps) {
  const qc = useQueryClient();
  const permsQ = useQuery({
    queryKey: ["engine-permissions", runId],
    queryFn: () => listEnginePermissions(runId),
    enabled: !!caps?.replyPermission,
    refetchInterval: 3_000,
  });
  const qsQ = useQuery({
    queryKey: ["engine-questions", runId],
    queryFn: () => listEngineQuestions(runId),
    enabled: !!caps?.replyQuestion,
    refetchInterval: 3_000,
  });
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  // pick the FIRST pending permission (multi-permission UI is a later slice)
  const perm = permsQ.data?.permissions?.[0];
  const question = qsQ.data?.questions?.[0];

  // dismiss handled via local state so the buttons don't re-trigger before
  // the engine acknowledges. We rely on the query refetch after a reply —
  //
  // once the engine removes the perm, the poll no longer returns it, so
  // `perm` flips to undefined and the overlay unmounts. The local state is
  // kept only as a fallback for engines whose /permission list still
  // returns the answered item for one extra poll.
  const [dismissed, setDismissed] = useState<string | null>(null);
  const visiblePerm = perm && perm.id !== dismissed ? perm : undefined;
  const visibleQuestion =
    question && question.id !== dismissed ? question : undefined;

  if (!visiblePerm && !visibleQuestion) return null;

  async function reply(
    allow: boolean,
    scope: "session" | "always" = "session",
  ) {
    if (!visiblePerm) return;
    setBusy(visiblePerm.id);
    setError(null);
    try {
      await replyEnginePermission(runId, visiblePerm.id, allow, scope);
      setDismissed(visiblePerm.id);
      await qc.invalidateQueries({ queryKey: ["engine-permissions", runId] });
    } catch (e) {
      setError(e instanceof Error ? e.message : "reply failed");
    } finally {
      setBusy(null);
    }
  }

  async function answer(label: string) {
    if (!visibleQuestion) return;
    setBusy(visibleQuestion.id);
    setError(null);
    try {
      await replyEngineQuestion(runId, visibleQuestion.id, label);
      setDismissed(visibleQuestion.id);
      await qc.invalidateQueries({ queryKey: ["engine-questions", runId] });
    } catch (e) {
      setError(e instanceof Error ? e.message : "answer failed");
    } finally {
      setBusy(null);
    }
  }

  if (visiblePerm) {
    return (
      <div
        role="alertdialog"
        aria-label="Permission request"
        className="bg-background/95 border-warning/40 fixed inset-x-4 bottom-32 z-30 max-w-md rounded-2xl border p-4 shadow-2xl backdrop-blur-xl"
        style={{ paddingBottom: "max(env(safe-area-inset-bottom), 0.5rem)" }}
      >
        <div className="flex items-start gap-3">
          <ShieldAlert
            className="text-warning mt-0.5 size-5 shrink-0"
            aria-hidden
          />
          <div className="min-w-0 flex-1">
            <p className="text-foreground text-sm font-semibold">
              Allow {visiblePerm.tool || "tool"}?
            </p>
            <p className="text-muted-foreground mt-1 truncate text-[12px]">
              {visiblePerm.title || visiblePerm.id}
            </p>
          </div>
        </div>
        <div className="mt-3 grid grid-cols-3 gap-2">
          <button
            type="button"
            onClick={() => reply(true, "always")}
            disabled={!!busy}
            className="bg-success/15 text-success hover:bg-success/25 rounded-xl px-3 py-2 text-[13px] font-medium transition-colors active:scale-95"
          >
            Always
          </button>
          <button
            type="button"
            onClick={() => reply(true)}
            disabled={!!busy}
            className="bg-success text-success-foreground hover:brightness-110 rounded-xl px-3 py-2 text-[13px] font-medium transition-colors active:scale-95"
          >
            <Check className="mr-1 inline size-3.5" />
            Allow
          </button>
          <button
            type="button"
            onClick={() => reply(false)}
            disabled={!!busy}
            className="bg-destructive/15 text-destructive hover:bg-destructive/25 rounded-xl px-3 py-2 text-[13px] font-medium transition-colors active:scale-95"
          >
            <X className="mr-1 inline size-3.5" />
            Deny
          </button>
        </div>
        {error && <p className="text-destructive mt-2 text-[12px]">{error}</p>}
      </div>
    );
  }

  if (visibleQuestion) {
    return (
      <div
        role="alertdialog"
        aria-label="Engine question"
        className="bg-background/95 border-border/60 fixed inset-x-4 bottom-32 z-30 max-w-md rounded-2xl border p-4 shadow-2xl backdrop-blur-xl"
        style={{ paddingBottom: "max(env(safe-area-inset-bottom), 0.5rem)" }}
      >
        <p className="text-foreground text-sm font-semibold">
          {visibleQuestion.header || "Engine question"}
        </p>
        <p className="text-muted-foreground mt-1 text-[13px]">
          {visibleQuestion.prompt || visibleQuestion.question}
        </p>
        {visibleQuestion.options && visibleQuestion.options.length > 0 && (
          <div className="mt-3 flex flex-col gap-2">
            {visibleQuestion.options.map((opt, i) => (
              <button
                key={`${visibleQuestion.id}:${i}`}
                type="button"
                onClick={() => answer(opt.label)}
                disabled={!!busy}
                className="hover:bg-accent/60 border-border/60 rounded-xl border px-3 py-2 text-left transition-colors active:scale-95"
              >
                <span className="text-foreground block text-[14px] font-medium">
                  {opt.label}
                </span>
                {opt.description && (
                  <span className="text-muted-foreground block text-[12px]">
                    {opt.description}
                  </span>
                )}
              </button>
            ))}
          </div>
        )}
        {error && <p className="text-destructive mt-2 text-[12px]">{error}</p>}
      </div>
    );
  }

  return null;
}
