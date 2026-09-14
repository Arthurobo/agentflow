"use client";

// wizard.tsx — name the loop, pick a play, pick an engine and model per role.
//
// Every option is rendered from the backend registries, never a hardcoded
// list, and an UNPROBED engine says so where the model is chosen: picking a
// model believing it was checked is the failure this screen exists to avoid.
//
// A registry that fails to load says so. It used to render as an empty grid,
// which is indistinguishable from "there are no plays" and is why a working
// play picker looked like a missing one.

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  AlertCircle,
  AlertTriangle,
  ChevronRight,
  Copy,
  KeyRound,
  ListTree,
  Pencil,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { engineMeta } from "@/lib/engine";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { instructionProblem } from "@/lib/instruction";
import { Picker, type PickerItem } from "@/components/ui/picker";
import { CodePreview, EmptyState, Field, FieldGroup, SectionHeader } from "@/components/ui/surface";
import { CwdCombobox } from "@/features/run/cwd-combobox";
import { PromptEditor } from "@/features/prompts/prompt-editor";
import { PlayCard } from "./play-card";
import { getPrompts, playIsEdited } from "@/lib/prompts";
import {
  createLoop,
  listPlays,
  listToolModels,
  listTools,
  LoopError,
  type CreateResult,
  type Handoff,
  type MemberDraft,
  type Play,
  type ToolSpec,
} from "@/lib/loop";

const ENGINEER = "ENGINEER";

// DEFAULT_PLAY is what a first-time engineer lands on: make a change and have
// the diff reviewed against its brief. Overridden by whatever he used last.
const DEFAULT_PLAY = "build";

// DEFAULT_MODEL is the picker id standing for "no model override". The wire
// value is the empty string; an empty id would collide with the picker's
// keying, so the sentinel only exists between the picker and this file.
const DEFAULT_MODEL = "__default__";

// Preferences remembered per device. The cwd key is the run form's, on
// purpose: a loop and a run both mean "this folder", and the engineer should
// not have to teach the app the same path twice.
const LAST_CWD_KEY = "agentflow.run.lastCwd";
const LAST_PLAY_KEY = "agentflow.loop.lastPlay";
const LAST_CREW_KEY = "agentflow.loop.lastCrew";

function readLocal(key: string): string {
  if (typeof window === "undefined") return "";
  try {
    return window.localStorage.getItem(key) ?? "";
  } catch {
    return "";
  }
}

function writeLocal(key: string, value: string) {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(key, value);
  } catch {
    // a private window with storage blocked is not a reason to fail a create
  }
}

// readLastCrew returns the per-role engine and model the engineer picked last
// time. Kept as a pair: remembering the engine alone would silently pair a
// remembered engine with a model the engineer never chose.
export function readLastCrew(raw: string): Record<string, MemberDraft> {
  if (!raw) return {};
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    const out: Record<string, MemberDraft> = {};
    for (const [role, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (!v || typeof v !== "object") continue;
      const d = v as { tool?: unknown; model?: unknown };
      out[role] = {
        role,
        tool: typeof d.tool === "string" ? d.tool : "claude",
        model: typeof d.model === "string" ? d.model : "",
      };
    }
    return out;
  } catch {
    return {};
  }
}

type PickerTarget = { role: string; kind: "engine" | "model" };

export function LoopWizard({ onCreated }: { onCreated?: (id: string) => void }) {
  const plays = useQuery({ queryKey: ["loop", "plays"], queryFn: listPlays });
  const tools = useQuery({ queryKey: ["loop", "tools"], queryFn: listTools });

  const [title, setTitle] = useState("");
  const [task, setTask] = useState("");
  const [cwd, setCwd] = useState(() => readLocal(LAST_CWD_KEY));
  // Last used wins; otherwise a sensible default rather than nothing chosen.
  // build does work and has it reviewed in one pass, which is the common case.
  const [playName, setPlayName] = useState(() => readLocal(LAST_PLAY_KEY) || DEFAULT_PLAY);
  const [drafts, setDrafts] = useState<Record<string, MemberDraft>>({});
  const [picker, setPicker] = useState<PickerTarget | null>(null);
  // Editing is reached from the card he is standing on when he thinks "this
  // step should ask for something different", not from a settings section.
  const [editing, setEditing] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<CreateResult | null>(null);

  const remembered = useMemo(() => readLastCrew(readLocal(LAST_CREW_KEY)), []);
  // An edit made three weeks ago silently shapes every loop he starts, so the
  // card says so.
  const prompts = useQuery({ queryKey: ["prompts"], queryFn: getPrompts, staleTime: 30_000 });
  const overrides = prompts.data?.overrides ?? [];
  const dropped = prompts.data?.dropped ?? {};

  const play = useMemo(
    () => plays.data?.find((p) => p.name === playName),
    [plays.data, playName],
  );

  // The roles come from the play, so the crew is DERIVED from it rather than
  // synced into state by an effect: changing the play restaffs the form with
  // no render cascade, and a role the engineer already edited keeps its draft.
  const crew = useMemo<MemberDraft[]>(
    () =>
      play
        ? rolesOf(play).map(
            (role) =>
              drafts[role] ??
              remembered[role] ?? { role, tool: "claude", model: "" },
          )
        : [],
    [play, drafts, remembered],
  );

  // Same rule in one place, read by the guard and by the button's disabled
  // state, so the button is never clickable into an error the form already
  // knows about.
  // The instruction floor is mirrored from the server so the button explains
  // itself rather than submitting into a 400.
  const taskProblem = instructionProblem(title, task);
  const ready = !taskProblem && !!play;
  // ENGINEER is an address, not a process, so it is never one of the agents
  // this button is about to start.
  const spawnCount = crew.filter((c) => c.role !== ENGINEER).length;

  const target = picker ? crew.find((c) => c.role === picker.role) : undefined;
  const targetSpec = target
    ? (tools.data ?? []).find((t) => t.id === (target.tool || "claude"))
    : undefined;

  // An unprobed engine's model set is a property of THIS install, so it is
  // asked for rather than assumed. Only while the picker is actually open:
  // answering it may mean starting a throwaway process.
  const unprobedEngine = targetSpec && !targetSpec.probed ? targetSpec.id : "";
  // The catalogue is needed to RENDER the rows, not only to fill a picker: a
  // row cannot know whether to offer a list or a text box until the machine
  // has answered. agentd caches the answer, so asking on open costs one probe
  // for the whole wizard rather than one per picker.
  const crewEngine = crew
    .map((c) => (tools.data ?? []).find((t) => t.id === (c.tool || "claude")))
    .find((t) => t && !t.probed)?.id;
  const askEngine = unprobedEngine || crewEngine || "";
  // Declared above the early return below: a hook that only runs on some
  // renders is a hook-count mismatch, and React tears the tree down with
  // "rendered fewer hooks than expected".
  const liveModels = useQuery({
    queryKey: ["loop", "tool-models", askEngine, cwd],
    queryFn: () => listToolModels(askEngine, cwd),
    enabled: askEngine !== "",
    staleTime: 5 * 60 * 1000,
  });
  const liveCatalogue = liveModels.data?.models ?? [];

  if (created) {
    return <Handoffs result={created} onDone={() => onCreated?.(created.loop.id)} />;
  }

  async function submit() {
    setError("");
    if (!ready || !play) return;
    setBusy(true);
    try {
      const result = await createLoop({
        title: title.trim(),
        task: task.trim(),
        cwd: cwd.trim(),
        play: play.name,
        crew,
      });
      if (cwd.trim()) writeLocal(LAST_CWD_KEY, cwd.trim());
      writeLocal(LAST_PLAY_KEY, play.name);
      writeLocal(
        LAST_CREW_KEY,
        JSON.stringify(
          Object.fromEntries(
            crew
              .filter((c) => c.role !== ENGINEER)
              .map((c) => [c.role, { tool: c.tool ?? "claude", model: c.model ?? "" }]),
          ),
        ),
      );
      // Straight to the board. The handoff screen exists to let him copy
      // prompts and tokens by hand, and when the crew was spawned there is
      // nothing to copy.
      //
      // The board rather than a terminal, because at this instant no member
      // has a process: the rows exist and the spawns are in flight. The board
      // polls while anything is starting and turns each row into a link to
      // its terminal the moment there is one, so it is the only destination
      // that is correct in the first second AND in the tenth.
      //
      // Two conditions, not one. `spawning` is the deployment's own answer,
      // and the second covers a response that does not carry it: if no handoff
      // has a token and a prompt, the screen would be empty anyway.
      const pasteable = result.handoffs.filter((h) => h.token && h.prompt);
      if (result.spawning || pasteable.length === 0) {
        onCreated?.(result.loop.id);
        return;
      }
      setCreated(result);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  function pick(item: PickerItem) {
    if (!picker || !target) return;
    if (picker.kind === "engine") {
      // A new engine invalidates the model: the lists do not overlap.
      setDrafts((prev) => ({
        ...prev,
        [picker.role]: { role: picker.role, tool: item.id, model: "" },
      }));
    } else {
      setDrafts((prev) => ({
        ...prev,
        [picker.role]: {
          ...target,
          role: picker.role,
          model: item.id === DEFAULT_MODEL ? "" : item.id,
        },
      }));
    }
    setPicker(null);
  }

  return (
    <div className="flex flex-col gap-5">
      <SectionHeader
        eyebrow="New loop"
        title="Start a loop"
        description="The engine owns the sequence. Pick the play and who runs it."
      />

      {/* Two fields, because the product needs both and one field could only
          ever be one of them. The name is what the list, the board header and
          every member's session name read; the instruction is what the
          orchestrator is sent as its first message. When they were one field
          the short name won every surface and the orchestrator was told
          nothing. */}
      <Field label="Name this loop" hint="A short label. It names the loop everywhere, including each member's terminal.">
        <Input
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder="Flaky guest export"
        />
      </Field>

      <Field
        label="What should this loop do"
        hint="Write it as you would say it to the orchestrator. It is delivered as its first message, before the play's own brief."
      >
        <Textarea
          value={task}
          onChange={(e) => setTask(e.target.value)}
          autoResize
          maxRows={8}
          placeholder="Find why the guest export goes flaky above 500 rows and fix it. Do not change the response shape."
        />
      </Field>
      {task.trim().length > 0 && taskProblem && (
        <p role="status" className="text-warning -mt-3 text-xs">
          {taskProblem}
        </p>
      )}

      <Field
        label="Working directory"
        hint="Where the members work. Suggestions are folders this machine has run sessions in; any other path can be typed."
      >
        <CwdCombobox value={cwd} onChange={setCwd} placeholder="/home/you/code/project" />
      </Field>

      <FieldGroup
        label="Play"
        hint="The sequence the engine enforces. A message that does not belong to the current step is refused."
      >
        {plays.isError ? (
          <QueryError what="the plays" error={plays.error} onRetry={() => void plays.refetch()} />
        ) : plays.isLoading ? (
          <EmptyState loading title="Loading the plays" />
        ) : (plays.data ?? []).length === 0 ? (
          <p className="text-muted-foreground text-xs">The engine reports no plays.</p>
        ) : (
          <div className="grid gap-2 sm:grid-cols-2">
            {(plays.data ?? []).map((p) => (
              <PlayCard
                key={p.name}
                play={p}
                selected={p.name === playName}
                edited={playIsEdited(overrides, p.name)}
                onSelect={() => setPlayName(p.name)}
              />
            ))}
          </div>
        )}
        {/* A play dropped at load time is a disabled card with its reason, not
            an absence: an engineer whose play vanished will not guess why. */}
        {Object.entries(dropped).map(([name, reason]) => (
          <div
            key={name}
            aria-disabled
            className="border-destructive/40 bg-destructive/5 mt-2 rounded-lg border p-3 text-left"
          >
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-semibold">{name}</span>
              <Badge variant="destructive">not offered</Badge>
              <Button
                size="sm"
                variant="outline"
                className="ml-auto"
                onClick={() => setEditing(name)}
              >
                <Pencil className="size-3" /> Fix it
              </Button>
            </div>
            <p className="text-destructive mt-1 text-xs leading-5">{reason}</p>
          </div>
        ))}
      </FieldGroup>

      {play && (
        <div>
          <Button size="sm" variant="outline" onClick={() => setEditing(editing ? "" : play.name)}>
            <Pencil className="size-3" /> {editing ? "Close the prompts" : "Edit this play's prompts"}
          </Button>
        </div>
      )}
      {editing && (
        <div className="af-panel rounded-lg p-3">
          <PromptEditor scope={{ kind: "play", play: editing }} onClose={() => setEditing("")} />
        </div>
      )}

      {play && (
        <FieldGroup
          label={`Who runs ${play.title}`}
          hint="One agent per role. Each is launched with its own brief when you create the loop."
        >
          {tools.isError ? (
            <QueryError what="the engines" error={tools.error} onRetry={() => void tools.refetch()} />
          ) : (
            // Tied to the card above by the same accent, so it reads as the
            // roles this play needs rather than a detached list.
            <div className="border-primary/40 flex flex-col gap-3 border-l-2 pl-3">
              {crew.map((draft) => (
                <RoleRow
                  key={draft.role}
                  role={draft.role}
                  tools={tools.data ?? []}
                  loading={tools.isLoading}
                  draft={draft}
                  onOpenPicker={(kind) => setPicker({ role: draft.role, kind })}
                  onChange={(d) => setDrafts((prev) => ({ ...prev, [draft.role]: d }))}
                  catalogue={
                    (draft.tool || "claude") === askEngine && !liveModels.isLoading
                      ? liveCatalogue.length
                      : undefined
                  }
                />
              ))}
              <p className="text-muted-foreground text-xs leading-5">
                You are in this loop too, as ENGINEER. You run on nothing and there is nothing to
                set up: members write to you and you type back on the loop&apos;s own screen.
              </p>
            </div>
          )}
        </FieldGroup>
      )}

      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      <div>
        {/* The last moment before real processes start on his machine, so it
            names the play and how many of them there will be. */}
        <Button onClick={submit} disabled={busy || !ready} size="xl">
          {busy
            ? "Creating…"
            : play
              ? `Create loop: ${play.title}, starting ${spawnCount} ${spawnCount === 1 ? "agent" : "agents"}`
              : "Create loop"}
        </Button>
      </div>

      {picker && target && (
        <Picker
          open
          title={picker.kind === "engine" ? `${picker.role} engine` : `${picker.role} model`}
          items={
            picker.kind === "engine"
              ? engineItems(tools.data ?? [], target.tool || "claude")
              : unprobedEngine
                ? liveItems(liveCatalogue, target.model ?? "", targetSpec)
                : modelItems(targetSpec, target.model ?? "")
          }
          loading={tools.isLoading || (picker.kind === "model" && liveModels.isLoading)}
          note={
            picker.kind === "model" && unprobedEngine
              ? liveModels.data?.note ||
                `${targetSpec?.title} was asked what it runs and named nothing. Type the model instead.`
              : undefined
          }
          onClose={() => setPicker(null)}
          onPick={pick}
        />
      )}
    </div>
  );
}

export function engineItems(tools: ToolSpec[], current: string): PickerItem[] {
  return tools.map((t) => ({
    id: t.id,
    label: t.title,
    description: t.probed
      ? `${t.models.length} models, checked against the real thing`
      : "model list never checked",
    current: t.id === current,
  }));
}

// liveItems renders what the tool itself reported. agentd already dropped
// everything but id, provider, name and status: opencode's own listing carries
// a live provider credential on every entry.
export function liveItems(
  models: { id: string; displayName?: string; provider?: string; status?: string }[],
  current: string,
  spec?: ToolSpec,
): PickerItem[] {
  const items: PickerItem[] = [];
  // "Let the engine choose" is a real answer and used to be missing, which is
  // why a model had to be typed at all.
  if (spec?.defaultModelAllowed) {
    items.push({
      id: DEFAULT_MODEL,
      label: `${spec.title}'s own default`,
      description: "whatever it already resolves to, with no model passed",
      current: current === "",
    });
  }
  for (const m of models) {
    items.push({
      id: m.id,
      label: m.displayName || m.id,
      description: m.id,
      badge: m.provider || undefined,
      group: m.provider || "models",
      current: m.id === current,
    });
  }
  return items;
}

export function modelItems(spec: ToolSpec | undefined, current: string): PickerItem[] {
  if (!spec) return [];
  const items: PickerItem[] = [];
  if (spec.defaultModelAllowed) {
    items.push({
      id: DEFAULT_MODEL,
      label: "default for the cwd",
      description: "whatever this engine already resolves to there",
      current: current === "",
    });
  }
  for (const m of spec.models) {
    items.push({ id: m, label: m, badge: spec.title, current: m === current });
  }
  return items;
}

// QueryError is the branch that used to be missing. A registry that fails now
// says which one failed, what the server said, and offers the retry, instead
// of rendering as an empty list that reads like "there are none".
function QueryError({
  what,
  error,
  onRetry,
}: {
  what: string;
  error: unknown;
  onRetry: () => void;
}) {
  const message =
    error instanceof LoopError
      ? `${error.message}${error.code ? ` (${error.code})` : ""}`
      : error instanceof Error
        ? error.message
        : "unknown error";
  return (
    <div
      role="alert"
      className="border-destructive/40 bg-destructive/5 text-destructive flex flex-wrap items-center gap-2 rounded-md border px-3 py-2 text-sm"
    >
      <AlertCircle className="size-4 shrink-0" />
      <span className="min-w-0">Could not load {what}: {message}</span>
      <Button size="sm" variant="outline" className="ml-auto" onClick={onRetry}>
        Retry
      </Button>
    </div>
  );
}

// rolesOf lists the AGENTS a play staffs. The engineer is not one of them: he
// is the human, he runs on no engine and there is nothing to configure for
// him, so a row asking which model he uses beside three real agents was the
// form contradicting what the rest of this file already says about him.
//
// He is still a member of the loop; the surface adds his address itself.
function rolesOf(play: Play): string[] {
  return play.roles.filter((r) => r !== ENGINEER);
}

// PickerTrigger is the control-sheet's row shape at form scale: the current
// value, and a chevron saying it opens something.
export function PickerTrigger({
  label,
  onClick,
  children,
}: {
  label: string;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      onClick={onClick}
      className="border-border/70 bg-surface-raised/60 hover:border-primary/40 hover:bg-accent flex h-8 min-w-32 flex-1 items-center gap-2 rounded-md border px-2 text-xs transition-colors sm:min-w-40 sm:flex-none"
    >
      <span className="min-w-0 flex-1 truncate text-left">{children}</span>
      <ChevronRight className="text-muted-foreground size-3.5 shrink-0" aria-hidden />
    </button>
  );
}

function RoleRow({
  role,
  tools,
  loading,
  draft,
  onChange,
  onOpenPicker,
  catalogue,
}: {
  role: string;
  tools: ToolSpec[];
  loading?: boolean;
  draft: MemberDraft;
  onChange: (d: MemberDraft) => void;
  onOpenPicker: (kind: "engine" | "model") => void;
  /** catalogue: how many models this machine reported for an unprobed engine.
   *  undefined means nobody has asked yet. */
  catalogue?: number;
}) {
  // No ENGINEER branch here any more: he is never in the crew, so a row that
  // renders him would be unreachable code pretending to be a feature.
  const engine = draft.tool || "claude";
  const spec = tools.find((t) => t.id === engine);
  return (
    <div className="af-panel flex flex-col gap-2 rounded-lg p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="w-full shrink-0 font-mono text-xs sm:w-40">{role}</span>
        <PickerTrigger label={`${role} engine`} onClick={() => onOpenPicker("engine")}>
          {engineMeta(engine).displayName}
        </PickerTrigger>
        {/* A picker whenever there is a list to pick from, which is the
            registry's list for a probed engine and the machine's own answer
            for an unprobed one. The text box is the exception now: it is for
            a machine that answered with nothing, where typing is the only
            thing left. */}
        {spec?.probed || (catalogue ?? 0) > 0 ? (
          <PickerTrigger label={`${role} model`} onClick={() => onOpenPicker("model")}>
            {draft.model || `${spec?.title ?? "engine"} default`}
          </PickerTrigger>
        ) : (
          <>
            <Input
              aria-label={`${role} model`}
              className="h-8 max-w-56 text-xs"
              value={draft.model ?? ""}
              placeholder={loading || catalogue === undefined ? "asking the engine…" : "provider/model"}
              onChange={(e) => onChange({ ...draft, role, model: e.target.value })}
            />
            <Button
              type="button"
              size="sm"
              variant="outline"
              aria-label={`${role} model list`}
              onClick={() => onOpenPicker("model")}
            >
              <ListTree className="size-3" /> Browse
            </Button>
          </>
        )}
      </div>
      {/* One sentence, from one source. The registry's note used to be
          printed here AND had a second sentence appended saying the same
          thing, so the row carried the warning twice. It only appears at all
          when the machine named nothing, which is the case where a typed
          model really is unchecked. */}
      {spec && !spec.probed && catalogue === 0 && (
        <p className="text-warning-foreground flex items-start gap-1.5 text-xs leading-4">
          <AlertTriangle className="mt-0.5 size-3 shrink-0" />
          <span>{spec.note}</span>
        </p>
      )}
    </div>
  );
}

// Handoffs is the one moment a token is readable. It says so, because the
// token is stored as a hash and there is no second chance to read it.
function Handoffs({ result, onDone }: { result: CreateResult; onDone: () => void }) {
  const issued = result.handoffs.filter((h) => h.token);
  return (
    <div className="flex flex-col gap-4">
      <SectionHeader
        eyebrow="Loop created"
        title={result.loop.task}
        description="Paste one prompt into each agent's session. Every member reads the task and its step from the loop at runtime."
      />
      <p className="af-panel text-warning-foreground flex items-start gap-2 rounded-lg p-3 text-xs">
        <KeyRound className="mt-0.5 size-3.5 shrink-0" />
        <span>
          <strong>This is the only time these tokens are readable.</strong> They are stored as
          SHA-256 hashes and never returned again. If you lose one, rotate the role on the loop
          board to issue a new one.
        </span>
      </p>
      {issued.map((h) => (
        <HandoffCard key={h.role} handoff={h} />
      ))}
      <div>
        <Button onClick={onDone}>Open the loop</Button>
      </div>
    </div>
  );
}

export function HandoffCard({ handoff }: { handoff: Handoff }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="af-panel rounded-lg p-3">
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <span className="font-mono text-xs font-semibold">{handoff.role}</span>
        {handoff.tool && (
          <span className="text-muted-foreground text-xs">
            {engineMeta(handoff.tool).displayName}
          </span>
        )}
        {handoff.model && <Badge variant="outline">{handoff.model}</Badge>}
        <Button
          size="sm"
          variant="outline"
          className="ml-auto"
          onClick={() => {
            void navigator.clipboard?.writeText(handoff.prompt);
            setCopied(true);
          }}
        >
          <Copy className="size-3" /> {copied ? "Copied" : "Copy prompt"}
        </Button>
      </div>
      <CodePreview>{handoff.prompt}</CodePreview>
    </div>
  );
}
