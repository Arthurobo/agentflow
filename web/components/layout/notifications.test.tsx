// notifications.test.tsx — run lifecycle notifications come from polling the
// run list; there is no event stream behind them any more.
import { render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { RunSession } from "@/lib/agentd";

const listRuns = vi.fn();
const notifyLocal = vi.fn().mockResolvedValue(true);

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  listRuns: () => listRuns(),
}));
vi.mock("@/lib/notify", () => ({
  notifyLocal: (...a: unknown[]) => notifyLocal(...a),
  registerServiceWorker: vi.fn().mockResolvedValue(null),
}));

import { NotificationCenter, POLL_MS, runTransitions } from "./notifications";

function run(over: Partial<RunSession>): RunSession {
  return {
    id: "run-1",
    sessionId: "",
    kind: "tty",
    cwd: "",
    project: "",
    model: "",
    prompt: "",
    resumeFrom: "",
    state: "running",
    blocked: false,
    approvalsEnabled: false,
    pid: 0,
    startedAt: 0,
    endedAt: 0,
    exitCode: 0,
    eventCount: 0,
    permissionDenials: 0,
    totalCostUsd: 0,
    terminalReason: "",
    lastError: "",
    createdBy: "",
    updatedAt: 0,
    engine: "claude",
    controlPort: 0,
    controlBase: "",
    title: "Fix the export",
    ...over,
  };
}

describe("runTransitions", () => {
  it("announces a run that finished or crashed", () => {
    const prev = new Map([
      ["a", run({ id: "a" })],
      ["b", run({ id: "b" })],
    ]);
    const out = runTransitions(prev, [
      run({ id: "a", state: "finished" }),
      run({ id: "b", state: "crashed" }),
    ]);
    expect(out.map((n) => n.title)).toEqual(["Session finished", "Session crashed"]);
    expect(out[0].body).toContain("Fix the export");
  });

  it("says nothing about runs it has not seen before, or that did not change", () => {
    const prev = new Map([["a", run({ id: "a" })]]);
    expect(
      runTransitions(prev, [run({ id: "a" }), run({ id: "new", state: "finished" })]),
    ).toEqual([]);
  });
});

describe("NotificationCenter", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    listRuns.mockReset();
    notifyLocal.mockClear();
  });
  afterEach(() => vi.useRealTimers());

  it("polls the run list and notifies on a transition", async () => {
    listRuns
      .mockResolvedValueOnce({ items: [run({ state: "running" })] })
      .mockResolvedValue({ items: [run({ state: "finished" })] });
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(
      <QueryClientProvider client={client}>
        <NotificationCenter />
      </QueryClientProvider>,
    );
    await vi.advanceTimersByTimeAsync(0);
    expect(listRuns).toHaveBeenCalledTimes(1);
    expect(notifyLocal).not.toHaveBeenCalled();
    expect(invalidate).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(listRuns).toHaveBeenCalledTimes(2);
    expect(notifyLocal).toHaveBeenCalledWith(
      "Session finished",
      expect.stringContaining("Fix the export"),
      "run-run-1",
    );
    // the sessions list drops the run's live dot straight away
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["all-sessions"] });
  });
});
