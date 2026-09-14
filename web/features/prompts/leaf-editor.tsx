"use client";

// leaf-editor.tsx — one editable paragraph. Everything the round asks for
// happens at this level: the shipped text, the engineer's version, the three
// override states, revert, and the validator's message under the field it
// belongs to.

import { useState } from "react";
import { AlertCircle, Check, Lock, RotateCcw } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { Field } from "@/components/ui/surface";
import {
  OVERRIDE_STALE,
  PromptError,
  type OverrideTarget,
  type PromptOverride,
} from "@/lib/prompts";

export interface LeafEditorProps {
  label: string;
  hint?: string;
  target: OverrideTarget;
  /** shipped is what the binary ships. For a binding rule it is immovable. */
  shipped: string;
  /** current is the merged text this machine actually uses. */
  current: string;
  override?: PromptOverride;
  /** binding rules are APPEND-ONLY: the shipped text is fixed and an edit can
   *  only add to it, so the interaction teaches the constraint rather than a
   *  409 explaining it after he has typed a replacement. */
  binding?: boolean;
  onSave: (value: string) => Promise<void>;
  onRevert: () => Promise<void>;
}

export function LeafEditor({
  label,
  hint,
  target,
  shipped,
  current,
  override,
  binding,
  onSave,
  onRevert,
}: LeafEditorProps) {
  // A binding rule's box holds only the addition, never the whole rule.
  //
  // The draft is seeded once and never synced from props by an effect: a
  // setState in an effect to mirror props is a cascading render. The caller
  // gives this component a key derived from the server's copy, so new data
  // remounts it with a fresh draft and typing is never interrupted.
  const [draft, setDraft] = useState(binding ? (override?.value ?? "") : current);
  const [busy, setBusy] = useState<"save" | "revert" | null>(null);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);

  const stale = override?.status === OVERRIDE_STALE;
  const dirty = draft !== (binding ? (override?.value ?? "") : current);

  async function run(kind: "save" | "revert", fn: () => Promise<void>) {
    setBusy(kind);
    setError("");
    try {
      await fn();
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } catch (e) {
      // The validator's own message, verbatim, under the field it belongs to.
      setError(e instanceof PromptError ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="af-subtle-panel flex flex-col gap-2 rounded-lg p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="af-label">{label}</span>
        {binding && (
          <Badge variant="outline" className="gap-1">
            <Lock className="size-2.5" /> add only
          </Badge>
        )}
        {override && !stale && <Badge variant="outline">edited</Badge>}
        {stale && <Badge variant="destructive">the default changed</Badge>}
        {override && (
          <Button
            size="sm"
            variant="outline"
            className="ml-auto"
            disabled={busy !== null}
            onClick={() => void run("revert", onRevert)}
          >
            <RotateCcw className="size-3" /> {busy === "revert" ? "Reverting…" : "Revert"}
          </Button>
        )}
      </div>

      {binding && (
        // Fixed and immovable, above the box rather than inside it.
        <div className="border-border/70 bg-surface-sunken/50 text-muted-foreground rounded-md border px-3 py-2 text-xs leading-5">
          <p className="mb-1 flex items-center gap-1.5 font-medium">
            <Lock className="size-3" /> This rule ships fixed and cannot be replaced from here.
          </p>
          <p className="whitespace-pre-wrap">{shipped}</p>
        </div>
      )}

      {stale && (
        // His choice, never automatic: it is his text and we do not revoke it.
        <StaleChoice
          mine={override?.value ?? ""}
          theirs={override?.base ?? shipped}
          busy={busy !== null}
          onKeepMine={() => void run("save", () => onSave(override?.value ?? ""))}
          onTakeTheirs={() => void run("revert", onRevert)}
        />
      )}

      <Field label={binding ? "Add to this rule" : "Your version"} hint={hint}>
        <Textarea
          autoResize
          aria-label={`${label} text`}
          value={draft}
          placeholder={binding ? "Anything you add here is appended to the rule above." : shipped}
          onChange={(e) => setDraft(e.target.value)}
        />
      </Field>

      {error && (
        <p role="alert" className="text-destructive flex items-start gap-1.5 text-xs leading-5">
          <AlertCircle className="mt-0.5 size-3.5 shrink-0" />
          <span>{error}</span>
        </p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          disabled={busy !== null || !dirty || draft.trim() === ""}
          onClick={() => void run("save", () => onSave(draft))}
        >
          {busy === "save" ? "Saving…" : "Save"}
        </Button>
        {saved && !error && (
          <span className="text-success flex items-center gap-1 text-xs">
            <Check className="size-3" /> in effect on every session from now on
          </span>
        )}
        {!override && !dirty && (
          <span className="text-muted-foreground text-xs">Shipped default, unedited.</span>
        )}
        <span className="sr-only">{`${target.kind} ${target.field}`}</span>
      </div>
    </div>
  );
}

// StaleChoice is a side by side, not a merge. These are prose paragraphs and a
// welded sentence neither of us wrote would be shipped to an agent as an
// instruction.
function StaleChoice({
  mine,
  theirs,
  busy,
  onKeepMine,
  onTakeTheirs,
}: {
  mine: string;
  theirs: string;
  busy: boolean;
  onKeepMine: () => void;
  onTakeTheirs: () => void;
}) {
  return (
    <div className="border-destructive/40 bg-destructive/5 flex flex-col gap-2 rounded-md border p-2.5">
      <p className="text-xs leading-5">
        We changed the shipped text after you edited this. Yours is still what runs; nothing was
        reverted for you.
      </p>
      <div className="grid gap-2 sm:grid-cols-2">
        <div className="border-border/60 bg-background/50 rounded-md border p-2">
          <p className="af-label mb-1">Yours</p>
          <p className="text-xs leading-5 whitespace-pre-wrap">{mine}</p>
        </div>
        <div className="border-border/60 bg-background/50 rounded-md border p-2">
          <p className="af-label mb-1">Ours, now</p>
          <p className="text-xs leading-5 whitespace-pre-wrap">{theirs}</p>
        </div>
      </div>
      <div className="flex flex-wrap gap-2">
        <Button size="sm" variant="outline" disabled={busy} onClick={onKeepMine}>
          Keep mine
        </Button>
        <Button size="sm" variant="outline" disabled={busy} onClick={onTakeTheirs}>
          Take the new one
        </Button>
      </div>
    </div>
  );
}
