// projection.ts — the board is a PROJECTION over the rows, never a second
// store. Nothing here holds state: member rows and message rows go in, member
// status comes out, and every fact is read off a row rather than tracked
// alongside it.
//
// This file used to also build chat cards from those same rows. The chat is
// gone; what remains is the half that answers "is this loop healthy, and is
// anything waiting on me", which the terminal cannot answer because it shows
// one member at a time.

import type { CrewMember, Loop, LoopMessage, Play } from "@/lib/loop";

export const ROLE_ORCHESTRATOR = "ORCHESTRATOR";
export const ROLE_ENGINEER = "ENGINEER";
export const ROLE_ENGINE = "ENGINE";

// baseRole strips the fan-out suffix: INVESTIGATION#2 is an INVESTIGATION.
export function baseRole(role: string): string {
  const up = (role ?? "").trim().toUpperCase();
  const hash = up.indexOf("#");
  return hash > 0 ? up.slice(0, hash) : up;
}

/** DEFAULT_STALL_MS: a brief held this long without an ack is what a stall
 *  looks like. It matches the mailbox's default 30 minute lease. */
export const DEFAULT_STALL_MS = 30 * 60 * 1000;

/** isStalled: this message is not moving.
 *
 *  Two shapes, and only one of them used to count. A DELIVERED brief past the
 *  lease is a member that took it and went quiet. A PENDING message past the
 *  same window is worse: nothing ever picked it up. The Flowing PRs loop had
 *  its entry brief pending for an hour while the board said "Running", because
 *  a message nobody claimed did not match a check that required a claim. */
export function isStalled(m: LoopMessage, now: number, stallMs = DEFAULT_STALL_MS): boolean {
  if (m.status === "pending") {
    return !!m.createdAt && now - m.createdAt >= stallMs;
  }
  if (m.status !== "delivered") return false;
  if (!m.deliveredAt) return false;
  return now - m.deliveredAt >= stallMs;
}

/** REFUSAL_KINDS are the refusal rows the board surfaces. Enforcement being
 *  visible is what makes a stuck loop legible instead of mysterious; with the
 *  chat gone the board is the only place left that can say it. */
export const REFUSAL_KINDS = ["step.refused", "round.capped", "route.refused"];

// --- the board ---------------------------------------------------------------------

/** MemberState is what is actually true about a member's process, derived from
 *  fields already on the wire. runId alone says nothing: handlers.go assigns it
 *  when the ROW is created, before anything is spawned. */
export type MemberState =
  | "failed"
  | "starting"
  | "external"
  | "listening"
  | "working"
  | "overdue"
  | "stranded";

/** memberState answers "is anyone reading this member's mail".
 *
 *  The process questions come first because they are about whether there is
 *  anything to read the mail AT ALL: a spawn that failed, or one still in
 *  flight, is not a mailbox problem. Past those, the answer is the SERVER's:
 *  only agentd knows which long polls it is holding, so the client renders
 *  that verdict rather than inventing one.
 *
 *  It used to say "running" the moment a session id existed, which is true
 *  forever once it is true once. Two live investigators reported their work,
 *  stopped polling, and read as running for the rest of the day. */
export function memberState(m: {
  runId?: string;
  sessionId?: string;
  lastError?: string;
  health?: string;
}): MemberState {
  if (m.lastError) return "failed";
  switch (m.health) {
    case "listening":
    case "working":
    case "overdue":
    case "stranded":
    case "starting":
    case "external":
      return m.health;
  }
  // No verdict yet: the row exists and the process does not, or this agentd
  // is older than the health fields.
  if (m.sessionId) return "working";
  if (m.runId) return "starting";
  return "external";
}

export const MEMBER_STATE_LABEL: Record<MemberState, string> = {
  failed: "did not start",
  starting: "starting",
  external: "elsewhere",
  listening: "waiting for work",
  working: "working",
  overdue: "working, past its lease",
  stranded: "not reading its mail",
};

/** ATTENTION_STATES are the ones the engineer has to do something about. */
export const ATTENTION_STATES: MemberState[] = ["failed", "stranded"];

export interface LaneState {
  role: string;
  agentId: string;
  status: string;
  tool: string;
  model: string;
  charsIn: number;
  notes: number;
  /** holding: this member is the actor of the current step. */
  holding: boolean;
  /** awaited: the step will not advance until this member acts. */
  awaited: boolean;
  /** state: what is true about this member's process right now. */
  state: MemberState;
  /** external: this host did not start it, so it has no terminal here. */
  external: boolean;
  /** runId: the terminal to open for this member, empty when there is none. */
  runId: string;
  /** strandedSince: when the server first decided nobody reads its mail. */
  strandedSince: number;
  /** oldestPendingAt: when the unread message it is ignoring arrived. */
  oldestPendingAt: number;
  /** lastError: why it has no process. Empty for a member that started. */
  lastError: string;
  /** stalledBriefs: briefs handed to it and never acked. */
  stalledBriefs: number;
  lastActivity: number;
}

// lanes derives every member fact from the member's own row. It used to take a
// role-to-run map built by the caller, which was a second source of truth for
// the same question and, worse, answered it wrongly: a run id exists from row
// creation, so that map called a member "ours" before anything had spawned.
export function lanes(
  loop: Loop,
  crew: CrewMember[],
  messages: LoopMessage[],
  play: Play | undefined,
  now: number,
  stallMs = DEFAULT_STALL_MS,
): LaneState[] {
  const step = play?.steps.find((s) => s.id === loop.stepId);
  const awaiting = new Set((loop.stepAwaiting ?? []).map(baseRole));
  return crew.map((m) => {
    const mine = messages.filter(
      (x) => baseRole(x.recipientRole) === baseRole(m.role) || baseRole(x.senderRole) === baseRole(m.role),
    );
    return {
      role: m.role,
      agentId: m.agentId,
      status: m.status,
      tool: m.tool,
      model: m.model,
      charsIn: m.charsIn,
      notes: m.notes,
      holding: !!step && baseRole(step.actor) === baseRole(m.role) && loop.playStatus === "running",
      awaited: awaiting.has(baseRole(m.role)),
      state: memberState(m),
      strandedSince: m.strandedSince ?? 0,
      oldestPendingAt: m.oldestPendingAt ?? 0,
      external: !m.runId,
      runId: m.runId ?? "",
      lastError: m.lastError,
      stalledBriefs: mine.filter(
        (x) => baseRole(x.recipientRole) === baseRole(m.role) && isStalled(x, now, stallMs),
      ).length,
      lastActivity: mine.reduce((a, x) => Math.max(a, x.createdAt), 0),
    };
  });
}

export type StepMark = "done" | "current" | "ahead";

export interface DiagramStep {
  id: string;
  actor: string;
  note: string;
  fanIn: boolean;
  mark: StepMark;
  awaiting: string[];
}

/** diagram lights the current step. Steps before it in the play's own order
 *  read as done, which is honest for the linear plays and approximate for a
 *  cycle, where the round counter is the real progress signal. */
export function diagram(loop: Loop, play: Play | undefined): DiagramStep[] {
  if (!play) return [];
  const at = play.steps.findIndex((s) => s.id === loop.stepId);
  return play.steps.map((s, i) => {
    let mark: StepMark = "ahead";
    if (at >= 0 && i < at) mark = "done";
    if (at >= 0 && i === at) mark = loop.playStatus === "running" ? "current" : "done";
    return {
      id: s.id,
      actor: s.actor,
      note: s.note,
      fanIn: s.fanIn,
      mark,
      awaiting: mark === "current" ? (loop.stepAwaiting ?? []) : [],
    };
  });
}
