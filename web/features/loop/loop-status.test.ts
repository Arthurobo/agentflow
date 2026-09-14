// loop-status.test.ts — the board resolves to exactly ONE sentence. A screen
// that shows four true things at once does not tell the engineer which one to
// act on, and waiting-on-you is the only state that needs him at all.
import { describe, expect, it } from "vitest";
import { loopStatus } from "./loop-board";
import type { LaneState } from "./projection";
import type { Loop } from "@/lib/loop";

const NOW = 1_700_000_000_000;

function loop(over: Partial<Loop> = {}): Loop {
  return {
    id: "loop_1", title: "Fix it", task: "Fix it", cwd: "/tmp", status: "active", round: 2,
    state: "", parentLoopId: "", endReason: "", play: "build", playStatus: "running",
    stepId: "review", stepAwaiting: [], stepEnteredAt: NOW, createdAt: NOW, completedAt: 0,
    ...over,
  };
}

function lane(over: Partial<LaneState> & { role: string }): LaneState {
  return {
    agentId: "a", status: "active", tool: "claude", model: "", charsIn: 0, notes: 0,
    holding: false, awaited: false, state: "working", external: false, runId: "run_1", lastError: "",
    strandedSince: 0, oldestPendingAt: 0,
    stalledBriefs: 0, lastActivity: 0, ...over,
  };
}

describe("loopStatus", () => {
  it("puts waiting-on-you above everything else", () => {
    // A stall is also true here, and it is not what he needs to read.
    const out = loopStatus(loop(), [
      lane({ role: "ENGINEER", awaited: true }),
      lane({ role: "REVIEW", stalledBriefs: 3 }),
    ], "REVIEW");
    expect(out.tone).toBe("you");
    expect(out.text).toMatch(/waiting on you/i);
  });

  it("reports a stall with its count when nothing needs him", () => {
    const out = loopStatus(loop(), [lane({ role: "REVIEW", stalledBriefs: 2 })], "REVIEW");
    expect(out.tone).toBe("stalled");
    expect(out.text).toMatch(/2 briefs/);
  });

  it("says who holds the step and which round it is", () => {
    const out = loopStatus(loop(), [lane({ role: "REVIEW", holding: true })], "REVIEW");
    expect(out.tone).toBe("running");
    expect(out.text).toMatch(/REVIEW holds review, round 2/);
  });

  it("falls back to the step actor when no lane claims the step", () => {
    const out = loopStatus(loop(), [lane({ role: "REVIEW" })], "IMPLEMENTATION");
    expect(out.text).toMatch(/IMPLEMENTATION holds review/);
  });

  // An ended loop is not running, whatever the lanes still say.
  it("reports an ended loop over everything", () => {
    const out = loopStatus(
      loop({ status: "complete", endReason: "review reported clean" }),
      [lane({ role: "ENGINEER", awaited: true }), lane({ role: "REVIEW", stalledBriefs: 9 })],
      "REVIEW",
    );
    expect(out.tone).toBe("ended");
    expect(out.text).toMatch(/review reported clean/);
  });

  it("uses the singular for one stalled brief", () => {
    const out = loopStatus(loop(), [lane({ role: "REVIEW", stalledBriefs: 1 })], "");
    expect(out.text).toMatch(/1 brief handed/);
  });
});
