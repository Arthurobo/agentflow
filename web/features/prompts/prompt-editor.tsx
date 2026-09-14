"use client";

// prompt-editor.tsx — one editor, reached from three places. It is scoped to
// what the engineer is already looking at rather than living in a settings
// section he has to go and find: the play he is about to run, the rules his
// member is following, the block his next solo session will carry.
//
// PROSE ONLY. Briefs, notes, purpose, title, rule text and the solo intro.
// Nothing structural: no edges, guards, fan-in, fan-out, loop flags, entry,
// round caps, step ids, or adding and removing steps. Those are exactly the
// edits that trip the validator, and a validator error is least useful on a
// 390px screen. Structure stays in the file, on a laptop.

import { useMemo } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertCircle, Archive, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { EmptyState, SectionHeader } from "@/components/ui/surface";
import {
  deleteOverride,
  getPrompts,
  overrideFor,
  putOverride,
  OVERRIDE_ORPHANED,
  PromptError,
  type OverrideTarget,
  type PromptOverride,
} from "@/lib/prompts";
import { LeafEditor } from "./leaf-editor";

export type EditorScope =
  | { kind: "play"; play: string }
  | { kind: "rules"; role?: string }
  | { kind: "solo" };

export function PromptEditor({ scope, onClose }: { scope: EditorScope; onClose?: () => void }) {
  const qc = useQueryClient();
  const prompts = useQuery({ queryKey: ["prompts"], queryFn: getPrompts });

  const save = useMutation({
    mutationFn: ({ target, value }: { target: OverrideTarget; value: string }) =>
      putOverride(target, value),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["prompts"] }),
  });
  const revert = useMutation({
    mutationFn: (id: string) => deleteOverride(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["prompts"] }),
  });

  const data = prompts.data;
  const overrides = useMemo(() => data?.overrides ?? [], [data]);

  // leafKey remounts a leaf when the SERVER's copy of it changes, which is
  // what seeds a fresh draft after a save, a revert or a take-the-new-one
  // without an effect mirroring props into state.
  const leafKey = (target: OverrideTarget, current: string) => {
    const o = overrideFor(overrides, target);
    return [target.kind, target.play, target.step, target.rule, target.field, o?.id ?? "", o?.updatedAt ?? 0, current].join("|");
  };

  const leafProps = (target: OverrideTarget, shipped: string, current: string, binding?: boolean) => {
    const o = overrideFor(overrides, target);
    return {
      target,
      shipped,
      current,
      binding,
      override: o,
      onSave: async (value: string) => {
        await save.mutateAsync({ target, value });
      },
      onRevert: async () => {
        if (o) await revert.mutateAsync(o.id);
      },
    };
  };

  if (prompts.isLoading) {
    return <EmptyState loading title="Loading the prompts" />;
  }
  if (prompts.isError || !data) {
    return (
      <div
        role="alert"
        className="border-destructive/40 bg-destructive/5 text-destructive flex flex-wrap items-center gap-2 rounded-md border px-3 py-2 text-sm"
      >
        <AlertCircle className="size-4 shrink-0" />
        <span className="min-w-0">
          Could not load the prompts:{" "}
          {prompts.error instanceof PromptError ? prompts.error.message : "the surface did not answer"}
        </span>
        <Button size="sm" variant="outline" className="ml-auto" onClick={() => void prompts.refetch()}>
          Retry
        </Button>
      </div>
    );
  }

  const orphans = overrides.filter((o) => o.status === OVERRIDE_ORPHANED);

  return (
    <div className="flex flex-col gap-4">
      <SectionHeader
        eyebrow="Prompts"
        title={titleFor(scope, data.plays.find((p) => p.Name === (scope as { play?: string }).play)?.Title)}
        description="Your edits apply to this machine and to every device paired with it. Structure stays in the file; this is the prose."
        actions={
          onClose ? (
            <Button size="sm" variant="outline" onClick={onClose}>
              <X className="size-3" /> Close
            </Button>
          ) : undefined
        }
      />

      {scope.kind === "play" && (
        <PlayLeaves scope={scope} data={data} leafProps={leafProps} leafKey={leafKey} />
      )}

      {scope.kind === "rules" && (
        <div className="flex flex-col gap-3">
          {ruleGroupsFor(scope.role).map((group) => {
            const rules = data.rules[group] ?? [];
            const shipped = data.defaultRules[group] ?? [];
            if (rules.length === 0) return null;
            return (
              <div key={group} className="flex flex-col gap-2">
                <p className="af-label">{GROUP_LABEL[group] ?? group}</p>
                {rules.map((r) => {
                  const def = shipped.find((x) => x.name === r.name);
                  return (
                    <LeafEditor
                      key={leafKey({ kind: "rule", rule: r.name, field: "text" }, r.text)}
                      label={r.name}
                      hint={r.binding ? undefined : "Replaces the shipped text entirely."}
                      {...leafProps(
                        { kind: "rule", rule: r.name, field: "text" },
                        def?.text ?? r.text,
                        r.text,
                        r.binding,
                      )}
                    />
                  );
                })}
              </div>
            );
          })}
        </div>
      )}

      {scope.kind === "solo" && (
        <LeafEditor
          key={leafKey({ kind: "solo", field: "intro" }, data.solo.Intro)}
          label="Solo session opening"
          hint="What a session with no loop is told before its rules. The rules it carries are set in the file."
          {...leafProps({ kind: "solo", field: "intro" }, data.solo.Intro, data.solo.Intro)}
        />
      )}

      {orphans.length > 0 && <Orphans orphans={orphans} onDrop={(id) => void revert.mutateAsync(id)} />}
    </div>
  );
}

const GROUP_LABEL: Record<string, string> = {
  shared: "Every member",
  worker: "Workers only",
  orchestrator: "The orchestrator only",
  solo: "Sessions with no loop",
};

function ruleGroupsFor(role?: string): string[] {
  const base = ["shared"];
  if (!role) return [...base, "worker", "orchestrator", "solo"];
  if (role.toUpperCase().startsWith("ORCHESTRATOR")) return [...base, "orchestrator"];
  if (role.toUpperCase().startsWith("ENGINEER")) return base;
  return [...base, "worker"];
}

function titleFor(scope: EditorScope, playTitle?: string): string {
  if (scope.kind === "play") return `${playTitle ?? scope.play}: what each step asks for`;
  if (scope.kind === "rules") return "The standing rules";
  return "The solo session block";
}

function PlayLeaves({
  scope,
  data,
  leafProps,
  leafKey,
}: {
  scope: { kind: "play"; play: string };
  data: NonNullable<ReturnType<typeof useQuery<Awaited<ReturnType<typeof getPrompts>>>>["data"]>;
  leafProps: (
    target: OverrideTarget,
    shipped: string,
    current: string,
    binding?: boolean,
  ) => Omit<React.ComponentProps<typeof LeafEditor>, "label" | "hint">;
  leafKey: (target: OverrideTarget, current: string) => string;
}) {
  const play = data.plays.find((p) => p.Name === scope.play);
  const shipped = data.defaultPlays.find((p) => p.Name === scope.play);
  const droppedReason = data.dropped[scope.play];

  if (!play) {
    return (
      <div className="border-destructive/40 bg-destructive/5 flex flex-col gap-2 rounded-md border p-3 text-sm">
        <p className="text-destructive flex items-start gap-1.5">
          <AlertCircle className="mt-0.5 size-4 shrink-0" />
          <span>
            {droppedReason
              ? `This play is not being offered: ${droppedReason}`
              : "This play is not in the catalogue."}
          </span>
        </p>
        <p className="text-muted-foreground text-xs leading-5">
          Its edits are still here. Reverting the one that broke it puts the play back.
        </p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <LeafEditor
        key={leafKey({ kind: "play", play: play.Name, field: "title" }, play.Title)}
        label="Title"
        hint="What this play is called on the card you pick it from."
        {...leafProps(
          { kind: "play", play: play.Name, field: "title" },
          shipped?.Title ?? play.Title,
          play.Title,
        )}
      />
      <LeafEditor
        key={leafKey({ kind: "play", play: play.Name, field: "purpose" }, play.Purpose)}
        label="Purpose"
        hint="One sentence saying what this play is for. Shown on the card when you pick it."
        {...leafProps(
          { kind: "play", play: play.Name, field: "purpose" },
          shipped?.Purpose ?? play.Purpose,
          play.Purpose,
        )}
      />
      {play.Steps.map((s) => {
        const def = shipped?.Steps.find((x) => x.ID === s.ID);
        return (
          <div key={s.ID} className="flex flex-col gap-2">
            <p className="af-label">
              {s.ID} · {s.Actor}
            </p>
            <LeafEditor
              key={leafKey({ kind: "play", play: play.Name, step: s.ID, field: "brief" }, s.Brief)}
              label="Brief"
              hint="What the member holding this step is told to do, delivered when the step opens."
              {...leafProps(
                { kind: "play", play: play.Name, step: s.ID, field: "brief" },
                def?.Brief ?? s.Brief,
                s.Brief,
              )}
            />
            <LeafEditor
              key={leafKey({ kind: "play", play: play.Name, step: s.ID, field: "note" }, s.Note)}
              label="Note"
              hint="The short label for this step, shown on the board's play diagram. Not sent to anyone."
              {...leafProps(
                { kind: "play", play: play.Name, step: s.ID, field: "note" },
                def?.Note ?? s.Note,
                s.Note,
              )}
            />
          </div>
        );
      })}
    </div>
  );
}

// Orphans are edits whose anchor is gone: a step we renamed or removed. We do
// not delete an engineer's writing because we changed our own file, so they
// are listed with their text and he decides.
function Orphans({
  orphans,
  onDrop,
}: {
  orphans: PromptOverride[];
  onDrop: (id: string) => void;
}) {
  return (
    <div className="af-panel flex flex-col gap-2 rounded-lg p-3">
      <p className="flex items-center gap-1.5 text-sm font-medium">
        <Archive className="size-3.5" /> Edits with nowhere to go
      </p>
      <p className="text-muted-foreground text-xs leading-5">
        These were written against something that no longer exists, so they are not applied. They
        are kept because they are yours.
      </p>
      {orphans.map((o) => (
        <div key={o.id} className="border-border/60 rounded-md border p-2">
          <p className="af-label mb-1">
            {o.kind === "rule" ? o.rule : [o.play, o.step].filter(Boolean).join(" · ")} · {o.field}
          </p>
          <p className="text-xs leading-5 whitespace-pre-wrap">{o.value}</p>
          <Button size="sm" variant="outline" className="mt-2" onClick={() => onDrop(o.id)}>
            Discard
          </Button>
        </div>
      ))}
    </div>
  );
}
