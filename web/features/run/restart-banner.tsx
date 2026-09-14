"use client";

// RestartBanner sits above the terminal of a run that is no longer live. A
// run stopped on purpose (by you, a loop, or a daemon restart) is never
// brought back just by opening it, so this is the one place to do that.
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Loader2, RotateCcw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { startRunTty } from "@/lib/agentd";
import { runStateLabel } from "@/lib/run";
import { currentTtyGeom } from "@/lib/tty";

export function RestartBanner({
  runId,
  state,
  onRestarted,
}: {
  runId: string;
  state: string;
  onRestarted?: () => void;
}) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const restart = async () => {
    setBusy(true);
    setError(null);
    try {
      await startRunTty(runId, currentTtyGeom(), { restart: true });
      onRestarted?.();
      await qc.invalidateQueries({ queryKey: ["run-status", runId] });
    } catch (e) {
      setError(e instanceof Error ? e.message : "The session could not be restarted");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      role="status"
      className="border-border/50 bg-muted/40 flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-3 py-2 text-xs"
    >
      <span className="text-muted-foreground min-w-0 flex-1">
        {runStateLabel(state)}. The terminal shows its last output.
        {error && <span className="text-destructive block">{error}</span>}
      </span>
      <Button size="sm" variant="outline" onClick={restart} disabled={busy}>
        {busy ? (
          <Loader2 className="size-4 animate-spin" />
        ) : (
          <RotateCcw className="size-4" />
        )}
        Restart session
      </Button>
    </div>
  );
}
