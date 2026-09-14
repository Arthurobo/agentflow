// sessions.ts — helpers for the sessions home list.
import type { AllSessionItem } from "@/lib/agentd";
import { normaliseEngine, type EngineId } from "@/lib/engine";
import { isLiveState } from "@/lib/run";

// SESSIONS_PAGE_SIZE is how many sessions one list request asks for.
export const SESSIONS_PAGE_SIZE = 100;

// sessionLabel is what a session is called in the list: its own name, then
// the project it belongs to, then its short id.
export function sessionLabel(s: Pick<AllSessionItem, "id" | "title" | "project">): string {
  return s.title.trim() || s.project.trim() || s.id.slice(0, 8);
}

// matchesSearch filters on what a row shows: its name and where it ran.
export function matchesSearch(s: AllSessionItem, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return [s.title, s.cwd, s.project].some((v) => v.toLowerCase().includes(q));
}

// EngineFilter is the engine picker's value: "" shows both engines.
export type EngineFilter = "" | EngineId;

// StateFilter narrows by the run's own lifecycle: "active" while a run holds
// the session (starting, running or awaiting input), "idle" otherwise.
export type StateFilter = "" | "active" | "idle";

// SessionFilterValues is what the filters above the list hold; "" means all.
export interface SessionFilterValues {
  project: string;
  engine: EngineFilter;
  model: string;
  state: StateFilter;
}

export const NO_FILTERS: SessionFilterValues = { project: "", engine: "", model: "", state: "" };

// hasFilters reports whether any filter narrows the list.
export function hasFilters(f: SessionFilterValues): boolean {
  return Boolean(f.project || f.engine || f.model || f.state);
}

// matchesEngine keeps the rows of the chosen engine. A row with no engine
// is a Claude Code session, as it is everywhere else in the UI.
export function matchesEngine(s: Pick<AllSessionItem, "engine">, engine: EngineFilter): boolean {
  return !engine || normaliseEngine(s.engine) === engine;
}

// matchesFilters applies every filter to one row.
export function matchesFilters(
  s: Pick<AllSessionItem, "engine" | "model" | "project" | "state">,
  f: SessionFilterValues,
): boolean {
  if (f.project && s.project !== f.project) return false;
  if (!matchesEngine(s, f.engine)) return false;
  if (f.model && s.model !== f.model) return false;
  if (f.state === "active" && !isLiveState(s.state)) return false;
  if (f.state === "idle" && isLiveState(s.state)) return false;
  return true;
}

// filterOption is one choice in a project or model picker, with how many
// loaded rows carry it.
export interface FilterOption {
  name: string;
  count: number;
}

// projectOptions lists the projects among the loaded rows, most sessions
// first, then by name.
export function projectOptions(rows: Pick<AllSessionItem, "project">[]): FilterOption[] {
  return countBy(rows.map((r) => r.project));
}

// modelOptions lists the models among the loaded rows of the chosen engine.
// The model list follows the engine: Claude's model names and OpenCode's
// provider/model ids are two vocabularies, and one mixed list gives no way to
// tell which is which. Empty when no row reports a model.
export function modelOptions(
  rows: Pick<AllSessionItem, "engine" | "model">[],
  engine: EngineFilter,
): FilterOption[] {
  return countBy(rows.filter((r) => matchesEngine(r, engine)).map((r) => r.model));
}

function countBy(values: string[]): FilterOption[] {
  const counts = new Map<string, number>();
  for (const v of values) {
    if (v) counts.set(v, (counts.get(v) ?? 0) + 1);
  }
  return [...counts]
    .map(([name, count]) => ({ name, count }))
    .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
}
