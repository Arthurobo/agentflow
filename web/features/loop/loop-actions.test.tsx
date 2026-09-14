// loop-actions.test.tsx — the verbs the loop surface exposed and the board
// never offered. Every one of these endpoints had a client function that
// nothing called, or no client function at all.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { LoopActions } from "./loop-actions";
import { addMember, continueLoop, dismissLoop, endLoop, LoopError, type Loop } from "@/lib/loop";

vi.mock("@/lib/loop", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/loop")>()),
  endLoop: vi.fn(),
  dismissLoop: vi.fn(),
  continueLoop: vi.fn(),
  addMember: vi.fn(),
}));

const NOW = 1_700_000_000_000;

function loop(over: Partial<Loop> = {}): Loop {
  return {
    id: "loop_1",
    title: "Flaky guest export",
    task: "Fix the flaky guest export",
    cwd: "/tmp",
    status: "active",
    round: 1,
    state: "",
    parentLoopId: "",
    endReason: "",
    play: "audit",
    playStatus: "running",
    stepId: "review",
    stepAwaiting: [],
    stepEnteredAt: NOW,
    createdAt: NOW,
    completedAt: 0,
    ...over,
  };
}

const PLAYS = [
  { name: "audit", title: "Audit", purpose: "why you would pick Audit", entry: "brief", maxRounds: 3, hasCycle: false, roles: [], steps: [] },
  { name: "patch", title: "Patch", purpose: "why you would pick Patch", entry: "brief", maxRounds: 0, hasCycle: false, roles: [], steps: [] },
];

const TOOLS = [
  {
    id: "claude",
    title: "Claude Code",
    models: ["opus", "haiku"],
    probed: true,
    defaultModelAllowed: true,
    note: "",
  },
];

function renderActions(over: Partial<Loop> = {}) {
  const onChanged = vi.fn();
  render(<LoopActions loop={loop(over)} plays={PLAYS} tools={TOOLS} onChanged={onChanged} />);
  return { onChanged };
}

beforeEach(() => {
  vi.mocked(endLoop).mockReset();
  vi.mocked(dismissLoop).mockReset();
  vi.mocked(continueLoop).mockReset();
  vi.mocked(addMember).mockReset();
});

describe("ending", () => {
  it("parks the crew by default, because their tokens are still worth something", async () => {
    const user = userEvent.setup();
    vi.mocked(endLoop).mockResolvedValue({ loopId: "loop_1", status: "complete", agents: "parked" });
    const { onChanged } = renderActions();

    await user.click(screen.getByRole("button", { name: /end loop/i }));
    await user.type(screen.getByPlaceholderText(/review reported clean/i), "done");
    // The button says which of the two things it is about to do.
    await user.click(screen.getByRole("button", { name: /end and park the crew/i }));

    await waitFor(() => expect(endLoop).toHaveBeenCalled());
    expect(vi.mocked(endLoop).mock.calls[0][1]).toEqual({ reason: "done", dismissAgents: false });
    expect(onChanged).toHaveBeenCalled();
  });

  it("retires them only when the engineer ticks it", async () => {
    const user = userEvent.setup();
    vi.mocked(endLoop).mockResolvedValue({ loopId: "loop_1", status: "complete", agents: "dismissed" });
    renderActions();

    await user.click(screen.getByRole("button", { name: /end loop/i }));
    expect(screen.getByRole("button", { name: /end and park the crew/i })).toBeInTheDocument();
    await user.click(screen.getByLabelText(/retire the agents too/i));
    // ticking it changes what the button promises, not just what it sends
    await user.click(screen.getByRole("button", { name: /end and retire the crew/i }));

    await waitFor(() => expect(endLoop).toHaveBeenCalled());
    expect(vi.mocked(endLoop).mock.calls[0][1].dismissAgents).toBe(true);
  });

  it("surfaces a refusal rather than looking like it worked", async () => {
    const user = userEvent.setup();
    vi.mocked(endLoop).mockRejectedValue(new LoopError(409, "loop_ended", "loop has ended"));
    renderActions();
    await user.click(screen.getByRole("button", { name: /end loop/i }));
    await user.click(screen.getByRole("button", { name: /end and park the crew/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/loop has ended \(loop_ended\)/);
  });
});

describe("retiring every agent", () => {
  // Ending parks; dismissing retires. They are different endpoints and the
  // harsher one only shows once the loop is over.
  it("is offered on an ended loop and not on a live one", async () => {
    const user = userEvent.setup();
    vi.mocked(dismissLoop).mockResolvedValue({ loopId: "loop_1", dismissed: 3 });

    const live = render(<LoopActions loop={loop()} plays={PLAYS} tools={TOOLS} onChanged={vi.fn()} />);
    expect(screen.queryByRole("button", { name: /retire all agents/i })).not.toBeInTheDocument();
    live.unmount();

    const { onChanged } = renderActions({ status: "complete" });
    await user.click(screen.getByRole("button", { name: /retire all agents/i }));
    await waitFor(() => expect(dismissLoop).toHaveBeenCalledWith("loop_1"));
    expect(onChanged).toHaveBeenCalled();
  });
});

describe("continuing", () => {
  it("carries the crew and reports who kept their token", async () => {
    const user = userEvent.setup();
    vi.mocked(continueLoop).mockResolvedValue({
      loop: loop({ id: "loop_2" }),
      continuedFrom: "loop_1",
      keptAgents: ["REVIEW", "INVESTIGATION"],
      notesCarried: 4,
      handoffs: [
        { role: "IMPLEMENTATION", agentId: "i1", tool: "claude", model: "opus", token: "af_new", prompt: "You are IMPLEMENTATION." },
      ],
    });
    renderActions();

    await user.click(screen.getByRole("button", { name: /continue/i }));
    await user.type(screen.getByPlaceholderText(/fix the review/i), "Round two");
    await user.click(screen.getByRole("button", { name: /patch/i }));
    await user.click(screen.getByRole("button", { name: /start the successor/i }));

    await waitFor(() => expect(continueLoop).toHaveBeenCalled());
    expect(vi.mocked(continueLoop).mock.calls[0][1]).toEqual({
      task: "Round two",
      play: "patch",
      carryNotes: true,
    });
    // Adopted members keep their tokens, so only the NEW role gets a prompt.
    expect(await screen.findByText(/2 agents kept their tokens, 4 notes carried over/i)).toBeInTheDocument();
    expect(screen.getByText(/You are IMPLEMENTATION\./)).toBeInTheDocument();
  });

  it("will not start a successor with no task", async () => {
    const user = userEvent.setup();
    renderActions();
    await user.click(screen.getByRole("button", { name: /continue/i }));
    expect(screen.getByRole("button", { name: /start the successor/i })).toBeDisabled();
    expect(continueLoop).not.toHaveBeenCalled();
  });
});

describe("adding a member", () => {
  it("sends the engine on the wire field the backend reads", async () => {
    const user = userEvent.setup();
    vi.mocked(addMember).mockResolvedValue({
      role: "REVIEW#2",
      agentId: "r2",
      tool: "claude",
      model: "haiku",
      token: "af_tok",
      prompt: "You are REVIEW#2.",
    });
    renderActions();

    await user.click(screen.getByRole("button", { name: /add member/i }));
    await user.type(screen.getByPlaceholderText(/INVESTIGATION#2/), "review#2");
    await user.click(screen.getByLabelText("new member model"));
    await user.click(await screen.findByText("haiku"));
    await user.click(screen.getAllByRole("button", { name: /^add member$/i })[1]);

    await waitFor(() => expect(addMember).toHaveBeenCalled());
    // The role input uppercases as you type: a role is an address.
    expect(vi.mocked(addMember).mock.calls[0][1]).toEqual({
      role: "REVIEW#2",
      tool: "claude",
      model: "haiku",
    });
    expect(await screen.findByText(/You are REVIEW#2\./)).toBeInTheDocument();
  });

});

// The backend refuses a member on an ended loop with a raw error. Continue
// stays enabled, because continuing is what you do with an ended loop.
describe("an ended loop", () => {
  it("stops taking new members and says why", () => {
    renderActions({ status: "complete" });
    const add = screen.getByRole("button", { name: /add member/i });
    expect(add).toBeDisabled();
    expect(add).toHaveAttribute("title", expect.stringMatching(/has ended/i));
    expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled();
  });

  it("still takes new members while it is live", () => {
    renderActions();
    expect(screen.getByRole("button", { name: /add member/i })).toBeEnabled();
  });
});