// engine.ts — shared engine identity vocabulary for the frontend. Mirrors
// internal/engine on the daemon. One source of truth so the list row, run header, loop card, and
// control sheet all render the same badge.

export type EngineId = "claude" | "opencode";

export const ENGINE_IDS: EngineId[] = ["claude", "opencode"];

export function normaliseEngine(value: string | undefined | null): EngineId {
  return value === "opencode" ? "opencode" : "claude";
}

export interface EngineMeta {
  id: EngineId;
  // displayName is what shows in the UI; never localized beyond a fixed
  // short string (the badge always renders this verbatim).
  displayName: string;
  // mark is the glyph (kept ASCII for monospace tables).
  mark: string;
  // accent is the badge's color class — the engine color bar on the
  // sessions list reads this too, so the badge and the bar stay in sync.
  accent: "orange" | "violet";
}

const ENGINE_META: Record<EngineId, EngineMeta> = {
  claude: {
    id: "claude",
    displayName: "Claude Code",
    mark: "C",
    accent: "orange",
  },
  opencode: {
    id: "opencode",
    displayName: "OpenCode",
    mark: "O",
    accent: "violet",
  },
};

export function engineMeta(id: string | undefined | null): EngineMeta {
  return ENGINE_META[normaliseEngine(id)];
}