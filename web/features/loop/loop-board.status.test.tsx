// loop-board.status.test.tsx — the board is a STATUS BOARD and nothing else.
// A member is a link to its TERMINAL, which is the only place a session is
// read now that the chat is gone; and a refusal, which used to be a card in
// that chat, has to land here because there is nowhere else for it to go.
import { render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LoopBoard, OPEN_POLL_MS } from "./loop-board";
import { getLoop, listPlays, listTools, LoopError, loopMessages, loopRefusals } from "@/lib/loop";

vi.mock("./loop-actions", () => ({ LoopActions: () => <div data-testid="actions" /> }));
vi.mock("next/link", () => ({
  default: ({ children, href }: { children: React.ReactNode; href: string }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("@/lib/loop", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/loop")>()),
  getLoop: vi.fn(),
  listPlays: vi.fn(),
  listTools: vi.fn(),
  loopMessages: vi.fn(),
  loopRefusals: vi.fn(),
}));

const PLAY = {
  name: "audit", title: "Audit", purpose: "why", entry: "brief", maxRounds: 3, hasCycle: false,
  roles: ["ORCHESTRATOR", "INVESTIGATION"],
  steps: [
    { id: "brief", actor: "ORCHESTRATOR", note: "brief the investigator", fanIn: false },
    { id: "investigate", actor: "INVESTIGATION", note: "research and report", fanIn: false },
    { id: "conclude", actor: "ORCHESTRATOR", note: "act on the findings", fanIn: false },
  ],
};

const LOOP = {
  id: "loop_1", task: "Fix the export", cwd: "/tmp", status: "active", round: 1,
  state: "", parentLoopId: "", endReason: "", play: "audit", playStatus: "running",
  stepId: "investigate", stepAwaiting: [], stepEnteredAt: 0, createdAt: 0, completedAt: 0,
};

const member = (role: string, over: Record<string, unknown> = {}) => ({
  role, agentId: role.toLowerCase(), status: "active", tool: "claude", model: "opus",
  charsIn: 0, notes: 0, lastSeenAt: 0, runId: "run-" + role, sessionId: "sess-" + role,
  lastError: "",
  // The server's verdict. Absent it, the row falls back to the process
  // questions, which is what an older agentd produces.
  health: "working", listening: false, listeningSince: 0, held: 0, pending: 0,
  oldestPendingAt: 0, strandedSince: 0, runState: "running",
  ...over,
});

// The roster and the play diagram both name the roles, so every assertion
// about a MEMBER is scoped to the roster.
const roster = () => within(screen.getByRole("list", { name: "members" }));

function renderBoard() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LoopBoard loopId="loop_1" />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(getLoop).mockReset();
  vi.mocked(getLoop).mockResolvedValue({
    loop: LOOP as never,
    crew: [member("INVESTIGATION"), member("ORCHESTRATOR")],
    messageCounts: {}, stepBrief: "", stepNote: "", stepActor: "", maxRounds: 0,
  });
  vi.mocked(listPlays).mockResolvedValue([PLAY as never]);
  vi.mocked(listTools).mockResolvedValue([]);
  vi.mocked(loopMessages).mockResolvedValue([]);
  vi.mocked(loopRefusals).mockReset();
  vi.mocked(loopRefusals).mockResolvedValue([]);
});

describe("the board does one job", () => {
  it("renders no conversation: no composer, no tabs, no message bodies", async () => {
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    expect(screen.queryByRole("tab", { name: /chat/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: /terminal/i })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/^message /)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /send as engineer/i })).not.toBeInTheDocument();
  });

  // The terminal is the session. A member row that pointed anywhere else
  // would be pointing at a page that no longer exists.
  it("makes every member a link to its own terminal", async () => {
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    expect(roster().getByText("INVESTIGATION").closest("a")).toHaveAttribute(
      "href",
      "/session/?id=run-INVESTIGATION",
    );
    expect(roster().getByText("ORCHESTRATOR").closest("a")).toHaveAttribute(
      "href",
      "/session/?id=run-ORCHESTRATOR",
    );
    expect(document.querySelectorAll('[aria-pressed="true"]')).toHaveLength(0);
  });

  // A member with no run has no terminal. Linking it anyway would send him to
  // a 404; saying nothing would leave him tapping a row that does not respond.
  it("does not link a member that has no terminal, and says which kind it is", async () => {
    vi.mocked(getLoop).mockResolvedValue({
      loop: LOOP as never,
      crew: [
        member("INVESTIGATION", { runId: "", sessionId: "" }),
        member("ORCHESTRATOR", { runId: "run-x", sessionId: "sess-x" }),
      ],
      messageCounts: {}, stepBrief: "", stepNote: "", stepActor: "", maxRounds: 0,
    });
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    expect(roster().getByText("INVESTIGATION").closest("a")).toBeNull();
    expect(roster().getByText("runs elsewhere")).toBeInTheDocument();
    expect(roster().getByText("ORCHESTRATOR").closest("a")).toHaveAttribute("href", "/session/?id=run-x");
  });

  // A refusal is the loop declining to proceed. It was a card in the chat;
  // losing it with the chat would have made a stuck loop mysterious again.
  it("surfaces refusals, newest first, with the step that refused", async () => {
    vi.mocked(loopRefusals).mockResolvedValue([
      { id: "r1", loopId: "loop_1", kind: "step.refused", content: "REVIEW may not speak yet",
        fromRole: "REVIEW", toRole: "ORCHESTRATOR", stepId: "investigate", detail: {}, createdAt: 10 },
      { id: "r2", loopId: "loop_1", kind: "round.capped", content: "three rounds is the cap",
        fromRole: "", toRole: "", stepId: "", detail: {}, createdAt: 20 },
    ] as never);
    renderBoard();
    const box = within(await screen.findByRole("region", { name: "refusals" }));
    expect(box.getByText("three rounds is the cap")).toBeInTheDocument();
    expect(box.getByText("REVIEW may not speak yet")).toBeInTheDocument();
    expect(box.getByText("investigate")).toBeInTheDocument();
    const kinds = box.getAllByText(/step\.refused|round\.capped/).map((n) => n.textContent);
    expect(kinds).toEqual(["round.capped", "step.refused"]);
  });

  it("says nothing at all when the loop has refused nothing", async () => {
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    expect(screen.queryByRole("region", { name: "refusals" })).not.toBeInTheDocument();
  });

  // A fan-out role carries a "#" and the run id is built from it. Unencoded,
  // the "#" would start a URL fragment and the terminal would open the wrong
  // (truncated) id.
  it("encodes a fan-out role into the id query", async () => {
    vi.mocked(getLoop).mockResolvedValue({
      loop: LOOP as never,
      crew: [member("INVESTIGATION#2")],
      messageCounts: {}, stepBrief: "", stepNote: "", stepActor: "", maxRounds: 0,
    });
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    expect(roster().getByText("INVESTIGATION#2").closest("a")).toHaveAttribute(
      "href",
      "/session/?id=run-INVESTIGATION%232",
    );
  });

  it("shows each member's state, what it holds and what waits on it", async () => {
    vi.mocked(getLoop).mockResolvedValue({
      loop: { ...LOOP, stepAwaiting: ["INVESTIGATION"] } as never,
      crew: [
        member("INVESTIGATION"),
        member("ORCHESTRATOR", { runId: "", sessionId: "", health: "external" }),
      ],
      messageCounts: {}, stepBrief: "", stepNote: "", stepActor: "", maxRounds: 0,
    });
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    // INVESTIGATION holds the current step of the audit play
    expect(roster().getByText("holding")).toBeInTheDocument();
    // and the orchestrator has no process, which the row says
    expect(screen.getByTitle(/this host did not start it/i)).toBeInTheDocument();
  });

  // The verdict the board could not previously make: a member with a live
  // process that has stopped reading its mail. It used to read "running"
  // forever, because a session id is true once and then true always.
  it("says when a member with a live session is not reading its mail", async () => {
    vi.mocked(getLoop).mockResolvedValue({
      loop: LOOP as never,
      crew: [
        member("INVESTIGATION", { health: "stranded", pending: 1, strandedSince: 10 }),
        member("ORCHESTRATOR", { health: "listening", listening: true }),
      ],
      messageCounts: {}, stepBrief: "", stepNote: "", stepActor: "", maxRounds: 0,
    });
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    expect(screen.getByTitle(/not polling and is holding nothing/i)).toBeInTheDocument();
    // And the status line leads with it, above anything else true right now.
    const status = await screen.findByRole("status", { name: "loop status" });
    expect(status).toHaveTextContent(/INVESTIGATION is not reading its mail/i);
  });

  // Working past the lease is amber, never red: it may still be thinking, and
  // a badge that shouts at a busy agent is one nobody reads twice.
  it("does not call a member working past its lease a failure", async () => {
    vi.mocked(getLoop).mockResolvedValue({
      loop: LOOP as never,
      crew: [member("INVESTIGATION", { health: "overdue", held: 1 })],
      messageCounts: {}, stepBrief: "", stepNote: "", stepActor: "", maxRounds: 0,
    });
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    const chip = roster().getByText("INVESTIGATION").closest("a")?.querySelector("[data-state]");
    expect(chip).toHaveAttribute("data-state", "overdue");
    const status = await screen.findByRole("status", { name: "loop status" });
    expect(status).not.toHaveTextContent(/not reading its mail/i);
  });

  // The one question the page exists to answer.
  it("keeps the status line", async () => {
    renderBoard();
    // Named, because a loading EmptyState is also a status region and a page
    // with two unnamed ones tells a screen reader nothing about which is which.
    expect(await screen.findByRole("status", { name: "loop status" })).toHaveTextContent(/running/i);
  });

  it("still says when the loop cannot be loaded", async () => {
    vi.mocked(getLoop).mockRejectedValue(new LoopError(404, "not_found", "no route"));
    renderBoard();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/could not load this loop/i);
    expect(screen.queryByText(/^No such loop$/)).not.toBeInTheDocument();
  });

  it("still polls while a member is starting and stops when none is", async () => {
    vi.mocked(getLoop).mockResolvedValue({
      loop: LOOP as never,
      crew: [member("ORCHESTRATOR", { sessionId: "", health: "starting" })],
      messageCounts: {}, stepBrief: "", stepNote: "", stepActor: "", maxRounds: 0,
    });
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    const first = vi.mocked(getLoop).mock.calls.length;
    await waitFor(() => expect(vi.mocked(getLoop).mock.calls.length).toBeGreaterThan(first), {
      timeout: 8000,
    });
  }, 12000);
});

// Nothing pushes loop changes to the board, so an open loop must be reread on
// a timer or the board silently goes stale; an ended loop must not be.
describe("keeping the board current", () => {
  afterEach(() => vi.useRealTimers());

  it("rereads an open loop, its messages and its refusals", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderBoard();
    await screen.findByRole("list", { name: "members" });
    const loops = vi.mocked(getLoop).mock.calls.length;
    const msgs = vi.mocked(loopMessages).mock.calls.length;
    const refusals = vi.mocked(loopRefusals).mock.calls.length;
    await vi.advanceTimersByTimeAsync(OPEN_POLL_MS + 50);
    expect(vi.mocked(getLoop).mock.calls.length).toBeGreaterThan(loops);
    expect(vi.mocked(loopMessages).mock.calls.length).toBeGreaterThan(msgs);
    expect(vi.mocked(loopRefusals).mock.calls.length).toBeGreaterThan(refusals);
  });

  it("stops rereading once the loop has ended", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.mocked(getLoop).mockResolvedValue({
      loop: { ...LOOP, status: "ended" } as never,
      crew: [member("INVESTIGATION", { status: "retired", runState: "stopped" })],
      messageCounts: {}, stepBrief: "", stepNote: "", stepActor: "", maxRounds: 0,
    });
    renderBoard();
    await waitFor(() => expect(getLoop).toHaveBeenCalled());
    await vi.advanceTimersByTimeAsync(100);
    const loops = vi.mocked(getLoop).mock.calls.length;
    await vi.advanceTimersByTimeAsync(OPEN_POLL_MS * 3);
    expect(vi.mocked(getLoop).mock.calls.length).toBe(loops);
  });
});
