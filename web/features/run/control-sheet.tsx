"use client";

// ControlSheet — the bottom-sheet native control layer. Sections:
//   - Model
//   - Agent       (OpenCode)
//   - Provider    (OpenCode)
//   - MCP         (OpenCode)
//   - Skills      (OpenCode)
//   - Commands    (OpenCode; the composer reads this for slash autocomplete)
//
// Principles:
//   - apply, then re-read the truth — every apply call returns the new
//     state, which the chips reconcile to (never optimistic-only).
//   - degrade honestly — engine "SetX = false" greys the row and explains
//     why ("engine does not support live X switching").
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  ChevronLeft,
  ChevronRight,
  Cpu,
  Loader2,
  ServerCog,
  Sparkles,
  Terminal as TerminalIcon,
  Wrench,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { Picker, type PickerItem } from "@/components/ui/picker";
import {
  getEngineCaps,
  getEngineState,
  listEngineAgents,
  listEngineCommands,
  listEngineMCP,
  listEngineModels,
  listEngineProviders,
  listEngineSkills,
  runEngineCommand,
  setEngineAgent,
  setEngineModel,
  setEngineProvider,
} from "@/lib/agentd";
import { engineMeta } from "@/lib/engine";

type SectionKind =
  "models" | "agents" | "providers" | "mcp" | "skills" | "commands";

interface ControlSheetProps {
  runId: string;
  open: boolean;
  onClose: () => void;
  initialModel?: string;
}

export function ControlSheet({
  runId,
  open,
  onClose,
  initialModel,
}: ControlSheetProps) {
  const qc = useQueryClient();
  const capsQ = useQuery({
    queryKey: ["engine-caps", runId],
    queryFn: () => getEngineCaps(runId),
    enabled: open,
    staleTime: 60_000,
  });
  const stateQ = useQuery({
    queryKey: ["engine-state", runId],
    queryFn: () => getEngineState(runId),
    enabled: open,
    refetchInterval: 30_000,
  });

  const [pickerOpen, setPickerOpen] = useState<SectionKind | null>(null);
  // A command with an argument hint asks for its argument before it runs:
  // firing /add-dir or /model with nothing attached either does nothing or
  // opens an interactive menu in a terminal nobody is looking at.
  const [pendingCommand, setPendingCommand] = useState<PendingCommand | null>(
    null,
  );

  // --- back navigation ---------------------------------------------------
  //
  // The sheet and the picker are two stacked layers with no window chrome
  // behind them: on a phone there is no Escape key, and the hardware/browser
  // Back button used to leave the run page entirely from under an open
  // picker. Each layer therefore pushes one history entry, and Back pops one
  // layer at a time — picker, then sheet, then the page.
  //
  // Every close path goes through closeLayer/closeSheet, which unwind the
  // pushed entry themselves and suppress the popstate that unwinding causes.
  // The UI never depends on popstate firing, so it still works where history
  // traversal does not (embedded webviews, a page opened with no session
  // history).
  const depthRef = useRef(0);
  const suppressPopRef = useRef(0);

  const pushLayer = useCallback(() => {
    depthRef.current += 1;
    try {
      window.history.pushState(
        { af: "control-sheet", depth: depthRef.current },
        "",
      );
    } catch {
      // no history to push onto: the buttons still close their layer
    }
  }, []);

  const popLayer = useCallback(() => {
    if (depthRef.current <= 0) return;
    depthRef.current -= 1;
    suppressPopRef.current += 1;
    try {
      window.history.back();
    } catch {
      suppressPopRef.current -= 1;
    }
  }, []);

  const layerOpen = pickerOpen !== null || pendingCommand !== null;

  // closeLayer drops the top layer (picker or argument step) and returns to
  // the sheet.
  const closeLayer = useCallback(() => {
    setPickerOpen(null);
    setPendingCommand(null);
    popLayer();
  }, [popLayer]);

  const closeSheet = useCallback(() => {
    setPickerOpen(null);
    setPendingCommand(null);
    // unwind whatever this sheet pushed, so a closed sheet leaves the
    // history stack exactly as it found it
    while (depthRef.current > 0) popLayer();
    onClose();
  }, [onClose, popLayer]);

  // one entry for the sheet itself
  useEffect(() => {
    if (!open) return undefined;
    pushLayer();
    return () => {
      while (depthRef.current > 0) {
        depthRef.current -= 1;
        suppressPopRef.current += 1;
        try {
          window.history.back();
        } catch {
          suppressPopRef.current -= 1;
        }
      }
    };
  }, [open, pushLayer]);

  // one entry per opened layer
  useEffect(() => {
    if (!open || !layerOpen) return;
    pushLayer();
  }, [open, layerOpen, pushLayer]);

  useEffect(() => {
    if (!open) return undefined;
    const onPop = () => {
      if (suppressPopRef.current > 0) {
        suppressPopRef.current -= 1;
        return;
      }
      // the browser already consumed one entry
      if (depthRef.current > 0) depthRef.current -= 1;
      if (pickerOpen !== null || pendingCommand !== null) {
        setPickerOpen(null);
        setPendingCommand(null);
        return;
      }
      onClose();
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, [open, pickerOpen, pendingCommand, onClose]);
  const [applyBusy, setApplyBusy] = useState<SectionKind | null>(null);
  const [applyError, setApplyError] = useState<string | null>(null);

  const caps = capsQ.data?.caps;
  const engineId = capsQ.data?.engine ?? stateQ.data?.engine ?? "claude";
  const meta = engineMeta(engineId);

  async function apply(kind: SectionKind, fn: () => Promise<unknown>) {
    setApplyBusy(kind);
    setApplyError(null);
    try {
      await fn();
      await qc.invalidateQueries({ queryKey: ["engine-state", runId] });
      await qc.invalidateQueries({ queryKey: ["run-status", runId] });
      await stateQ.refetch();
    } catch (e) {
      setApplyError(e instanceof Error ? e.message : "apply failed");
    } finally {
      setApplyBusy(null);
    }
  }

  if (!open) return null;
  return (
    <div
      role="dialog"
      aria-label="Control sheet"
      className="bg-background/95 border-border/60 fixed inset-x-0 bottom-0 z-40 max-h-[80dvh] overflow-y-auto rounded-t-2xl border-t shadow-2xl backdrop-blur-xl"
      style={{ paddingBottom: "env(safe-area-inset-bottom)" }}
    >
      <div className="mx-auto w-full max-w-2xl px-4 pt-3 pb-4">
        <header className="flex items-center justify-between gap-2 pb-2">
          <div>
            <p className="text-muted-foreground text-[10px] font-semibold tracking-wider uppercase">
              Control
            </p>
            <h2 className="text-foreground text-sm font-semibold">
              {meta.displayName} session
            </h2>
          </div>
          <button
            type="button"
            onClick={closeSheet}
            aria-label="Close"
            className="text-muted-foreground hover:bg-accent/60 grid min-h-11 min-w-11 place-items-center rounded-full text-lg transition-colors"
          >
            ×
          </button>
        </header>

        <Section
          icon={<Cpu className="text-muted-foreground size-4" aria-hidden />}
          label="Model"
          value={stateQ.data?.state?.model || initialModel || ""}
          emptyLabel="default"
          disabledReason={
            !caps
              ? "Loading capabilities…"
              : caps.setModel
                ? undefined
                : "This engine has no live model switch"
          }
          // A PTY-driven switch types /model and cannot read the result
          // back, so the value shown is the one we persisted on apply.
          hint={
            caps?.setModel && !stateQ.data?.state?.model
              ? "Applies from the next turn"
              : undefined
          }
          onClick={() => caps?.setModel && setPickerOpen("models")}
          loading={applyBusy === "models"}
        />

        {caps?.setAgent && (
          <Section
            icon={<Cpu className="text-muted-foreground size-4" aria-hidden />}
            label="Agent"
            value={stateQ.data?.state?.agent || ""}
            emptyLabel="default"
            disabledReason={
              caps
                ? caps.setAgent
                  ? undefined
                  : "Engine does not support live agent switching"
                : "Loading capabilities…"
            }
            onClick={() => caps?.setAgent && setPickerOpen("agents")}
            loading={applyBusy === "agents"}
          />
        )}

        {caps?.listProviders && (
          <Section
            icon={
              <ServerCog className="text-muted-foreground size-4" aria-hidden />
            }
            label="Provider"
            value={stateQ.data?.state?.provider || ""}
            emptyLabel="default"
            disabledReason={
              caps
                ? caps.setProvider
                  ? undefined
                  : "Engine does not support live provider switching"
                : "Loading capabilities…"
            }
            onClick={() => caps?.setProvider && setPickerOpen("providers")}
            loading={applyBusy === "providers"}
          />
        )}

        {caps?.listMcpServers && (
          <Section
            icon={
              <ServerCog className="text-muted-foreground size-4" aria-hidden />
            }
            label="MCP servers"
            value=""
            emptyLabel="View list"
            disabledReason={
              caps
                ? caps.listMcpServers
                  ? undefined
                  : "Engine does not expose MCP"
                : "Loading capabilities…"
            }
            onClick={() => caps?.listMcpServers && setPickerOpen("mcp")}
            loading={applyBusy === "mcp"}
          />
        )}

        {caps?.listSkills && (
          <Section
            icon={
              <Sparkles className="text-muted-foreground size-4" aria-hidden />
            }
            label="Skills"
            value=""
            emptyLabel="View list"
            disabledReason={
              caps
                ? caps.listSkills
                  ? undefined
                  : "Engine does not expose skills"
                : "Loading capabilities…"
            }
            onClick={() => caps?.listSkills && setPickerOpen("skills")}
            loading={applyBusy === "skills"}
          />
        )}

        {caps?.runCommand && (
          <Section
            icon={
              <TerminalIcon
                className="text-muted-foreground size-4"
                aria-hidden
              />
            }
            label="Commands"
            value=""
            emptyLabel="Run a slash command"
            disabledReason={
              caps
                ? caps.runCommand
                  ? undefined
                  : "Engine does not support slash commands"
                : "Loading capabilities…"
            }
            onClick={() => caps?.runCommand && setPickerOpen("commands")}
            loading={applyBusy === "commands"}
          />
        )}

        {applyError && (
          <p className="text-destructive mt-3 text-sm" role="alert">
            {applyError}
          </p>
        )}

        <ModelsPicker
          key={`models:${pickerOpen === "models" ? "open" : "closed"}`}
          open={pickerOpen === "models"}
          runId={runId}
          currentModel={stateQ.data?.state?.model || initialModel || ""}
          onClose={closeLayer}
          onPick={async (it) => {
            closeLayer();
            await apply("models", () =>
              setEngineModel(runId, it.id).then(() => undefined),
            );
          }}
        />
        <AgentsPicker
          key={`agents:${pickerOpen === "agents" ? "open" : "closed"}`}
          open={pickerOpen === "agents"}
          runId={runId}
          currentAgent={stateQ.data?.state?.agent || ""}
          onClose={closeLayer}
          onPick={async (it) => {
            closeLayer();
            await apply("agents", () =>
              setEngineAgent(runId, it.id).then(() => undefined),
            );
          }}
        />
        <ProvidersPicker
          key={`providers:${pickerOpen === "providers" ? "open" : "closed"}`}
          open={pickerOpen === "providers"}
          runId={runId}
          onClose={closeLayer}
          onPick={async (it) => {
            closeLayer();
            await apply("providers", () =>
              setEngineProvider(runId, it.id).then(() => undefined),
            );
          }}
        />
        <MCPPicker
          key={`mcp:${pickerOpen === "mcp" ? "open" : "closed"}`}
          open={pickerOpen === "mcp"}
          runId={runId}
          onClose={closeLayer}
        />
        <SkillsPicker
          key={`skills:${pickerOpen === "skills" ? "open" : "closed"}`}
          open={pickerOpen === "skills"}
          runId={runId}
          onClose={closeLayer}
        />
        <CommandsPicker
          key={`commands:${pickerOpen === "commands" ? "open" : "closed"}`}
          open={pickerOpen === "commands"}
          runId={runId}
          onClose={closeLayer}
          onPick={async (cmd) => {
            if (cmd.argsHint) {
              // a layer SWAP, not a close: the argument step replaces the
              // picker at the same depth, so Back still returns to the sheet
              setPickerOpen(null);
              setPendingCommand(cmd);
              return;
            }
            closeLayer();
            await apply("commands", () =>
              runEngineCommand(runId, cmd.name).then(() => undefined),
            );
          }}
        />
        {pendingCommand && (
          <CommandArgs
            command={pendingCommand}
            busy={applyBusy === "commands"}
            onCancel={closeLayer}
            onRun={async (args) => {
              const cmd = pendingCommand;
              closeLayer();
              await apply("commands", () =>
                runEngineCommand(runId, cmd.name, args).then(() => undefined),
              );
            }}
          />
        )}
      </div>
    </div>
  );
}

function Section({
  icon,
  label,
  value,
  emptyLabel,
  disabledReason,
  hint,
  onClick,
  loading,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  emptyLabel: string;
  disabledReason?: string;
  // hint is the small note under the value ("Applies from the next turn").
  hint?: string;
  onClick: () => void;
  loading?: boolean;
}) {
  const disabled = !!disabledReason;
  return (
    <button
      type="button"
      onClick={disabled ? undefined : onClick}
      disabled={disabled}
      aria-label={`${label}: ${value || emptyLabel}`}
      title={disabled ? disabledReason : undefined}
      className={cn(
        "border-border/40 hover:bg-accent/40 mt-2 flex w-full items-center gap-3 rounded-xl border px-3 py-2.5 text-left transition-colors",
        disabled && "opacity-50",
      )}
    >
      <span className="grid size-8 place-items-center rounded-full bg-current/5">
        {icon}
      </span>
      <span className="min-w-0 flex-1">
        <span className="text-muted-foreground block text-[11px] font-medium tracking-wider uppercase">
          {label}
        </span>
        <span className="text-foreground block truncate text-[15px] font-medium">
          {value || emptyLabel}
        </span>
        {hint && (
          <span className="text-muted-foreground block truncate text-[11px]">
            {hint}
          </span>
        )}
      </span>
      {loading ? (
        <Loader2 className="text-muted-foreground size-4 animate-spin" />
      ) : (
        <ChevronRight className="text-muted-foreground size-4" aria-hidden />
      )}
    </button>
  );
}

function ModelsPicker({
  open,
  runId,
  currentModel,
  onClose,
  onPick,
}: {
  open: boolean;
  runId: string;
  currentModel: string;
  onClose: () => void;
  onPick: (it: PickerItem) => void | Promise<void>;
}) {
  const modelsQ = useQuery({
    queryKey: ["engine-models", runId],
    queryFn: () => listEngineModels(runId),
    enabled: open,
    staleTime: 60_000,
  });
  const items: PickerItem[] =
    modelsQ.data?.models.map((m) => ({
      id: m.id,
      label: m.displayName || m.id,
      description: m.id,
      badge: m.provider,
      group: m.provider || "Other",
      current: m.id === currentModel,
    })) ?? [];
  return (
    <Picker
      open={open}
      title="Select model"
      items={items}
      loading={modelsQ.isLoading}
      note={modelsQ.data?.note}
      onClose={onClose}
      onPick={onPick}
    />
  );
}

function AgentsPicker({
  open,
  runId,
  currentAgent,
  onClose,
  onPick,
}: {
  open: boolean;
  runId: string;
  currentAgent: string;
  onClose: () => void;
  onPick: (it: PickerItem) => void | Promise<void>;
}) {
  const q = useQuery({
    queryKey: ["engine-agents", runId],
    queryFn: () => listEngineAgents(runId),
    enabled: open,
    staleTime: 60_000,
  });
  const items: PickerItem[] =
    q.data?.agents.map((a) => ({
      id: a.name,
      label: a.name,
      description: a.description,
      badge: a.model,
      current: a.name === currentAgent,
    })) ?? [];
  return (
    <Picker
      open={open}
      title="Select agent"
      items={items}
      loading={q.isLoading}
      onClose={onClose}
      onPick={onPick}
    />
  );
}

function ProvidersPicker({
  open,
  runId,
  onClose,
  onPick,
}: {
  open: boolean;
  runId: string;
  onClose: () => void;
  onPick: (it: PickerItem) => void | Promise<void>;
}) {
  const q = useQuery({
    queryKey: ["engine-providers", runId],
    queryFn: () => listEngineProviders(runId),
    enabled: open,
    staleTime: 60_000,
  });
  const items: PickerItem[] =
    q.data?.providers.map((p) => ({
      id: p.id,
      label: p.name || p.id,
      description: p.id,
    })) ?? [];
  return (
    <Picker
      open={open}
      title="Select provider"
      items={items}
      loading={q.isLoading}
      onClose={onClose}
      onPick={onPick}
    />
  );
}

function ReadOnlyPicker({
  open,
  items,
  title,
  loading,
  onClose,
}: {
  open: boolean;
  items: PickerItem[];
  title: string;
  loading?: boolean;
  onClose: () => void;
}) {
  return (
    <Picker
      open={open}
      title={title}
      items={items}
      loading={loading}
      onClose={onClose}
      onPick={() => undefined}
    />
  );
}

function MCPPicker({
  open,
  runId,
  onClose,
}: {
  open: boolean;
  runId: string;
  onClose: () => void;
}) {
  const q = useQuery({
    queryKey: ["engine-mcp", runId],
    queryFn: () => listEngineMCP(runId),
    enabled: open,
    staleTime: 60_000,
  });
  const items: PickerItem[] = (q.data?.servers ?? []).map((m) => ({
    id: m.name,
    label: m.name,
    description: m.url,
    badge: m.status,
    disabled: true,
    disabledReason: "Toggle / remove arrives in a later slice",
  }));
  return (
    <ReadOnlyPicker
      open={open}
      title="MCP servers"
      items={items}
      loading={q.isLoading}
      onClose={onClose}
    />
  );
}

function SkillsPicker({
  open,
  runId,
  onClose,
}: {
  open: boolean;
  runId: string;
  onClose: () => void;
}) {
  const q = useQuery({
    queryKey: ["engine-skills", runId],
    queryFn: () => listEngineSkills(runId),
    enabled: open,
    staleTime: 60_000,
  });
  const items: PickerItem[] = (q.data?.skills ?? []).map((s) => ({
    id: s.name,
    label: s.name,
    description: s.description,
    badge: s.path,
    disabled: true,
    disabledReason: "Skill editing arrives in a later slice",
  }));
  return (
    <ReadOnlyPicker
      open={open}
      title="Skills"
      items={items}
      loading={q.isLoading}
      onClose={onClose}
    />
  );
}

// PendingCommand is a picked command still waiting for its argument.
interface PendingCommand {
  name: string;
  argsHint: string;
  description: string;
}

function CommandsPicker({
  open,
  runId,
  onClose,
  onPick,
}: {
  open: boolean;
  runId: string;
  onClose: () => void;
  onPick: (cmd: PendingCommand) => void | Promise<void>;
}) {
  const q = useQuery({
    queryKey: ["engine-commands", runId],
    queryFn: () => listEngineCommands(runId),
    enabled: open,
    staleTime: 60_000,
  });
  const commands = q.data?.commands ?? [];
  const items: PickerItem[] = commands.map((c) => ({
    id: c.name,
    label: "/" + c.name,
    description: c.description,
    badge: c.argsHint ? c.argsHint : c.source,
  }));
  return (
    <Picker
      open={open}
      title="Run command"
      items={items}
      loading={q.isLoading}
      onClose={onClose}
      onPick={(it) => {
        const cmd = commands.find((c) => c.name === it.id);
        return onPick({
          name: it.id,
          argsHint: cmd?.argsHint ?? "",
          description: cmd?.description ?? "",
        });
      }}
    />
  );
}

// CommandArgs is the second step for a command that takes an argument.
// Running /add-dir or /model with nothing attached either does nothing or
// leaves an interactive menu open in a terminal nobody is watching, so the
// sheet asks first.
function CommandArgs({
  command,
  busy,
  onRun,
  onCancel,
}: {
  command: PendingCommand;
  busy?: boolean;
  onRun: (args: string) => void | Promise<void>;
  onCancel: () => void;
}) {
  const [args, setArgs] = useState("");
  return (
    <form
      role="group"
      aria-label={`Arguments for /${command.name}`}
      onSubmit={(e) => {
        e.preventDefault();
        void onRun(args.trim());
      }}
      className="border-border/40 mt-2 rounded-xl border px-2 py-2"
    >
      {/* Back sits where the picker's does and says the same thing: this is
          a step on top of the sheet, and leaving it returns to the sheet.
          It used to read "Cancel", off to the right beside Run, which on a
          phone reads as "discard" rather than "go back". */}
      <div className="flex items-center justify-between gap-2">
        <button
          type="button"
          onClick={onCancel}
          aria-label="Back to the control sheet"
          className="text-muted-foreground hover:bg-accent/60 hover:text-foreground -ml-1 inline-flex min-h-11 shrink-0 items-center gap-0.5 rounded-full pr-3 pl-1.5 text-[13px] font-medium transition-colors"
        >
          <ChevronLeft className="size-5" aria-hidden />
          Back
        </button>
        <p className="text-foreground truncate text-sm font-semibold">
          /{command.name}
        </p>
      </div>
      {command.description && (
        <p className="text-muted-foreground truncate px-1 text-[12px]">
          {command.description}
        </p>
      )}
      <div className="mt-2 flex items-center gap-2 px-1 pb-1">
        <input
          autoFocus
          value={args}
          onChange={(e) => setArgs(e.target.value)}
          placeholder={command.argsHint}
          aria-label={`${command.name} argument`}
          className="border-border/60 focus:border-ring min-h-11 min-w-0 flex-1 rounded-lg border bg-transparent px-2.5 text-[14px] outline-none"
        />
        <button
          type="submit"
          disabled={busy}
          className="bg-primary text-primary-foreground min-h-11 shrink-0 rounded-lg px-4 text-[13px] font-medium disabled:opacity-50"
        >
          Run
        </button>
      </div>
    </form>
  );
}

// Re-export a Wrench import to avoid an unused-import error in tools that
// tree-shake this module before the icon reference is used.
const _wrench = Wrench;
void _wrench;
