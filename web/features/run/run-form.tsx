"use client";

// RunForm — the "Run agent" composer: a REQUIRED session name (so every new
// session is identifiable in the list from second one), an optional first
// prompt, engine, cwd, and model. That's the whole form — tools/effort/
// permission knobs are gone: every run is the full-autonomy interactive
// TUI now.
//
// Engine selector first: the engine is a property of the
// session, but the chosen engine determines which model/agent pickers
// actually list anything. Today only claude and opencode differ; future
// engines plug in via /api/v1/agentd/engines.
import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { Play, Loader2, Pencil } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Field, SectionHeader } from "@/components/ui/surface";
import { CwdCombobox } from "./cwd-combobox";
import { cn } from "@/lib/utils";
import { PromptEditor } from "@/features/prompts/prompt-editor";
import { getPrompts, standingIsEdited } from "@/lib/prompts";
import { spawnRun, AgentdError, listEngines } from "@/lib/agentd";
import { sessionPath } from "@/lib/paths";
import {
  engineMeta,
  normaliseEngine,
  ENGINE_IDS,
  type EngineId,
} from "@/lib/engine";

// Last-used working directory, remembered per device (localStorage) so the
// next spawn preselects it.
const LAST_CWD_KEY = "agentflow.run.lastCwd";
const LAST_ENGINE_KEY = "agentflow.run.lastEngine";
const LAST_PROMPT_MODE_KEY = "agentflow.run.lastPromptMode";

type PromptMode = "standing" | "scratch";

function readLastPromptMode(): PromptMode {
  if (typeof window === "undefined") return "standing";
  try {
    return window.localStorage.getItem(LAST_PROMPT_MODE_KEY) === "scratch"
      ? "scratch"
      : "standing";
  } catch {
    return "standing";
  }
}

function readLastCwd(): string {
  if (typeof window === "undefined") return "";
  return window.localStorage.getItem(LAST_CWD_KEY) ?? "";
}

function readLastEngine(): EngineId {
  if (typeof window === "undefined") return "claude";
  const v = window.localStorage.getItem(LAST_ENGINE_KEY);
  return normaliseEngine(v);
}

export function RunForm({
  initialPrompt,
  resumeSessionId,
}: {
  initialPrompt?: string;
  resumeSessionId?: string;
}) {
  const router = useRouter();
  const [name, setName] = useState("");
  const [prompt, setPrompt] = useState(initialPrompt ?? "");
  const [model, setModel] = useState("");
  const [agent, setAgent] = useState("");
  const [engine, setEngine] = useState<EngineId>(readLastEngine());
  const [promptMode, setPromptMode] =
    useState<PromptMode>(readLastPromptMode());
  const [editingPrompts, setEditingPrompts] = useState(false);
  // An edit to the solo block or to a rule it names shapes every session he
  // starts, and nothing reminded him. Discoverability of your own past
  // decisions is the difference between a feature and a trap.
  const prompts = useQuery({
    queryKey: ["prompts"],
    queryFn: getPrompts,
    staleTime: 30_000,
  });
  const edited = standingIsEdited(
    prompts.data?.overrides ?? [],
    prompts.data?.solo.Rules ?? [],
  );
  const [cwd, setCwd] = useState(readLastCwd());
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Available engines. Filters out engines whose binary is not
  // installed — agentd only registers OpenCode when it can find the
  // binary, so a missing engine simply does not show up.
  const enginesQ = useQuery({
    queryKey: ["engines"],
    queryFn: () => listEngines(),
    staleTime: 5 * 60_000,
  });
  const available = new Set(
    (
      enginesQ.data?.engines ?? ENGINE_IDS.map((id) => ({ id, version: "" }))
    ).map((e) => normaliseEngine(e.id)),
  );

  // Persist the chosen engine so the next spawn preselects it (last-used,
  // not per-project).
  useEffect(() => {
    if (typeof window !== "undefined") {
      window.localStorage.setItem(LAST_ENGINE_KEY, engine);
    }
  }, [engine]);

  // Remembered per device, like the engine and the cwd.
  useEffect(() => {
    if (typeof window !== "undefined") {
      try {
        window.localStorage.setItem(LAST_PROMPT_MODE_KEY, promptMode);
      } catch {
        // storage blocked is not a reason to fail a spawn
      }
    }
  }, [promptMode]);

  const submit = async () => {
    if (busy || !name.trim() || (!resumeSessionId && !cwd.trim())) return;
    setBusy(true);
    setError(null);
    try {
      const run = await spawnRun({
        // every run is the interactive TUI — the run view drives it as a
        // terminal, and the name becomes the session's title in the list
        kind: "tty",
        engine,
        title: name.trim(),
        prompt: prompt.trim(),
        model: model || undefined,
        agent: agent || undefined,
        cwd: cwd.trim() || undefined,
        promptMode,
        resumeSessionId,
      });
      if (cwd.trim() && typeof window !== "undefined") {
        window.localStorage.setItem(LAST_CWD_KEY, cwd.trim());
      }
      router.push(sessionPath(run.id));
    } catch (e) {
      setError(
        e instanceof AgentdError ? e.message : "Failed to start the run",
      );
      setBusy(false);
    }
  };

  const engineChoice = ENGINE_IDS.filter((id) => available.has(id));

  return (
    <div className="flex flex-col gap-4">
      <SectionHeader
        eyebrow={resumeSessionId ? "Continue session" : "New session"}
        title={
          resumeSessionId ? "Resume with intent" : "Start a terminal session"
        }
        description="Pick an engine, name it, pick a folder, hit go — the interactive TUI opens either way."
      />

      <Field
        label="Engine"
        hint="The interactive TUI to drive. Claude today; OpenCode when available."
      >
        <div className="border-border/60 flex flex-wrap gap-2 rounded-2xl border p-1.5">
          {engineChoice.map((id) => {
            const meta = engineMeta(id);
            const active = id === engine;
            return (
              <button
                key={id}
                type="button"
                onClick={() => setEngine(id)}
                aria-pressed={active}
                className={cn(
                  "flex flex-1 items-center justify-center gap-2 rounded-xl border border-transparent px-3 py-2 text-[14px] transition-colors",
                  active
                    ? "af-selected text-foreground font-semibold"
                    : "text-muted-foreground hover:bg-accent/40 font-medium",
                )}
              >
                <span>{meta.displayName}</span>
              </button>
            );
          })}
        </div>
      </Field>
      <div>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => setEditingPrompts((v) => !v)}
        >
          <Pencil className="size-3" />{" "}
          {editingPrompts ? "Close the prompts" : "Edit these rules"}
        </Button>
      </div>
      {editingPrompts && (
        <div className="af-panel max-h-[70dvh] overflow-y-auto rounded-lg p-3">
          <PromptEditor
            scope={{ kind: "solo" }}
            onClose={() => setEditingPrompts(false)}
          />
        </div>
      )}

      <Field
        label="Session name"
        hint="Required — this is how the session shows up in your list."
      >
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              void submit();
            }
          }}
          placeholder="e.g. Fix the login bug"
          className="h-12 text-[15px]"
          aria-label="Session name"
        />
      </Field>

      <Field
        label="Prompt"
        hint="Optional — type directly in the terminal instead if you prefer."
      >
        <Textarea
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          rows={3}
          placeholder={
            resumeSessionId
              ? "Where should the engine continue from here? (optional)"
              : "Optional first prompt for the terminal"
          }
          className="min-h-24 text-[15px] leading-6"
        />
      </Field>

      <div className="grid grid-cols-1 gap-2 sm:grid-cols-12">
        <Field label="cwd" className="sm:col-span-6">
          <CwdCombobox value={cwd} onChange={setCwd} />
        </Field>
        <Field label="Model" className="sm:col-span-3">
          <Input
            value={model}
            onChange={(e) => setModel(e.target.value)}
            placeholder="default"
            aria-label="Model"
            className="h-10"
          />
        </Field>
        <Field label="Agent" className="sm:col-span-3">
          <Input
            value={agent}
            onChange={(e) => setAgent(e.target.value)}
            placeholder="default"
            aria-label="Agent"
            className="h-10"
          />
        </Field>
      </div>

      {/* Not on/off. Scratch drops the advisory rules and keeps the two that
          are the only control of their kind, because a session spawns with
          permissions bypassed and those sentences are all that stands between
          a throwaway and a push to main. */}
      <Field
        label={edited ? "Standing rules, edited" : "Standing rules"}
        hint={
          promptMode === "scratch"
            ? "Scratch: the reporting contract and the house rules are off. Git and deploys stay locked; that part is not a default, it is a floor."
            : "The full set: how you work, what a report has to contain, and what the agent may never do on its own."
        }
      >
        <div className="border-border/70 flex w-fit gap-1 rounded-2xl border p-1">
          {(["standing", "scratch"] as const).map((mode) => (
            <button
              key={mode}
              type="button"
              aria-pressed={promptMode === mode}
              aria-label={mode === "standing" ? "Standing rules" : "Scratch"}
              onClick={() => setPromptMode(mode)}
              className={cn(
                "rounded-xl px-3 py-1.5 text-sm font-medium transition-colors",
                promptMode === mode
                  ? "af-selected text-primary font-semibold"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {mode === "standing" ? "Standing" : "Scratch"}
            </button>
          ))}
        </div>
      </Field>

      {error && <p className="text-destructive text-sm">{error}</p>}

      <div
        className="mt-1 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-end"
        style={{ paddingBottom: "env(safe-area-inset-bottom)" }}
      >
        {resumeSessionId && (
          <span className="text-muted-foreground min-w-0 text-xs sm:mr-auto">
            resuming session{" "}
            <span className="font-mono">{resumeSessionId.slice(0, 8)}</span>{" "}
            with <code>--resume</code>
          </span>
        )}
        <Button
          size="xl"
          className="w-full text-[15px] sm:w-auto"
          onClick={() => void submit()}
          disabled={busy || !name.trim() || (!resumeSessionId && !cwd.trim())}
        >
          {busy ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <Play className="size-4" />
          )}
          Start run
        </Button>
      </div>
    </div>
  );
}
