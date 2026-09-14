"use client";

// loop-board.tsx — the screen the engineer leaves open. The play as a diagram
// with the current step lit, a lane per member, kept current by polling the
// loop's rows, so the board never keeps a second copy of the truth.

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertCircle,
  AlertTriangle,
  ChevronDown,
  ChevronRight,
  CircleDot,
  Clock,
  Loader2,
  ShieldAlert,
  Users,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { EmptyState, SectionHeader } from "@/components/ui/surface";
import { cn } from "@/lib/utils";
import { sessionPath } from "@/lib/paths";
import {
  getLoop,
  listPlays,
  listTools,
  loopMessages,
  loopStanding,
  loopRefusals,
  LoopError,
  type Loop,
  type LoopRefusal,
} from "@/lib/loop";
import { LoopActions } from "./loop-actions";
import {
  diagram,
  lanes,
  MEMBER_STATE_LABEL,
  memberState,
  type DiagramStep,
  type LaneState,
  type MemberState,
} from "./projection";

// How often the board rereads a loop. Starting members change within seconds;
// an open loop moves at the pace of its members' turns.
export const STARTING_POLL_MS = 3000;
export const OPEN_POLL_MS = 10_000;

export function LoopBoard({ loopId }: { loopId: string }) {
  const qc = useQueryClient();

  const [now, setNow] = useState(() => Date.now());

  // The board polls: quickly while any member is still coming up (a crew that
  // came up silently is exactly the failure the engineer would read as
  // broken), slowly while the loop is open, and not at all once it has ended.
  //
  // Read straight off the query's own data rather than mirrored into state: a
  // setState in an effect to track derived data is a cascading render, and
  // there is nothing here that state knows and the data does not.
  const detail = useQuery({
    queryKey: ["loop", loopId],
    queryFn: () => getLoop(loopId),
    refetchInterval: (query) => {
      const data = query.state.data;
      if ((data?.crew ?? []).some((m) => memberState(m) === "starting")) return STARTING_POLL_MS;
      return data?.loop?.status === "active" ? OPEN_POLL_MS : false;
    },
  });
  const open = detail.data?.loop?.status === "active";
  const messages = useQuery({
    queryKey: ["loop", loopId, "messages"],
    queryFn: () => loopMessages(loopId),
    refetchInterval: open ? OPEN_POLL_MS : false,
  });
  // Refusals used to render as system cards in the conversation. The
  // conversation is gone and a refusal is the loop saying it declined to
  // proceed, which is the one thing the engineer must not have to hunt for.
  // The board is now the only place that can say it, so it says it here.
  const refusals = useQuery({
    queryKey: ["loop", loopId, "refusals"],
    queryFn: () => loopRefusals(loopId),
    refetchInterval: open ? OPEN_POLL_MS : false,
  });
  const plays = useQuery({ queryKey: ["loop", "plays"], queryFn: listPlays });
  const tools = useQuery({ queryKey: ["loop", "tools"], queryFn: listTools });

  // One refresh path for every verb on this screen: the rows are the truth, so
  // an action refetches rather than patching a second copy of them.
  const refresh = () => qc.invalidateQueries({ queryKey: ["loop"] });

  // The stall signal is a function of elapsed time, so it needs a clock.
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 15_000);
    return () => clearInterval(t);
  }, []);

  const loop = detail.data?.loop;
  const play = plays.data?.find((p) => p.name === loop?.play);
  const crew = useMemo(() => detail.data?.crew ?? [], [detail.data]);
  const rows = useMemo(() => messages.data ?? [], [messages.data]);

  const laneStates = useMemo(
    () => (loop ? lanes(loop, crew, rows, play, now) : []),
    [loop, crew, rows, play, now],
  );

  const steps = useMemo(() => (loop ? diagram(loop, play) : []), [loop, play]);

  if (detail.isError) {
    // "No such loop" was every failure: a 404 from a routing bug, a 401 from a
    // sleeping machine, and a genuinely deleted loop, all three words. Opening
    // a healthy running loop said it had been lost.
    return (
      <div
        role="alert"
        className="border-destructive/40 bg-destructive/5 text-destructive flex flex-wrap items-center gap-2 rounded-md border px-3 py-2 text-sm"
      >
        <AlertCircle className="size-4 shrink-0" />
        <span className="min-w-0">
          Could not load this loop:{" "}
          {detail.error instanceof LoopError
            ? `${detail.error.message}${detail.error.code ? ` (${detail.error.code})` : ""}`
            : "the loop surface did not answer"}
        </span>
        <Button size="sm" variant="outline" className="ml-auto" onClick={() => void detail.refetch()}>
          Retry
        </Button>
      </div>
    );
  }
  if (detail.isLoading) return <EmptyState loading title="Loading this loop" />;
  if (!loop) {
    return (
      <EmptyState icon={<Users className="size-4" />} title="No such loop">
        It was deleted, or the id in the address is wrong.
      </EmptyState>
    );
  }

  return (
    <div className="flex flex-col gap-5">
      <SectionHeader
        eyebrow={loop.play ? `${loop.play} · round ${loop.round}` : "no play"}
        title={loop.title || loop.task}
        description={loop.state || detail.data?.stepBrief || ""}
        actions={
          <>
            <Badge variant="outline">{loopStanding(loop)}</Badge>
            {loop.playStatus && loop.playStatus !== "done" && (
              <Badge variant="outline">{loop.playStatus}</Badge>
            )}
          </>
        }
      />

      <LoopStatusLine loop={loop} lanes={laneStates} stepActor={detail.data?.stepActor ?? ""} />

      <Refusals rows={refusals.data ?? []} />

      {/* The board answers one question: is this loop healthy, waiting on me,
          stalled or finished. Every member is a LINK to its own terminal,
          which is the only place a session is read now. */}
      <MemberList lanes={laneStates} />

      <PlayDiagram steps={steps} />

      <LoopActions loop={loop} plays={plays.data ?? []} tools={tools.data ?? []} onChanged={refresh} />
    </div>
  );
}

// MemberList is the roster. Each row says what the member is doing, whether it
// holds the current step and whether anything is waiting on it, and goes to
// that member's TERMINAL.
//
// A member without a run id has no terminal to go to: it is external, or it is
// in the seconds between its row being written and its process existing. That
// row is not a link, and it says which of the two it is rather than offering a
// destination that would 404.
function MemberList({ lanes }: { lanes: LaneState[] }) {
  if (lanes.length === 0) {
    return <EmptyState icon={<Users className="size-4" />} title="No members" />;
  }
  return (
    <ul aria-label="members" className="flex flex-col gap-2">
      {lanes.map((l) => (
        <li key={l.role}>
          {l.runId ? (
            <Link
              href={sessionPath(l.runId)}
              className="af-panel hover:border-primary/40 flex items-center gap-2 rounded-lg p-3 transition-colors"
            >
              <MemberRow lane={l} />
              <ChevronRight className="text-muted-foreground size-4 shrink-0" aria-hidden />
            </Link>
          ) : (
            <div className="af-panel flex items-center gap-2 rounded-lg p-3">
              <MemberRow lane={l} />
              <span className="text-muted-foreground ml-auto shrink-0 text-[11px]">
                {l.external ? "runs elsewhere" : "no terminal yet"}
              </span>
            </div>
          )}
        </li>
      ))}
    </ul>
  );
}

function MemberRow({ lane: l }: { lane: LaneState }) {
  return (
    <>
      <MemberStateChip state={l.state} />
      <span className="min-w-0 truncate font-mono text-sm font-semibold">{l.role}</span>
      {l.holding && <Badge>holding</Badge>}
      {l.awaited && !l.holding && <Badge variant="outline">awaited</Badge>}
      {l.stalledBriefs > 0 && (
        <span className="inline-flex items-center gap-1 text-xs text-warning">
          <Clock className="size-3" /> {l.stalledBriefs} stalled
        </span>
      )}
      {l.runId && (
        <span className="text-muted-foreground ml-auto hidden shrink-0 font-mono text-[10px] sm:inline">
          {l.tool}
          {l.model ? ` · ${l.model}` : ""}
        </span>
      )}
    </>
  );
}

// Refusals is enforcement made visible. Newest first, because the one that
// stopped the loop is the one being read.
function Refusals({ rows }: { rows: LoopRefusal[] }) {
  if (rows.length === 0) return null;
  const ordered = [...rows].sort((a, b) => b.createdAt - a.createdAt);
  return (
    <section aria-label="refusals" className="flex flex-col gap-2">
      {ordered.map((r) => (
        <div
          key={r.id}
          className="border-warning/40 bg-warning/5 flex flex-col gap-1 rounded-lg border px-3 py-2"
        >
          <div className="flex items-center gap-2">
            <ShieldAlert className="text-warning size-4 shrink-0" aria-hidden />
            <span className="font-mono text-xs font-semibold">{r.kind}</span>
            {r.stepId && <Badge variant="outline">{r.stepId}</Badge>}
          </div>
          <p className="text-muted-foreground text-sm">{r.content || r.kind}</p>
        </div>
      ))}
    </section>
  );
}

export function PlayDiagram({ steps }: { steps: DiagramStep[] }) {
  // Seven steps wrap to about four rows at 390px, which on a phone is four
  // rows of machinery above the thing he came to read. Below lg it collapses
  // to the one line that actually answers "where are we".
  const [open, setOpen] = useState(false);
  if (steps.length === 0) return null;
  const current = steps.find((s) => s.mark === "current");
  const done = steps.filter((s) => s.mark === "done").length;
  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className="af-panel flex w-full items-center gap-2 rounded-lg p-2 text-left text-xs lg:hidden"
      >
        {open ? <ChevronDown className="size-3.5 shrink-0" /> : <ChevronRight className="size-3.5 shrink-0" />}
        <span className="min-w-0 flex-1 truncate">
          {current ? (
            <>
              <span className="font-mono font-semibold">{current.actor}</span>
              <span className="text-muted-foreground"> on {current.id}</span>
            </>
          ) : (
            <span className="text-muted-foreground">play finished</span>
          )}
        </span>
        <span className="text-muted-foreground shrink-0 text-xs">
          {done}/{steps.length}
        </span>
      </button>
      <ol
        className={cn(
          "flex-wrap items-stretch gap-1.5 lg:flex",
          open ? "mt-2 flex lg:mt-0" : "hidden",
        )}
        aria-label="play"
      >
        {steps.map((s) => (
          <li
            key={s.id}
            data-mark={s.mark}
            aria-current={s.mark === "current" ? "step" : undefined}
            className={cn(
              "af-panel min-w-36 flex-1 rounded-lg border-l-2 p-2 lg:flex-none",
              s.mark === "current" && "border-l-primary bg-primary/10",
              s.mark === "done" && "border-l-emerald-500/50 opacity-70",
              s.mark === "ahead" && "border-l-border opacity-50",
            )}
          >
            <div className="flex items-center gap-1.5">
              {s.mark === "current" && <CircleDot className="text-primary size-3" />}
              <span className="font-mono text-xs font-semibold">{s.actor}</span>
              {s.fanIn && <Badge variant="outline">fan-in</Badge>}
            </div>
            <p className="text-muted-foreground mt-0.5 text-xs leading-5">{s.note}</p>
            {s.awaiting.length > 0 && (
              <p className="text-primary mt-1 font-mono text-xs">
                waiting: {s.awaiting.join(", ")}
              </p>
            )}
          </li>
        ))}
      </ol>
    </div>
  );
}


// MemberStateChip is the first thing on a lane, because "is this member
// actually running" is the first thing the engineer wants to know and the
// board used to answer it only when the answer was no.
export function MemberStateChip({ state }: { state: MemberState }) {
  // Red is reserved for what the engineer must act on. A member working past
  // its lease is amber, never red: it may still be thinking, and a detector
  // that shouts at a busy agent is one nobody reads twice.
  const tone: Record<MemberState, string> = {
    listening: "bg-success/15 text-success",
    working: "bg-success/15 text-success",
    overdue: "bg-warning/15 text-warning",
    stranded: "bg-destructive/15 text-destructive",
    starting: "bg-primary/15 text-primary",
    failed: "bg-destructive/15 text-destructive",
    external: "bg-muted text-muted-foreground",
  };
  return (
    <span
      data-state={state}
      title={
        state === "external"
          ? "this host did not start it; it joined by token"
          : state === "stranded"
            ? "it is not polling and is holding nothing, so mail sent to it will not be read"
            : MEMBER_STATE_LABEL[state]
      }
      className={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium",
        tone[state],
      )}
    >
      {state === "starting" && <Loader2 className="size-2.5 animate-spin" aria-hidden />}
      {state === "failed" && <AlertTriangle className="size-2.5" aria-hidden />}
      {MEMBER_STATE_LABEL[state]}
    </span>
  );
}

/** loopStatus resolves the board to exactly ONE sentence. Waiting-on-you wins
 *  over everything: it is the only state that needs the engineer, and a screen
 *  that shows four true things at once does not tell him which one to act on. */
export function loopStatus(
  loop: Loop,
  lanes: LaneState[],
  stepActor: string,
): { tone: "you" | "running" | "stalled" | "stranded" | "ended"; text: string } {
  if (loop.status !== "active") {
    return { tone: "ended", text: `This loop has ended${loop.endReason ? `: ${loop.endReason}` : "."}` };
  }
  const awaitingEngineer = lanes.some((l) => l.role === "ENGINEER" && (l.awaited || l.holding));
  if (awaitingEngineer) {
    return { tone: "you", text: "Waiting on you. The play cannot advance until you reply." };
  }
  // Stranded outranks a stall. A stalled brief is being held by something;
  // stranded means nothing is reading at all, which is the failure that made
  // a loop sit silent for fifty minutes while the board said "Running".
  const stranded = lanes.filter((l) => l.state === "stranded");
  if (stranded.length > 0) {
    const names = stranded.map((l) => l.role).join(", ");
    return {
      tone: "stranded",
      text:
        stranded.length === 1
          ? `${names} is not reading its mail. Nothing will move until it does.`
          : `${names} are not reading their mail. Nothing will move until they do.`,
    };
  }
  const dead = lanes.filter((l) => l.state === "failed");
  if (dead.length > 0) {
    return {
      tone: "stranded",
      text: `${dead.map((l) => l.role).join(", ")} did not start, so this loop is short-staffed.`,
    };
  }
  const stalled = lanes.reduce((n, l) => n + l.stalledBriefs, 0);
  if (stalled > 0) {
    return {
      tone: "stalled",
      text: `${stalled} brief${stalled === 1 ? "" : "s"} handed out and never acknowledged.`,
    };
  }
  const holder = lanes.find((l) => l.holding)?.role || stepActor;
  if (loop.stepId) {
    return {
      tone: "running",
      text: holder
        ? `Running: ${holder} holds ${loop.stepId}, round ${loop.round}.`
        : `Running: ${loop.stepId}, round ${loop.round}.`,
    };
  }
  return { tone: "running", text: "Running." };
}

function LoopStatusLine({
  loop,
  lanes,
  stepActor,
}: {
  loop: Loop;
  lanes: LaneState[];
  stepActor: string;
}) {
  const { tone, text } = loopStatus(loop, lanes, stepActor);
  return (
    <p
      role="status"
      aria-label="loop status"
      data-tone={tone}
      className={cn(
        "rounded-md border px-3 py-2 text-sm",
        tone === "you" && "border-primary/50 bg-primary/10 text-primary font-medium",
        tone === "stalled" && "border-warning/45 bg-warning/12 text-warning",
        tone === "stranded" && "border-destructive/45 bg-destructive/10 text-destructive font-medium",
        tone === "ended" && "border-border/60 bg-surface-sunken/50 text-muted-foreground",
        tone === "running" && "border-border/60 bg-surface-sunken/50 text-muted-foreground",
      )}
    >
      {text}
    </p>
  );
}
