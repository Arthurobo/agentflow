"use client";

// play-card.tsx — picking a play is the decision that shapes the whole loop,
// and it used to look like picking a radio button: every card rendered the
// same, and selection changed a border tint and a background wash. On a phone
// at arm's length, with the grid half scrolled, that is not a signal.
//
// So selection changes the CARD, not its colour. The chosen one expands to
// carry the play's shape, the roles it will staff and whether it cycles, and
// spans the grid; the others collapse to a title and one line. The difference
// is size and content, which reads from across a room and does not depend on
// seeing a hue correctly in sunlight.

import { Check, Play as PlayIcon, Repeat, Users } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { SelectedPill } from "@/components/ui/surface";
import { selectionClass } from "@/lib/selection";
import { cn } from "@/lib/utils";
import type { Play } from "@/lib/loop";

/** spawnedRoles are the members a play actually starts. ENGINEER is an
 *  address rather than a process, so it is never one of them. */
export function spawnedRoles(play: Play): string[] {
  return (play.roles ?? []).filter((r) => r.toUpperCase() !== "ENGINEER");
}

/** stepChain is the play's shape: the steps in order with who holds each. A
 *  play whose steps did not come down the wire falls back to its role chain,
 *  which is the same question answered less precisely. */
export function stepChain(play: Play): { id: string; actor: string }[] {
  if (play.steps && play.steps.length > 0) {
    return play.steps.map((s) => ({ id: s.id, actor: s.actor }));
  }
  return (play.roles ?? []).map((r) => ({ id: "", actor: r }));
}

export function PlayCard({
  play,
  selected,
  edited,
  onSelect,
}: {
  play: Play;
  selected: boolean;
  edited?: boolean;
  onSelect: () => void;
}) {
  const roles = spawnedRoles(play);
  const chain = stepChain(play);

  return (
    <button
      type="button"
      aria-pressed={selected}
      data-selected={selected ? "yes" : "no"}
      onClick={onSelect}
      className={cn(
        "af-panel rounded-lg text-left transition",
        // The shared idiom, plus the size change that is this card's own
        // content signal.
        selectionClass(selected),
        selected ? "p-3 sm:col-span-2" : "p-2.5",
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        {selected ? (
          <SelectedPill>
            <Check className="size-3" /> Selected
          </SelectedPill>
        ) : (
          <PlayIcon className="text-muted-foreground size-3.5 shrink-0" />
        )}
        <span className={cn("font-semibold", selected ? "text-base" : "text-sm")}>{play.title}</span>
        {play.hasCycle && (
          <Badge variant="outline" className="gap-1">
            <Repeat className="size-2.5" /> cycles up to {play.maxRounds}
          </Badge>
        )}
        {edited && <Badge variant="outline">edited</Badge>}
      </div>

      {play.purpose && (
        <p
          className={cn(
            "text-muted-foreground mt-1 leading-5",
            selected ? "text-sm" : "line-clamp-1 text-xs",
          )}
        >
          {play.purpose}
        </p>
      )}

      {/* The shape, and only on the chosen card. An unselected card showing
          the same chain was most of why they all looked alike. */}
      {selected && (
        <div className="mt-3 flex flex-col gap-2">
          <div>
            <p className="af-label mb-1">What happens, in order</p>
            <ol className="flex flex-wrap items-center gap-1">
              {chain.map((s, i) => (
                <li key={`${s.id}-${i}`} className="flex items-center gap-1">
                  {i > 0 && <span className="text-muted-foreground/60 text-[10px]">›</span>}
                  <span className="border-border/60 bg-background/60 rounded-md border px-1.5 py-0.5 font-mono text-xs">
                    <span className="font-semibold">{s.actor}</span>
                    {s.id && <span className="text-muted-foreground"> {s.id}</span>}
                  </span>
                </li>
              ))}
            </ol>
          </div>
          <p className="text-muted-foreground flex flex-wrap items-center gap-1.5 text-xs">
            <Users className="size-3 shrink-0" />
            <span>
              Staffs <span className="text-foreground font-semibold">{roles.length}</span>{" "}
              {roles.length === 1 ? "agent" : "agents"}: {roles.join(", ")}
            </span>
          </p>
          {play.hasCycle && (
            <p className="text-muted-foreground text-xs leading-5">
              Implementation and review trade until the review reports clean, up to {play.maxRounds}{" "}
              rounds.
            </p>
          )}
        </div>
      )}
    </button>
  );
}
