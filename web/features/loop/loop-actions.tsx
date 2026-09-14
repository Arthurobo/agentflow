"use client";

// loop-actions.tsx — the verbs the loop surface already had and the board
// never offered. Ending, continuing, staffing and retiring were all reachable
// from the API and from nothing the engineer could click, which is why the
// wizard could tell him to "rotate the role" next to no rotate control.
//
// Every form is an inline panel rather than a modal, matching the wizard: a
// picker is itself a sheet, and stacking one inside a dialog makes Escape
// close the form under it.

import { useState } from "react";
import { AlertCircle, CircleOff, GitBranch, Square, UserPlus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { engineMeta } from "@/lib/engine";
import { Input } from "@/components/ui/input";
import { Picker, type PickerItem } from "@/components/ui/picker";
import { Field, FieldGroup } from "@/components/ui/surface";
import {
  addMember,
  continueLoop,
  dismissLoop,
  endLoop,
  LoopError,
  type Handoff,
  type Loop,
  type Play,
  type ToolSpec,
} from "@/lib/loop";
import { engineItems, HandoffCard, modelItems, PickerTrigger } from "./wizard";

type Panel = "end" | "continue" | "member" | null;

function errorText(e: unknown): string {
  if (e instanceof LoopError) return `${e.message}${e.code ? ` (${e.code})` : ""}`;
  return e instanceof Error ? e.message : String(e);
}

export function LoopActions({
  loop,
  plays,
  tools,
  onChanged,
}: {
  loop: Loop;
  plays: Play[];
  tools: ToolSpec[];
  onChanged: () => Promise<void> | void;
}) {
  const [panel, setPanel] = useState<Panel>(null);
  const ended = loop.status !== "active";

  const toggle = (p: Panel) => setPanel((cur) => (cur === p ? null : p));

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap gap-2">
        {/* The backend refuses a member on an ended loop with a raw error.
            Continue stays enabled: continuing is what you do with one. */}
        <Button
          size="sm"
          variant="outline"
          disabled={ended}
          title={ended ? "This loop has ended, so it takes no new members. Continue it instead." : undefined}
          onClick={() => toggle("member")}
        >
          <UserPlus className="size-3" /> Add member
        </Button>
        <Button size="sm" variant="outline" onClick={() => toggle("continue")}>
          <GitBranch className="size-3" /> Continue
        </Button>
        {ended ? (
          <RetireAll loopId={loop.id} onChanged={onChanged} />
        ) : (
          <Button size="sm" variant="outline" onClick={() => toggle("end")}>
            <Square className="size-3" /> End loop
          </Button>
        )}
      </div>
      {panel === "end" && (
        <EndPanel loopId={loop.id} onDone={() => setPanel(null)} onChanged={onChanged} />
      )}
      {panel === "continue" && (
        <ContinuePanel
          loop={loop}
          plays={plays}
          onDone={() => setPanel(null)}
          onChanged={onChanged}
        />
      )}
      {ended && panel === "member" && null}
      {!ended && panel === "member" && (
        <AddMemberPanel
          loopId={loop.id}
          tools={tools}
          onDone={() => setPanel(null)}
          onChanged={onChanged}
        />
      )}
    </div>
  );
}

function Panel({ children }: { children: React.ReactNode }) {
  return <div className="af-subtle-panel flex flex-col gap-3 rounded-lg p-3">{children}</div>;
}

function ErrorLine({ error }: { error: string }) {
  if (!error) return null;
  return (
    <p role="alert" className="text-destructive flex items-center gap-1.5 text-xs">
      <AlertCircle className="size-3.5 shrink-0" /> {error}
    </p>
  );
}

// Ending PARKS the crew by default: their tokens stay live so the next loop
// can adopt them. Retiring is the separate, harsher thing and says so.
function EndPanel({
  loopId,
  onDone,
  onChanged,
}: {
  loopId: string;
  onDone: () => void;
  onChanged: () => Promise<void> | void;
}) {
  const [reason, setReason] = useState("");
  const [dismiss, setDismiss] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Panel>
      <Field label="Why it ended" hint="Recorded on the loop. Optional.">
        <Input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="review reported clean" />
      </Field>
      <label className="flex items-center gap-2 text-xs">
        <input
          type="checkbox"
          checked={dismiss}
          onChange={(e) => setDismiss(e.target.checked)}
          aria-label="retire the agents too"
        />
        <span>
          Retire the agents too. Left unticked they are parked idle with live tokens, ready for the
          next loop to adopt.
        </span>
      </label>
      <ErrorLine error={error} />
      <div className="flex gap-2">
        <Button
          size="sm"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            setError("");
            try {
              await endLoop(loopId, { reason: reason.trim(), dismissAgents: dismiss });
              await onChanged();
              onDone();
            } catch (e) {
              setError(errorText(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          {busy ? "Ending…" : dismiss ? "End and retire the crew" : "End and park the crew"}
        </Button>
        <Button size="sm" variant="outline" onClick={onDone} disabled={busy}>
          Cancel
        </Button>
      </div>
    </Panel>
  );
}

function RetireAll({ loopId, onChanged }: { loopId: string; onChanged: () => Promise<void> | void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <>
      <Button
        size="sm"
        variant="outline"
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          setError("");
          try {
            await dismissLoop(loopId);
            await onChanged();
          } catch (e) {
            setError(errorText(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        <CircleOff className="size-3" /> {busy ? "Retiring…" : "Retire all agents"}
      </Button>
      <ErrorLine error={error} />
    </>
  );
}

// Continue is the richest verb on the surface and had no button at all: it
// ends this loop and starts a successor that ADOPTS the crew, so the tokens
// already pasted into those sessions keep working.
function ContinuePanel({
  loop,
  plays,
  onDone,
  onChanged,
}: {
  loop: Loop;
  plays: Play[];
  onDone: () => void;
  onChanged: () => Promise<void> | void;
}) {
  const [task, setTask] = useState("");
  const [play, setPlay] = useState(loop.play);
  const [carryNotes, setCarryNotes] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<{ kept: string[]; carried: number; handoffs: Handoff[] } | null>(
    null,
  );

  if (result) {
    return (
      <Panel>
        <p className="text-xs">
          Successor started. {result.kept.length} agent{result.kept.length === 1 ? "" : "s"} kept
          their tokens{result.carried > 0 ? `, ${result.carried} notes carried over` : ""}.
        </p>
        {result.handoffs
          .filter((h) => h.token)
          .map((h) => (
            <HandoffCard key={h.role} handoff={h} />
          ))}
        <div>
          <Button size="sm" onClick={onDone}>
            Done
          </Button>
        </div>
      </Panel>
    );
  }

  return (
    <Panel>
      <Field
        label="Next task"
        hint="This loop ends and a successor starts. The crew is adopted, so nobody re-pastes a prompt."
      >
        <Input value={task} onChange={(e) => setTask(e.target.value)} placeholder="Fix the review's findings" />
      </Field>
      <FieldGroup label="Play" hint="Defaults to the play this loop ran.">
        <div className="flex flex-wrap gap-1.5">
          {plays.map((p) => (
            <button
              key={p.name}
              type="button"
              aria-pressed={p.name === play}
              onClick={() => setPlay(p.name)}
              className={`rounded-md border px-2 py-1 text-xs transition ${
                p.name === play
                  ? "border-primary/60 bg-primary/10 text-primary"
                  : "border-border/70 hover:border-primary/35"
              }`}
            >
              {p.title}
            </button>
          ))}
        </div>
      </FieldGroup>
      <label className="flex items-center gap-2 text-xs">
        <input
          type="checkbox"
          checked={carryNotes}
          onChange={(e) => setCarryNotes(e.target.checked)}
          aria-label="carry the notes over"
        />
        <span>Carry each member&apos;s notes into the successor.</span>
      </label>
      <ErrorLine error={error} />
      <div className="flex gap-2">
        <Button
          size="sm"
          disabled={busy || !task.trim()}
          onClick={async () => {
            setBusy(true);
            setError("");
            try {
              const r = await continueLoop(loop.id, {
                task: task.trim(),
                play,
                carryNotes,
              });
              await onChanged();
              setResult({ kept: r.keptAgents, carried: r.notesCarried, handoffs: r.handoffs });
            } catch (e) {
              setError(errorText(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          {busy ? "Continuing…" : "Start the successor"}
        </Button>
        <Button size="sm" variant="outline" onClick={onDone} disabled={busy}>
          Cancel
        </Button>
      </div>
    </Panel>
  );
}

function AddMemberPanel({
  loopId,
  tools,
  onDone,
  onChanged,
}: {
  loopId: string;
  tools: ToolSpec[];
  onDone: () => void;
  onChanged: () => Promise<void> | void;
}) {
  const [role, setRole] = useState("");
  const [tool, setTool] = useState("claude");
  const [model, setModel] = useState("");
  const [picker, setPicker] = useState<"engine" | "model" | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [handoff, setHandoff] = useState<Handoff | null>(null);

  const spec = tools.find((t) => t.id === tool);

  if (handoff) {
    return (
      <Panel>
        <HandoffCard handoff={handoff} />
        <div>
          <Button size="sm" onClick={onDone}>
            Done
          </Button>
        </div>
      </Panel>
    );
  }

  function pick(item: PickerItem) {
    if (picker === "engine") {
      setTool(item.id);
      setModel("");
    } else if (picker === "model") {
      setModel(item.id === "__default__" ? "" : item.id);
    }
    setPicker(null);
  }

  return (
    <Panel>
      <Field label="Role" hint="Uppercase, and unique in this loop. It is the address other members write to.">
        <Input
          value={role}
          onChange={(e) => setRole(e.target.value.toUpperCase())}
          placeholder="INVESTIGATION#2"
        />
      </Field>
      <div className="flex flex-wrap items-center gap-2">
        <PickerTrigger label="new member engine" onClick={() => setPicker("engine")}>
          {engineMeta(tool).displayName}
        </PickerTrigger>
        {spec?.probed ? (
          <PickerTrigger label="new member model" onClick={() => setPicker("model")}>
            {model || "default for the cwd"}
          </PickerTrigger>
        ) : (
          <Input
            aria-label="new member model"
            className="h-8 max-w-56 text-xs"
            value={model}
            placeholder="model name"
            onChange={(e) => setModel(e.target.value)}
          />
        )}
      </div>
      <ErrorLine error={error} />
      <div className="flex gap-2">
        <Button
          size="sm"
          disabled={busy || !role.trim()}
          onClick={async () => {
            setBusy(true);
            setError("");
            try {
              const h = await addMember(loopId, { role: role.trim(), tool, model });
              await onChanged();
              setHandoff(h);
            } catch (e) {
              setError(errorText(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          {busy ? "Adding…" : "Add member"}
        </Button>
        <Button size="sm" variant="outline" onClick={onDone} disabled={busy}>
          Cancel
        </Button>
      </div>
      {picker && (
        <Picker
          open
          title={picker === "engine" ? "new member engine" : "new member model"}
          items={picker === "engine" ? engineItems(tools, tool) : modelItems(spec, model)}
          onClose={() => setPicker(null)}
          onPick={pick}
        />
      )}
    </Panel>
  );
}
