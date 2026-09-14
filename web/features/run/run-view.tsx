"use client";

// RunView — the live run screen. Every run is driven through the real
// interactive TUI in a PTY, and the TUI is the whole screen: there is no
// second reading of a session and nothing to choose between.
//
// This page briefly carried a Chat tab that rendered the transcript as
// message cards. It is gone, along with the remembered per-device choice
// between the two. The terminal already shows everything the cards showed,
// in the form the agent actually produced it.
import { useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { ArrowLeft, Info, Loader2, Sliders } from "lucide-react";
import { runStatus } from "@/lib/agentd";
import { runStateLabel, isLiveState } from "@/lib/run";
import { TerminalPane, type TerminalHandle } from "./terminal-pane";
import { SessionDrawer } from "./session-drawer";
import { RestartBanner } from "./restart-banner";
import { ControlSheet } from "./control-sheet";
import { PermissionsOverlay } from "./permissions-overlay";
import { ComposerV2, type ComposerHandle } from "./composer-v2";
import {
  getEngineCaps,
  listEngineCommands,
  sendEnginePrompt,
} from "@/lib/agentd";

// runHeaderTitle names a run on its own page.
//
// The name a person chose (or the transcript knows) first, then the prompt,
// then the project. The run key is the last resort, never the first.
export function runHeaderTitle(run: {
  title?: string;
  prompt?: string;
  project?: string;
  cwd?: string;
  id: string;
}): string {
  const first = [run.title, run.prompt, run.project].find(
    (v) => (v ?? "").trim() !== "",
  );
  return (first ?? "").trim() || `run ${run.id.slice(0, 8)}`;
}

export function RunView({
  runId,
  backHref,
}: {
  runId: string;
  backHref?: string;
}) {
  const { data: run } = useQuery({
    queryKey: ["run-status", runId],
    queryFn: () => runStatus(runId),
    refetchInterval: (query) => {
      const s = query.state.data?.state;
      return isLiveState(s ?? "") ? 2500 : 15000;
    },
  });

  // the header's single button: opens the full session-detail slider (stop /
  // terminate + connection refresh live in there now, not in the header)
  const [drawerOpen, setDrawerOpen] = useState(false);
  // native control sheet (model picker and engine controls).
  const [controlOpen, setControlOpen] = useState(false);

  const ttyRef = useRef<TerminalHandle>(null);
  const composerRef = useRef<ComposerHandle>(null);

  // Full-screen terminal: the composer is hidden behind a floating keyboard
  // button on touch devices (the TUI owns the whole screen; tapping it never
  // pops the OS keyboard). Fine-pointer devices keep direct terminal typing
  // and start with the composer open.
  const isTouch =
    typeof window !== "undefined" &&
    window.matchMedia?.("(pointer: coarse)").matches === true;
  const [composerOpen, setComposerOpen] = useState(!isTouch);

  // Engine caps drive the permission overlay's polling — once we know the
  // engine does not surface permissions natively (claude) we skip the
  // round trip.
  const capsQ = useQuery({
    queryKey: ["engine-caps", runId],
    queryFn: () => getEngineCaps(runId),
    staleTime: 60_000,
  });
  // slash commands — composer v2 reads this for the / autocomplete
  const commandsQ = useQuery({
    queryKey: ["engine-commands", runId],
    queryFn: () => listEngineCommands(runId),
    // Gated on the capability, not on the engine id. Claude serves a
    // catalog now (its builtins plus this machine's command and skill
    // files), and hard-coding "opencode" here kept the composer's slash
    // autocomplete dark for it no matter what the backend answered.
    enabled: composerOpen && capsQ.data?.caps?.listCommands === true,
    staleTime: 5 * 60_000,
  });
  const openComposer = (open: boolean) => {
    setComposerOpen(open);
  };

  // Tapping the TUI's own prompt box is how the composer opens on a phone.
  // The focus() has to happen HERE, synchronously inside the terminal's
  // touchend handler, or iOS never pops the keyboard — which is also why
  // the composer stays mounted on touch devices rather than being mounted
  // by this state change.
  const requestKeyboard = () => {
    setComposerOpen(true);
    composerRef.current?.focus();
  };

  // NOTE: we deliberately do NOT gate the whole screen on `run`. Returning a
  // spinner here meant the TerminalPane did not mount until the status query
  // resolved, so on a phone connection the PTY dial was delayed by a full
  // round-trip and the terminal mounted into a container that was still
  // settling. The terminal only needs runId — mount it immediately and let
  // the header fill in.
  // A loop member is named after its loop and its role. Its prompt column is
  // empty (StartTTY builds the Session without one) and its cwd is the loop
  // cwd, identical for every member, so the old header read "Terminal session"
  // over "/home/you/code/thing" for all four members of a loop.
  const member = run?.loop;
  // The name the session actually has, from the same transcript records the
  // sessions list reads (custom-title > ai-title > summary > first prompt). Before this the
  // header read "resumed 290d8…" — the run key, which is the one thing that
  // is never the name anyone gave it.
  const title = !run
    ? "Terminal session"
    : member
      ? `${run.title || member.title || member.task || "Loop"} · ${member.role}`
      : runHeaderTitle(run);
  const metaBits = member
    ? [
        member.play
          ? `${member.play}${member.stepId ? ` · ${member.stepId}` : ""}`
          : undefined,
        run?.model || undefined,
        run && run.eventCount > 0 ? `${run.eventCount} events` : undefined,
      ].filter(Boolean)
    : run
      ? [
          run.model,
          run.cwd || undefined,
          run.eventCount > 0 ? `${run.eventCount} events` : undefined,
        ].filter(Boolean)
      : [];

  return (
    <div className="bg-background relative flex h-full min-h-0 flex-col">
      {/* header: back · state chip + title · details slider */}
      <header className="border-border/50 bg-background/85 z-20 border-b backdrop-blur-xl">
        <div className="mx-auto flex h-14 w-full max-w-3xl items-center gap-1.5 px-2 sm:px-3">
          <Link
            href={member ? `/loop/board/?id=${encodeURIComponent(member.id)}` : (backHref ?? "/")}
            aria-label={member ? "Back to the loop" : "Back to sessions"}
            className="text-muted-foreground hover:bg-accent/60 hover:text-foreground grid size-10 shrink-0 place-items-center rounded-full transition-colors"
          >
            <ArrowLeft className="size-5" />
          </Link>
          <div className="min-w-0 flex-1 leading-tight">
            <div className="flex min-w-0 items-center gap-2">
              {!run && (
                <Loader2
                  className="text-muted-foreground size-3 shrink-0 animate-spin"
                  aria-label="loading"
                />
              )}
              <span className="truncate text-sm font-medium">{title}</span>
              {/* A live session needs no badge saying it is alive — you are
                  looking at it. A dead one still has to be distinguishable,
                  so it gets one quiet word rather than a coloured pill. The
                  details drawer keeps the full state either way. */}
              {run && !isLiveState(run.state) && (
                <span className="text-muted-foreground shrink-0 text-[11px]">
                  {run.blocked ? "blocked" : runStateLabel(run.state)}
                </span>
              )}
              {run && isLiveState(run.state) && run.blocked && (
                <span className="text-muted-foreground shrink-0 text-[11px]">
                  blocked
                </span>
              )}
            </div>
            {metaBits.length > 0 && (
              <p className="text-muted-foreground truncate text-[11px]">
                {metaBits.join(" · ")}
              </p>
            )}
          </div>
          <div className="flex shrink-0 items-center gap-1">
            {/* No keyboard button here. On touch the composer opens by
                tapping the TUI's own prompt rows (TerminalPane
                onRequestKeyboard); the header stays for control + details. */}
            {/* control sheet trigger — opens the model picker (and, in
                subsequent slices, the rest of the picker sections). */}
            <button
              type="button"
              onClick={() => setControlOpen(true)}
              disabled={!run}
              aria-label="Control sheet"
              title="Control — model, agent, providers, MCP…"
              className="text-muted-foreground hover:bg-accent/60 hover:text-foreground grid size-10 place-items-center rounded-full transition-colors"
            >
              <Sliders className="size-[18px]" />
            </button>
            <button
              type="button"
              onClick={() => setDrawerOpen(true)}
              disabled={!run}
              aria-label="Session details"
              title="Session details, actions and connection refresh"
              className="text-muted-foreground hover:bg-accent/60 hover:text-foreground grid size-10 place-items-center rounded-full transition-colors"
            >
              <Info className="size-[18px]" />
            </button>
          </div>
        </div>
      </header>

      {/* full session detail + stop/terminate + connection refresh */}
      {drawerOpen && run && (
        <SessionDrawer
          run={run}
          open
          onClose={() => setDrawerOpen(false)}
          ttyRef={ttyRef}
        />
      )}

      {/* native control sheet (model picker and engine controls) */}
      {controlOpen && run && (
        <ControlSheet
          runId={runId}
          open={controlOpen}
          initialModel={run.model}
          onClose={() => setControlOpen(false)}
        />
      )}

      {/* native permission + question overlay — always mounted; gates
          itself on caps so engines without native prompts pay no cost. */}
      {run && <PermissionsOverlay runId={runId} caps={capsQ.data?.caps} />}

      {run && !isLiveState(run.state) && (
        <RestartBanner
          runId={runId}
          state={run.state}
          onRestarted={() => ttyRef.current?.reconnect()}
        />
      )}

      {/* terminal — takes everything below the header. Tapping the TUI never
          opens the OS keyboard while the composer is closed. */}
      <div className="flex min-h-0 flex-1 flex-col">
        <TerminalPane
          ref={ttyRef}
          runId={runId}
          keyboardEnabled={composerOpen || !isTouch}
          onRequestKeyboard={isTouch ? requestKeyboard : undefined}
        />
      </div>

      {/* Composer: slash autocomplete,
          attach surface, multi-line editor, send-vs-newline that respects
          mobile keyboards.
          Mounted always, shown when open: hidden it takes no layout space,
          so a closed composer still leaves the terminal full height, and its
          textarea exists to be focused inside the tap gesture that opens
          it. */}
      {(composerOpen || isTouch) && (
        <ComposerV2
          ref={composerRef}
          visible={composerOpen}
          isTouch={isTouch}
          runId={runId}
          sendInput={(seq, opts) =>
            ttyRef.current?.sendInput(seq, opts) ?? undefined
          }
          submitInput={(text) => ttyRef.current?.submitInput(text) ?? undefined}
          commands={commandsQ.data?.commands ?? []}
          commandsLoading={commandsQ.isFetching}
          onSendWithAttachments={async (text, attachmentIds) => {
            await sendEnginePrompt(runId, text, attachmentIds);
          }}
          onHide={() => openComposer(false)}
        />
      )}
    </div>
  );
}

// --- shared renderer helpers ------------------------------------------------------

export function runStateBadge(
  state: string,
): "live" | "secondary" | "destructive" | "outline" {
  switch (state) {
    case "running":
    case "starting":
      return "live";
    case "awaiting":
      return "secondary";
    case "finished":
    case "stopped":
      return "outline";
    case "crashed":
      return "destructive";
    default:
      return "secondary";
  }
}
