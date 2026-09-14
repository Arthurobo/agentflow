"use client";

// session-filters.tsx — the pickers above the sessions list: project, engine,
// model and state. They narrow the sessions already loaded; the page keeps
// the search box and the list.
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ENGINE_IDS, engineMeta } from "@/lib/engine";
import type {
  EngineFilter,
  FilterOption,
  SessionFilterValues,
  StateFilter,
} from "@/lib/sessions";

interface Props {
  value: SessionFilterValues;
  onChange: (next: SessionFilterValues) => void;
  projects: FilterOption[];
  // models follows the chosen engine; with none, the model picker is hidden
  // rather than offering an empty list.
  models: FilterOption[];
}

const ALL = "all";
const orAll = (v: string) => v || ALL;
const fromAll = (v: string) => (v === ALL ? "" : v);

export function SessionFilters({ value, onChange, projects, models }: Props) {
  // Changing engine clears the model, so an OpenCode model never stays
  // selected while filtering to Claude Code and empties the list silently.
  const onEngine = (v: string) =>
    onChange({ ...value, engine: fromAll(v) as EngineFilter, model: "" });

  // A selected model the loaded rows no longer carry stays listed, so the
  // picker never shows a value it can't display.
  const modelChoices =
    value.model && !models.some((m) => m.name === value.model)
      ? [{ name: value.model, count: 0 }, ...models]
      : models;

  return (
    // On a phone the pickers pair up two per row; sm:contents dissolves the
    // grid on wider screens so they flow inline with the search box.
    <div className="grid grid-cols-2 gap-2 sm:contents">
      <Select
        value={orAll(value.project)}
        onValueChange={(v) => onChange({ ...value, project: fromAll(v) })}
      >
        <SelectTrigger className="sm:w-44" aria-label="Project filter">
          <SelectValue placeholder="All projects" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={ALL}>All projects</SelectItem>
          {value.project && !projects.some((p) => p.name === value.project) && (
            <SelectItem value={value.project}>{value.project}</SelectItem>
          )}
          {projects.map((p) => (
            <SelectItem key={p.name} value={p.name}>
              {p.name} ({p.count})
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <Select value={orAll(value.engine)} onValueChange={onEngine}>
        <SelectTrigger className="sm:w-40" aria-label="Engine filter">
          <SelectValue placeholder="All engines" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={ALL}>All engines</SelectItem>
          {ENGINE_IDS.map((id) => (
            <SelectItem key={id} value={id}>
              {engineMeta(id).displayName}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {modelChoices.length > 0 && (
        <Select
          value={orAll(value.model)}
          onValueChange={(v) => onChange({ ...value, model: fromAll(v) })}
        >
          <SelectTrigger className="sm:w-44" aria-label="Model filter">
            <SelectValue placeholder="All models" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL}>All models</SelectItem>
            {modelChoices.map((m) => (
              <SelectItem key={m.name} value={m.name}>
                {m.count ? `${m.name} (${m.count})` : m.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      <Select
        value={orAll(value.state)}
        onValueChange={(v) => onChange({ ...value, state: fromAll(v) as StateFilter })}
      >
        <SelectTrigger className="sm:w-32" aria-label="Session state filter">
          <SelectValue placeholder="All states" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={ALL}>All states</SelectItem>
          <SelectItem value="active">Active</SelectItem>
          <SelectItem value="idle">Idle</SelectItem>
        </SelectContent>
      </Select>
    </div>
  );
}
