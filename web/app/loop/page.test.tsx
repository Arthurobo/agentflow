// page.test.tsx — the loops index. An API failure used to render as the same
// cheerful "No loops yet" as a successful empty response, so a 401 or an
// offline agentd looked like a feature nobody had used yet.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import LoopsPage from "./page";
import { listLoopsWithAttention, LoopError, type Loop } from "@/lib/loop";

vi.mock("@/lib/loop", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/loop")>()),
  listLoopsWithAttention: vi.fn(),
}));

// The wizard has registries of its own; this file is about the page around it.
vi.mock("@/features/loop/wizard", () => ({
  LoopWizard: ({ onCreated }: { onCreated?: (id: string) => void }) => (
    <div data-testid="wizard">
      the wizard
      <button type="button" onClick={() => onCreated?.("loop_new")}>
        pretend to create
      </button>
    </div>
  ),
}));

const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

vi.mock("next/link", () => ({
  default: ({ children, href }: { children: React.ReactNode; href: string }) => (
    <a href={href}>{children}</a>
  ),
}));

const LOOP: Loop = {
  id: "loop_1",
  title: "Flaky guest export",
  task: "Fix the flaky guest export",
  cwd: "/tmp",
  status: "active",
  round: 2,
  state: "",
  parentLoopId: "",
  endReason: "",
  play: "audit",
  playStatus: "running",
  stepId: "review",
  stepAwaiting: [],
  stepEnteredAt: 0,
  createdAt: 0,
  completedAt: 0,
};

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LoopsPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  push.mockReset();
  vi.mocked(listLoopsWithAttention).mockReset();
});

describe("the loops index", () => {
  it("says the list failed to load instead of showing an empty state", async () => {
    vi.mocked(listLoopsWithAttention).mockRejectedValue(
      new LoopError(401, "unauthorized", "invalid or revoked device token"),
    );
    renderPage();

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/could not load your loops/i);
    expect(alert).toHaveTextContent(/invalid or revoked device token/);
    expect(alert).toHaveTextContent(/unauthorized/);
    expect(screen.queryByText(/no loops yet/i)).not.toBeInTheDocument();
  });

  // A failure is not emptiness, so it must not be answered with a create form.
  it("does not open the wizard on an error", async () => {
    vi.mocked(listLoopsWithAttention).mockRejectedValue(new LoopError(0, "unreachable", "agentd unreachable"));
    renderPage();
    await screen.findByRole("alert");
    expect(screen.queryByTestId("wizard")).not.toBeInTheDocument();
  });

  // A page whose only content is a button reads as an unbuilt feature. This is
  // how the play picker went unnoticed while it was deployed and working.
  it("opens the wizard by itself when there is genuinely nothing to look at", async () => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({ loops: [], attention: {} });
    renderPage();
    expect(await screen.findByTestId("wizard")).toBeInTheDocument();
  });

  it("leaves the wizard closed when there are loops to read", async () => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({ loops: [LOOP], attention: {} });
    renderPage();
    // The row shows the NAME. It used to show the task, which is why the task
    // had to be a name, which is why nothing was ever instructed.
    expect(await screen.findByText("Flaky guest export")).toBeInTheDocument();
    expect(screen.queryByText("Fix the flaky guest export")).not.toBeInTheDocument();
    expect(screen.queryByTestId("wizard")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: /flaky guest export/i })).toHaveAttribute(
      "href",
      "/loop/board/?id=loop_1",
    );
  });

  it("lets the engineer close the wizard it opened, and open it again", async () => {
    const user = userEvent.setup();
    vi.mocked(listLoopsWithAttention).mockResolvedValue({ loops: [], attention: {} });
    renderPage();
    await screen.findByTestId("wizard");

    await user.click(screen.getByRole("button", { name: /close/i }));
    await waitFor(() => expect(screen.queryByTestId("wizard")).not.toBeInTheDocument());
    // and the empty state is what is left, not a blank page
    expect(screen.getByText(/no loops yet/i)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /start a loop/i }));
    expect(await screen.findByTestId("wizard")).toBeInTheDocument();
  });

  // The whole instruction went to the orchestrator, so that is the lane the
  // engineer lands on. Lane zero is only the orchestrator by accident of
  // insertion order.
  it("sends the engineer to the board after creating, not to a terminal", async () => {
    const user = userEvent.setup();
    vi.mocked(listLoopsWithAttention).mockResolvedValue({ loops: [], attention: {} });
    renderPage();
    await screen.findByTestId("wizard");

    await user.click(screen.getByRole("button", { name: /pretend to create/i }));
    // The board, not a terminal: at this instant no member has a process, so
    // every terminal address he could be sent to would be a dead PTY dial.
    // The board polls while members are starting and links each one the
    // moment it exists.
    expect(push).toHaveBeenCalledWith("/loop/board/?id=loop_new");
  });

  // Both of these were on screen at the two moments he is paying most
  // attention, and both stopped being true when members started being spawned.
  it("does not tell him to paste anything", async () => {
    const user = userEvent.setup();
    vi.mocked(listLoopsWithAttention).mockResolvedValue({ loops: [], attention: {} });
    renderPage();
    // The empty state is behind the auto-opened wizard, so close it first:
    // checking the body before that proves nothing.
    await screen.findByTestId("wizard");
    await user.click(screen.getByRole("button", { name: /close/i }));
    expect(await screen.findByText(/no loops yet/i)).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/paste/i);
  });

  it("leads with what a loop does before how the engine enforces it", async () => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({ loops: [LOOP], attention: {} });
    renderPage();
    const description = await screen.findByText(/start a crew of agents on one task/i);
    // the enforcement sentence stays, second
    expect(description.textContent).toMatch(/engine owns the sequence/i);
    expect(description.textContent!.indexOf("Start a crew")).toBeLessThan(
      description.textContent!.indexOf("engine owns the sequence"),
    );
  });
});
describe("the attention badge", () => {
  // Four open loops all read "active", which said nothing about which one had
  // stopped moving. This is the row saying which.
  it("names the member that stopped reading its mail", async () => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({
      loops: [LOOP],
      attention: { [LOOP.id]: { stranded: ["INVESTIGATION"], failed: [] } },
    });
    renderPage();
    expect(await screen.findByText(/INVESTIGATION not reading its mail/i)).toBeInTheDocument();
  });

  it("names a member that never started", async () => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({
      loops: [LOOP],
      attention: { [LOOP.id]: { stranded: [], failed: ["REVIEW"] } },
    });
    renderPage();
    expect(await screen.findByText(/REVIEW did not start/i)).toBeInTheDocument();
  });

  // A healthy loop must say nothing at all. A badge that is always present is
  // a badge nobody looks at.
  //
  // Both shapes, because they fail independently: a server that omits the
  // entry, and a server that sends an empty one. Testing only the first
  // leaves the empty-badge case unobserved on both sides at once.
  it.each([
    ["the entry is absent", {}],
    ["the entry is present but empty", { loop_1: { stranded: [], failed: [] } }],
  ])("says nothing about a loop that needs nothing: %s", async (_name, attention) => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({
      loops: [LOOP],
      attention: attention as never,
    });
    renderPage();
    await screen.findByText("Flaky guest export");
    expect(screen.queryByText(/not reading its mail|did not start/i)).not.toBeInTheDocument();
    expect(document.querySelectorAll(".border-destructive\\/45")).toHaveLength(0);
  });
});

describe("what a row says a loop is doing", () => {
  // A play reaching its terminal step does not end the loop, by design: the
  // engineer may continue it. But the row read "active · recon · conclude ·
  // round 0", which is what a working loop looks like, so two finished loops
  // sat in the list indistinguishable from two running ones.
  it("says the play finished rather than calling it active", async () => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({
      loops: [{ ...LOOP, playStatus: "done", stepId: "conclude" }],
      attention: {},
    });
    renderPage();
    expect(await screen.findByText("play finished, loop open")).toBeInTheDocument();
  });

  it("still says active for a play that is still running", async () => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({
      loops: [{ ...LOOP, playStatus: "running" }],
      attention: {},
    });
    renderPage();
    expect(await screen.findByText("active")).toBeInTheDocument();
    expect(screen.queryByText(/play finished/)).not.toBeInTheDocument();
  });

  // An ended loop says how it ended; the play's own state is beside the point.
  it("says how an ended loop ended, whatever its play did", async () => {
    vi.mocked(listLoopsWithAttention).mockResolvedValue({
      loops: [{ ...LOOP, status: "complete", playStatus: "done" }],
      attention: {},
    });
    renderPage();
    expect(await screen.findByText("complete")).toBeInTheDocument();
  });
});
