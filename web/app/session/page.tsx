"use client";

// /session/?id=… — one session's terminal. The id is either a managed run id,
// which is attached to as it is, or an engine session id from the sessions
// list, which is resumed into a terminal run first. After a resume the URL is
// replaced with the run's id, so a reload or a shared link attaches to that
// run instead of resuming the session again.
//
// A static export can't build unknown dynamic segments, so the id is a query
// parameter, read with useSearchParams inside <Suspense>.
import { Suspense, useEffect, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import {
  AlertCircle,
  ArrowLeft,
  Loader2,
  RotateCcw,
  ShieldCheck,
  SquareTerminal,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { TerminalShell } from "@/features/run/terminal-shell";
import {
  AgentdError,
  ensureTtyBySession,
  isRunStopped,
  runForSession,
  runStatus,
  startRunTty,
} from "@/lib/agentd";
import { HOME_PATH, PAIR_PATH, sessionPath } from "@/lib/paths";
import { currentTtyGeom } from "@/lib/tty";

export default function SessionPage() {
  return (
    <Suspense fallback={null}>
      <SessionPageInner />
    </Suspense>
  );
}

function SessionPageInner() {
  const id = useSearchParams().get("id") ?? "";
  // Same query key the terminal's header reads, so attaching to a run costs
  // no second status request.
  const status = useQuery({
    queryKey: ["run-status", id],
    queryFn: () => runStatus(id),
    enabled: id !== "",
    retry: false,
  });

  if (!id) {
    return <OpenFailed title="No session to open" detail="The link is missing its session id." />;
  }
  if (status.data) return <TerminalShell runId={id} />;
  if (status.error instanceof AgentdError && status.error.status === 404) {
    return <ResumeSession key={id} sessionId={id} />;
  }
  if (status.error) {
    return (
      <OpenFailed
        title="could not open the terminal"
        detail={status.error instanceof Error ? status.error.message : "agentflow is unreachable"}
      />
    );
  }
  return <Opening label="opening session" id={id} />;
}

// ResumeSession resolves (or starts) the terminal run for an engine session
// and moves the URL onto it.
function ResumeSession({ sessionId }: { sessionId: string }) {
  const router = useRouter();
  const { data, error } = useQuery({
    queryKey: ["session-tty", sessionId],
    queryFn: () => ensureTtyBySession(sessionId, currentTtyGeom()),
    retry: 1,
    staleTime: 30_000,
  });

  useEffect(() => {
    if (data?.id) router.replace(sessionPath(data.id));
  }, [data?.id, router]);

  // A session whose run was stopped on purpose is not restarted just by
  // opening it; the engineer restarts it explicitly from here.
  const stopped = isRunStopped(error);
  const [restarting, setRestarting] = useState(false);
  const [restartError, setRestartError] = useState<string | null>(null);
  const restart = async () => {
    setRestarting(true);
    setRestartError(null);
    try {
      const run = await runForSession(sessionId);
      await startRunTty(run.id, currentTtyGeom(), { restart: true });
      router.replace(sessionPath(run.id));
    } catch (e) {
      setRestartError(e instanceof Error ? e.message : "The session could not be restarted");
      setRestarting(false);
    }
  };

  if (!error) return <Opening label="resuming session" id={sessionId} />;
  return (
    <OpenFailed
      title={stopped ? "this session was stopped" : "could not open the terminal"}
      detail={
        restartError ??
        (stopped
          ? "Restart it to continue where it left off."
          : error instanceof Error
            ? error.message
            : "agentflow is unreachable")
      }
    >
      {stopped && (
        <Button size="sm" onClick={restart} disabled={restarting}>
          {restarting ? <Loader2 className="size-4 animate-spin" /> : <RotateCcw className="size-4" />}
          Restart session
        </Button>
      )}
    </OpenFailed>
  );
}

function Opening({ label, id }: { label: string; id: string }) {
  return (
    <div className="bg-background flex h-[100dvh] flex-col items-center justify-center gap-4 px-6 text-center">
      <span className="border-border/60 bg-card grid size-16 place-items-center rounded-2xl border shadow-sm">
        <SquareTerminal className="text-primary size-7" />
      </span>
      <div role="status" className="text-muted-foreground flex items-center gap-2 text-sm">
        <Loader2 className="size-4 animate-spin" /> {label}{" "}
        <span className="font-mono">{id.slice(0, 8)}</span>…
      </div>
    </div>
  );
}

function OpenFailed({
  title,
  detail,
  children,
}: {
  title: string;
  detail: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="bg-background flex h-[100dvh] flex-col items-center justify-center gap-4 px-6 text-center">
      <span className="bg-destructive/10 text-destructive grid size-14 place-items-center rounded-2xl">
        <AlertCircle className="size-6" />
      </span>
      <div>
        <p className="text-sm font-medium">{title}</p>
        <p className="text-muted-foreground mt-1 max-w-xs text-xs">{detail}</p>
      </div>
      <div className="flex gap-2">
        {children}
        <Link href={HOME_PATH}>
          <Button variant="outline" size="sm">
            <ArrowLeft className="size-4" /> Sessions
          </Button>
        </Link>
        <Link href={PAIR_PATH}>
          <Button variant="outline" size="sm">
            <ShieldCheck className="size-4" /> Pair device
          </Button>
        </Link>
      </div>
    </div>
  );
}
