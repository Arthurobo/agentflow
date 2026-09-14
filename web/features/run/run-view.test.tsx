// run-view.test.tsx — a session page is the terminal and nothing else. There is no
// second reading of a session and no tab to choose between: the TUI mounts
// immediately and everything around it is chrome on that one pane.
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { RunView, runHeaderTitle } from "./run-view";
import { getEngineCaps, listEngineCommands, runStatus } from "@/lib/agentd";

// The terminal is mocked down to the one thing this file is about: the tap
// on the TUI's prompt box that opens the composer. The button below stands
// in for that gesture — it fires the same callback, synchronously.
vi.mock("./terminal-pane", () => ({
  TerminalPane: React.forwardRef<
    unknown,
    { runId: string; keyboardEnabled?: boolean; onRequestKeyboard?: () => void }
  >(function TerminalPane(props, _ref) {
    return (
      <div
        data-testid="terminal"
        data-keyboard={props.keyboardEnabled ? "on" : "off"}
      >
        pty {props.runId}
        {props.onRequestKeyboard && (
          <button type="button" onClick={props.onRequestKeyboard}>
            tap prompt
          </button>
        )}
      </div>
    );
  }),
}));

const composerFocus = vi.fn();

vi.mock("./composer-v2", () => ({
  ComposerV2: React.forwardRef<
    { focus: () => void },
    { visible?: boolean; commands?: { name: string }[] }
  >(function ComposerV2(props, ref) {
    React.useImperativeHandle(ref, () => ({ focus: composerFocus }));
    return (
      <div
        data-testid="composer"
        data-visible={props.visible === false ? "no" : "yes"}
        data-commands={(props.commands ?? []).length}
      />
    );
  }),
}));

vi.mock("./permissions-overlay", () => ({
  PermissionsOverlay: () => <div data-testid="permissions" />,
}));
vi.mock("./control-sheet", () => ({
  ControlSheet: () => <div data-testid="control" />,
}));
vi.mock("./session-drawer", () => ({
  SessionDrawer: () => <div data-testid="drawer" />,
}));

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  runStatus: vi.fn(),
  getEngineCaps: vi.fn().mockResolvedValue({ caps: {} }),
  getEngineState: vi.fn().mockResolvedValue({ state: {} }),
  listEngineCommands: vi.fn().mockResolvedValue({ commands: [] }),
}));

vi.mock("next/link", () => ({
  default: ({
    children,
    href,
    ...rest
  }: { children: React.ReactNode; href: string } & Record<string, unknown>) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

const RUN = {
  id: "run-1",
  sessionId: "sess-abc",
  state: "running",
  blocked: false,
  engine: "claude",
  model: "opus",
  cwd: "/home/you/code",
  prompt: "Fix the export",
  eventCount: 42,
  resumeFrom: "",
};

function renderRun() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <RunView runId="run-1" />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  composerFocus.mockClear();
  vi.mocked(listEngineCommands).mockClear();
  vi.mocked(getEngineCaps).mockResolvedValue({ caps: {} } as never);
  vi.mocked(listEngineCommands).mockResolvedValue({ commands: [] } as never);
  vi.mocked(runStatus).mockResolvedValue(RUN as never);
});

// touch() makes the pointer coarse for one test: the composer starts closed
// only there, so without it every assertion about opening it is vacuous.
function touch(): () => void {
  const original = window.matchMedia;
  window.matchMedia = (q: string) =>
    ({
      matches: q.includes("coarse"),
      media: q,
      addEventListener() {},
      removeEventListener() {},
    }) as unknown as MediaQueryList;
  return () => {
    window.matchMedia = original;
  };
}

// --- opening the composer -----------------------------------------------
//
// The keyboard used to open only from a header icon: the hardest spot to
// reach one-handed and the furthest from where the composer appears. A tap
// on the TUI's own prompt box opens it now.
describe("opening the keyboard on a phone", () => {
  it("opens the composer from a tap on the prompt", async () => {
    const restore = touch();
    try {
      renderRun();
      expect(await screen.findByTestId("terminal")).toHaveAttribute(
        "data-keyboard",
        "off",
      );
      expect(screen.getByTestId("composer")).toHaveAttribute(
        "data-visible",
        "no",
      );
      fireEvent.click(screen.getByText("tap prompt"));
      expect(screen.getByTestId("composer")).toHaveAttribute(
        "data-visible",
        "yes",
      );
      expect(screen.getByTestId("terminal")).toHaveAttribute(
        "data-keyboard",
        "on",
      );
    } finally {
      restore();
    }
  });

  // iOS pops the on-screen keyboard only for a focus() made synchronously
  // inside the gesture. Mounting the composer on the state change and
  // focusing in an effect opens nothing, which is why it stays mounted.
  it("focuses the composer inside the tap itself", async () => {
    const restore = touch();
    try {
      renderRun();
      await screen.findByTestId("terminal");
      fireEvent.click(screen.getByText("tap prompt"));
      expect(composerFocus).toHaveBeenCalledTimes(1);
    } finally {
      restore();
    }
  });

  // A fine pointer types into the terminal directly, so there is no tap
  // gesture to hand it.
  it("leaves a desktop terminal without the tap handler", async () => {
    renderRun();
    await screen.findByTestId("terminal");
    expect(screen.queryByText("tap prompt")).not.toBeInTheDocument();
  });
});

// The composer's slash autocomplete was gated on the engine being opencode,
// so it stayed dark for claude no matter what the backend answered. It is
// the capability that decides now.
describe("the slash-command catalog", () => {
  it("is fetched for any engine whose caps list commands", async () => {
    vi.mocked(getEngineCaps).mockResolvedValue({
      caps: { listCommands: true },
    } as never);
    vi.mocked(listEngineCommands).mockResolvedValue({
      commands: [
        {
          name: "model",
          description: "",
          argsHint: "alias",
          source: "builtin",
        },
      ],
    } as never);
    renderRun();
    await vi.waitFor(() =>
      expect(screen.getByTestId("composer")).toHaveAttribute(
        "data-commands",
        "1",
      ),
    );
    expect(listEngineCommands).toHaveBeenCalledWith("run-1");
  });

  it("is not fetched when the engine has no catalog", async () => {
    renderRun();
    await screen.findByTestId("terminal");
    await new Promise((r) => setTimeout(r, 20));
    expect(listEngineCommands).not.toHaveBeenCalled();
  });
});

describe("the run view", () => {
  it("is the terminal, with no view to choose", async () => {
    renderRun();
    expect(await screen.findByTestId("terminal")).toBeInTheDocument();
    expect(screen.queryByRole("tab")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("tab", { name: /chat/i }),
    ).not.toBeInTheDocument();
  });

  // The PTY dial must not wait on the status query: on a phone connection
  // that is a whole round trip of blank screen.
  it("mounts the terminal before the run status has answered", async () => {
    vi.mocked(runStatus).mockReturnValue(new Promise(() => {}));
    renderRun();
    expect(await screen.findByTestId("terminal")).toBeInTheDocument();
  });

  // The header used to carry an "Open keyboard" icon for touch devices, where
  // the composer starts closed. Tapping the TUI's prompt rows opens the
  // composer now, so the header holds only control + details and nothing
  // else competes for that space. The pointer query says coarse so the
  // composer really is closed and the assertion is not vacuous.
  it("has no keyboard button in the header on touch; the prompt tap opens the composer", async () => {
    const original = window.matchMedia;
    window.matchMedia = (q: string) =>
      ({
        matches: q.includes("coarse"),
        media: q,
        addEventListener() {},
        removeEventListener() {},
      }) as unknown as MediaQueryList;
    try {
      renderRun();
      expect(await screen.findByText("Fix the export")).toBeInTheDocument();
      expect(
        screen.queryByRole("button", { name: /open keyboard/i }),
      ).toBeNull();
      expect(screen.queryByText("keyboard")).toBeNull();
      expect(
        screen.getByRole("button", { name: /control sheet/i }),
      ).toBeInTheDocument();
      expect(
        screen.getByRole("button", { name: /session details/i }),
      ).toBeInTheDocument();
    } finally {
      window.matchMedia = original;
    }
  });

  // Nothing the terminal-first view did is allowed to go missing.
  it("keeps the composer, the overlay and the control sheet", async () => {
    renderRun();
    expect(await screen.findByText("Fix the export")).toBeInTheDocument();
    expect(screen.getByTestId("composer")).toBeInTheDocument();
    expect(screen.getByTestId("permissions")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /control sheet/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /session details/i }),
    ).toBeInTheDocument();
  });
});

// A loop member's terminal says which loop and which role. Its prompt column
// is empty and its cwd is the loop cwd, identical for every member, so the
// header used to read "Terminal session" over the same path four times.
describe("a run that is no longer live", () => {
  it("offers a restart above the terminal", async () => {
    vi.mocked(runStatus).mockResolvedValue({ ...RUN, state: "stopped" } as never);
    renderRun();
    expect(await screen.findByRole("button", { name: /restart session/i })).toBeInTheDocument();
    expect(screen.getByTestId("terminal")).toBeInTheDocument();
  });

  it("shows no restart while the run is live", async () => {
    renderRun();
    await screen.findByTestId("terminal");
    await Promise.resolve();
    expect(screen.queryByRole("button", { name: /restart session/i })).toBeNull();
  });
});

describe("a loop member's terminal", () => {
  const member = {
    id: "loop_1",
    title: "Flaky guest export",
    task: "Find why it goes flaky above 500 rows",
    role: "ORCHESTRATOR",
    play: "recon",
    stepId: "brief",
    health: "listening",
  };

  it("is named after its loop and its role", async () => {
    vi.mocked(runStatus).mockResolvedValue({
      ...RUN,
      prompt: "",
      cwd: "/home/you/code/thing",
      loop: member,
    } as never);
    renderRun();
    expect(
      await screen.findByText("Flaky guest export · ORCHESTRATOR"),
    ).toBeInTheDocument();
    // The cwd is dropped: it is the same for every member of the loop and
    // told him nothing about which one he was looking at.
    expect(
      screen.queryByText(/home\/you\/code\/thing/),
    ).not.toBeInTheDocument();
    expect(screen.getByText(/recon · brief/)).toBeInTheDocument();
  });

  it("goes back to its loop rather than to the sessions list", async () => {
    vi.mocked(runStatus).mockResolvedValue({
      ...RUN,
      prompt: "",
      loop: member,
    } as never);
    renderRun();
    expect(await screen.findByLabelText("Back to the loop")).toHaveAttribute(
      "href",
      "/loop/board/?id=loop_1",
    );
  });

  it("leaves an ordinary run named by its own prompt", async () => {
    vi.mocked(runStatus).mockResolvedValue({
      ...RUN,
      prompt: "fix the export",
    } as never);
    renderRun();
    expect(await screen.findByText("fix the export")).toBeInTheDocument();
    expect(screen.getByLabelText("Back to sessions")).toBeInTheDocument();
  });
});

// --- the header name -----------------------------------------------------
//
// A resumed run used to be titled "resumed 290d8…" — the run key, which is
// the one thing that is never the name anyone gave the session.
describe("the run header's name", () => {
  it("prefers the name the session's own transcript knows it by", () => {
    expect(
      runHeaderTitle({
        id: "run_0123456789",
        title: "Agent Flow ORCHESTRATOR",
        prompt: "do a thing",
        project: "agent_flow",
      }),
    ).toBe("Agent Flow ORCHESTRATOR");
  });

  it("falls back through prompt, then project, then the short id", () => {
    expect(
      runHeaderTitle({
        id: "run_0123456789",
        prompt: "do a thing",
        project: "p",
      }),
    ).toBe("do a thing");
    expect(
      runHeaderTitle({ id: "run_0123456789", project: "agent_flow" }),
    ).toBe("agent_flow");
    expect(runHeaderTitle({ id: "run_0123456789" })).toBe("run run_0123");
  });

  it("treats a blank title as absent rather than as a name", () => {
    expect(runHeaderTitle({ id: "r", title: "   ", prompt: "real" })).toBe(
      "real",
    );
  });

  // The old behaviour, pinned so it cannot come back: a resumed run with a
  // name shows the NAME.
  it("never titles a run by its resume key", async () => {
    vi.mocked(runStatus).mockResolvedValue({
      ...RUN,
      prompt: "",
      resumeFrom: "290d8f2e-1111-2222-3333-444455556666",
      title: "AgentFlow REVIEW",
    } as never);
    renderRun();
    expect(await screen.findByText("AgentFlow REVIEW")).toBeInTheDocument();
    expect(screen.queryByText(/resumed 290d8/)).not.toBeInTheDocument();
  });

  it("names a loop member by its session name and role", async () => {
    vi.mocked(runStatus).mockResolvedValue({
      ...RUN,
      prompt: "",
      title: "Flaky guest export",
      loop: { id: "loop_1", title: "Loop title", task: "t", role: "REVIEW" },
    } as never);
    renderRun();
    expect(
      await screen.findByText("Flaky guest export · REVIEW"),
    ).toBeInTheDocument();
  });
});

// --- the state word ------------------------------------------------------
//
// Inside a session you can see it is alive; a pill saying "Running" spends
// header width on something the screen already shows. A dead one still has
// to be distinguishable, so it gets one quiet word.
describe("the header's state", () => {
  it("says nothing at all while the session is live", async () => {
    renderRun();
    expect(await screen.findByText("Fix the export")).toBeInTheDocument();
    expect(screen.queryByText("Running")).not.toBeInTheDocument();
    expect(screen.queryByText("running")).not.toBeInTheDocument();
  });

  it("says one quiet word when the session is not live", async () => {
    vi.mocked(runStatus).mockResolvedValue({
      ...RUN,
      state: "crashed",
    } as never);
    renderRun();
    const word = await screen.findByText("Crashed");
    // muted text, not a coloured pill
    expect(word.className).toContain("text-muted-foreground");
    expect(word.className).not.toContain("bg-destructive");
    expect(word.className).not.toContain("rounded-full");
  });

  it("still surfaces blocked, which is the one live state worth saying", async () => {
    vi.mocked(runStatus).mockResolvedValue({ ...RUN, blocked: true } as never);
    renderRun();
    expect(await screen.findByText("blocked")).toBeInTheDocument();
  });

  it("shows no engine badge anywhere in the header", async () => {
    renderRun();
    await screen.findByText("Fix the export");
    expect(screen.queryByText(/^claude$/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/^opencode$/i)).not.toBeInTheDocument();
  });
});
