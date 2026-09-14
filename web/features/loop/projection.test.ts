// projection.test.ts — the board is a projection over the rows. These pin the
// stall signal, the play diagram, the per-member lanes and what a member's
// state actually is, which is the half of this file the chat's removal left
// standing.
import { describe, expect, it } from "vitest";
import type { CrewMember, Loop, LoopMessage, Play } from "@/lib/loop";
import { DEFAULT_STALL_MS, diagram, isStalled, lanes, memberState } from "./projection";

const NOW = 1_700_000_000_000;

function msg(over: Partial<LoopMessage> = {}): LoopMessage {
  return {
    id: "msg_1",
    loopId: "loop_1",
    senderRole: "ORCHESTRATOR",
    recipientRole: "INVESTIGATION",
    subject: "",
    body: "go and look",
    bodyChars: 11,
    status: "pending",
    deliveryCount: 0,
    forwardedFrom: "",
    mirrorOf: "",
    outcome: "",
    stepId: "",
    createdAt: NOW - 1000,
    deliveredAt: 0,
    ackedAt: 0,
    ...over,
  };
}

describe("the stall signal", () => {
  it("is a delivered brief that has sat past the lease, and nothing else", () => {
    const held = msg({ status: "delivered", deliveredAt: NOW - DEFAULT_STALL_MS - 1 });
    expect(isStalled(held, NOW)).toBe(true);
    expect(isStalled(msg({ status: "delivered", deliveredAt: NOW - 1000 }), NOW)).toBe(false);
    expect(isStalled(msg({ status: "acked", deliveredAt: 1 }), NOW)).toBe(false);
    expect(isStalled(msg({ status: "pending", deliveredAt: 0 }), NOW)).toBe(false);
  });
});

const play: Play = {
  name: "audit",
  title: "Audit",
  purpose: "Ask one question and have the answer attacked before you act on it.",
  entry: "brief",
  maxRounds: 3,
  hasCycle: false,
  roles: ["ORCHESTRATOR", "INVESTIGATION", "REVIEW"],
  steps: [
    { id: "brief", actor: "ORCHESTRATOR", note: "brief the investigator", fanIn: false },
    { id: "investigate", actor: "INVESTIGATION", note: "research and report", fanIn: false },
    { id: "route", actor: "ORCHESTRATOR", note: "forward verbatim", fanIn: false },
    { id: "review", actor: "REVIEW", note: "attack the findings", fanIn: false },
    { id: "conclude", actor: "ORCHESTRATOR", note: "act on it", fanIn: false },
  ],
};

function loopAt(stepId: string, over: Partial<Loop> = {}): Loop {
  return {
    id: "loop_1",
    title: "Flaky guest export",
    task: "Fix the flaky guest export",
    cwd: "/tmp",
    status: "active",
    round: 0,
    state: "",
    parentLoopId: "",
    endReason: "",
    play: "audit",
    playStatus: "running",
    stepId,
    stepAwaiting: [],
    stepEnteredAt: NOW,
    createdAt: NOW,
    completedAt: 0,
    ...over,
  };
}

describe("diagram", () => {
  it("lights the current step and marks what is behind it", () => {
    const steps = diagram(loopAt("route", { stepAwaiting: ["ORCHESTRATOR"] }), play);
    expect(steps.map((s) => s.mark)).toEqual(["done", "done", "current", "ahead", "ahead"]);
    expect(steps[2].awaiting).toEqual(["ORCHESTRATOR"]);
    expect(steps[3].awaiting).toEqual([]);
  });

  it("lights nothing once the play is finished", () => {
    const steps = diagram(loopAt("conclude", { playStatus: "done" }), play);
    expect(steps.some((s) => s.mark === "current")).toBe(false);
  });

  it("is empty when no play is loaded", () => {
    expect(diagram(loopAt("brief"), undefined)).toEqual([]);
  });
});

describe("lanes", () => {
  const member = (over: Partial<CrewMember> & { role: string; agentId: string }): CrewMember => ({
    status: "active", tool: "claude", model: "", charsIn: 0, notes: 0, lastSeenAt: 0,
    runId: "", sessionId: "", lastError: "",
    health: "", listening: false, listeningSince: 0, held: 0, pending: 0,
    oldestPendingAt: 0, strandedSince: 0, runState: "",
    ...over,
  });
  const crew: CrewMember[] = [
    member({ role: "ORCHESTRATOR", agentId: "o1", model: "opus", charsIn: 900, lastSeenAt: NOW, runId: "run-o1" }),
    member({ role: "INVESTIGATION", agentId: "i1", model: "sonnet", charsIn: 120, notes: 2, lastSeenAt: NOW, runId: "run-i1" }),
    member({ role: "REVIEW", agentId: "r1", status: "idle", tool: "opencode", model: "some-model" }),
  ];

  it("marks who holds the step and who it is waiting for", () => {
    const out = lanes(
      loopAt("investigate", { stepAwaiting: ["INVESTIGATION"] }),
      crew,
      [],
      play,
      NOW,
    );
    const inv = out.find((l) => l.role === "INVESTIGATION")!;
    expect(inv.holding).toBe(true);
    expect(inv.awaited).toBe(true);
    expect(out.find((l) => l.role === "REVIEW")!.holding).toBe(false);
  });

  it("calls a member this host never started external, because it has no terminal", () => {
    const out = lanes(loopAt("brief"), crew, [], play, NOW);
    // ORCHESTRATOR and INVESTIGATION carry run ids in the fixture; REVIEW does not.
    expect(out.find((l) => l.role === "ORCHESTRATOR")!.external).toBe(false);
    expect(out.find((l) => l.role === "REVIEW")!.external).toBe(true);
  });

  it("counts the briefs a member was handed and never acked", () => {
    const out = lanes(
      loopAt("investigate"),
      crew,
      [
        msg({ id: "s1", recipientRole: "INVESTIGATION", status: "delivered", deliveredAt: NOW - DEFAULT_STALL_MS - 1 }),
        msg({ id: "s2", recipientRole: "INVESTIGATION", status: "acked", deliveredAt: NOW - DEFAULT_STALL_MS - 1 }),
        msg({ id: "s3", recipientRole: "REVIEW", status: "delivered", deliveredAt: NOW - 10 }),
      ],
      play,
      NOW,
    );
    expect(out.find((l) => l.role === "INVESTIGATION")!.stalledBriefs).toBe(1);
    expect(out.find((l) => l.role === "REVIEW")!.stalledBriefs).toBe(0);
  });

  it("carries the roster numbers the surface reports", () => {
    const out = lanes(loopAt("brief"), crew, [], play, NOW);
    const orch = out.find((l) => l.role === "ORCHESTRATOR")!;
    expect(orch.charsIn).toBe(900);
    expect(out.find((l) => l.role === "INVESTIGATION")!.notes).toBe(2);
    expect(out.find((l) => l.role === "REVIEW")!.tool).toBe("opencode");
  });

  // The board used to build this from a role-to-run map, which answered the
  // question wrongly: a run id exists from ROW creation, so it called a member
  // "ours" before anything had spawned.
  it("marks a member we started as ours, and one we did not as external", () => {
    const out = lanes(loopAt("investigate"), crew, [], undefined, NOW);
    const byRole = Object.fromEntries(out.map((l) => [l.role, l]));
    expect(byRole.ORCHESTRATOR.external).toBe(false);
    expect(byRole.INVESTIGATION.external).toBe(false);
    // no run id was passed for REVIEW, so it is running somewhere else
    expect(byRole.REVIEW.external).toBe(true);
  });

  it("carries why a member has no process", () => {
    const broken = crew.map((m) =>
      m.role === "REVIEW" ? { ...m, lastError: "executable file not found in $PATH" } : m,
    );
    const out = lanes(loopAt("investigate"), broken, [], undefined, NOW);
    expect(out.find((l) => l.role === "REVIEW")?.lastError).toMatch(/not found/);
    expect(out.find((l) => l.role === "ORCHESTRATOR")?.lastError).toBe("");
  });
});

// runId alone says nothing about whether a process exists: handlers.go assigns
// it when the ROW is created. Reading it as "started" is what made the board
// claim a terminal for a member that had not spawned.
describe("memberState", () => {
  it("reads a failure first, because it explains itself", () => {
    expect(memberState({ runId: "r", sessionId: "s", lastError: "boom" })).toBe("failed");
    expect(memberState({ lastError: "boom" })).toBe("failed");
  });

  // A session id proves a PROCESS. It never proved that anything was reading
  // the mailbox, which is what the board was using it to claim: two live
  // investigators reported, stopped polling, and read as running all day.
  it("takes the server's verdict over a session id", () => {
    expect(memberState({ runId: "r", sessionId: "s", health: "stranded" })).toBe("stranded");
    expect(memberState({ runId: "r", sessionId: "s", health: "listening" })).toBe("listening");
    expect(memberState({ runId: "r", sessionId: "s", health: "overdue" })).toBe("overdue");
  });

  it("falls back to the process questions when there is no verdict yet", () => {
    expect(memberState({ runId: "r", sessionId: "s" })).toBe("working");
    expect(memberState({ runId: "r", sessionId: "s", health: "" })).toBe("working");
  });

  // A failure is about whether there is anything to read the mail at all, so
  // it outranks a verdict computed about a process that never started.
  it("still reads a failure first", () => {
    expect(memberState({ runId: "r", lastError: "boom", health: "listening" })).toBe("failed");
  });

  it("treats a run id with no session as still coming up", () => {
    expect(memberState({ runId: "r" })).toBe("starting");
  });

  it("treats no run id at all as running somewhere else", () => {
    expect(memberState({})).toBe("external");
    expect(memberState({ sessionId: "" , runId: "" })).toBe("external");
  });
});

describe("isStalled", () => {
  const PENDING_MS = 60 * 60 * 1000;

  it("counts a delivered brief that has sat past the lease", () => {
    expect(isStalled(msg({ status: "delivered", deliveredAt: NOW - PENDING_MS }), NOW)).toBe(true);
  });

  // The one that was missed. A brief nobody ever claimed does not match a
  // check that requires a claim, so an orchestrator that polled once and
  // never again read as healthy for an hour.
  it("counts a pending message nobody ever picked up", () => {
    expect(isStalled(msg({ status: "pending", createdAt: NOW - PENDING_MS, deliveredAt: 0 }), NOW)).toBe(
      true,
    );
  });

  it("does not count mail that only just arrived", () => {
    expect(isStalled(msg({ status: "pending", createdAt: NOW - 1000, deliveredAt: 0 }), NOW)).toBe(false);
  });

  it("does not count what has been dealt with", () => {
    expect(isStalled(msg({ status: "acked", deliveredAt: NOW - PENDING_MS }), NOW)).toBe(false);
    expect(isStalled(msg({ status: "cancelled", deliveredAt: NOW - PENDING_MS }), NOW)).toBe(false);
  });
});
