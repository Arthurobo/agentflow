// run.ts — shared run state-machine vocabulary (mirrors backend internal/
// spawner: starting, running, awaiting, finished, stopped, crashed + blocked).
export type RunState =
  | "starting"
  | "running"
  | "awaiting"
  | "finished"
  | "stopped"
  | "crashed";

export const runStates: RunState[] = [
  "starting",
  "running",
  "awaiting",
  "finished",
  "stopped",
  "crashed",
];

export function isTerminal(state: string): boolean {
  return state === "finished" || state === "stopped" || state === "crashed";
}

export function runStateLabel(state: string): string {
  const map: Record<string, string> = {
    starting: "Starting",
    running: "Running",
    awaiting: "Awaiting",
    finished: "Finished",
    stopped: "Stopped",
    crashed: "Crashed",
  };
  return map[state] ?? state;
}

// A "live" state is one where the process still has activity to surface.
export function isLiveState(state: string): boolean {
  return state === "starting" || state === "running" || state === "awaiting";
}

// Tool presets for the Run-agent form (scoped autonomy: a tool missing from
// --allowedTools is auto-denied; Edit passes under acceptEdits).
export const allowedToolsOptions = [
  "Read",
  "Edit",
  "Write",
  "Bash",
  "Glob",
  "Grep",
  "NotebookEdit",
  "Task",
];

// Permission modes the spawner passes through (manual aliases default on this
// CLI). bypassPermissions is the product default: managed runs
// get full session parity — every tool available, nothing denied or stuck on
// unanswerable headless prompts.
export const permissionModes = [
  {
    value: "bypassPermissions",
    label: "allow all",
    description: "every tool runs without prompts (full parity)",
  },
  {
    value: "acceptEdits",
    label: "acceptEdits",
    description: "edits auto-pass, other tools need allowlisting",
  },
  {
    value: "default",
    label: "default",
    description: "asks on every non-allowed tool",
  },
  {
    value: "plan",
    label: "plan",
    description: "read-only unless allowed",
  },
];

export const models = [
  "",
  "claude-opus-5",
  "claude-sonnet-5",
  "claude-haiku-4-5",
];
