// control-sheet.test.tsx — the sheet's Claude story. Every row here used to
// be greyed out because the engine's caps said it could do nothing; they are
// served by keystroke injection now, and the one thing the picker cannot do
// on its own is guess a command's argument.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ControlSheet } from "./control-sheet";
import {
  getEngineCaps,
  getEngineState,
  listEngineCommands,
  listEngineModels,
  listEngineSkills,
  runEngineCommand,
  setEngineModel,
} from "@/lib/agentd";

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  getEngineCaps: vi.fn(),
  getEngineState: vi.fn(),
  listEngineCommands: vi.fn(),
  listEngineModels: vi.fn(),
  listEngineSkills: vi.fn(),
  runEngineCommand: vi.fn(),
  setEngineModel: vi.fn(),
}));

const CLAUDE_CAPS = {
  listModels: true,
  setModel: true,
  listCommands: true,
  runCommand: true,
  listSkills: true,
  prompt: true,
  abort: true,
};

function renderSheet(props: { onClose?: () => void } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ControlSheet
        runId="run-1"
        open
        onClose={props.onClose ?? vi.fn()}
        initialModel="sonnet"
      />
    </QueryClientProvider>,
  );
}

// capsLoaded waits for the answer the whole sheet is gated on: every row is
// disabled and inert until it lands, so asserting before it is asserting on
// the loading state.
async function capsLoaded() {
  return screen.findByRole("button", { name: /^Commands:/ });
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(getEngineCaps).mockResolvedValue({
    engine: "claude",
    caps: CLAUDE_CAPS,
  } as never);
  vi.mocked(getEngineState).mockResolvedValue({
    engine: "claude",
    state: {},
  } as never);
  vi.mocked(listEngineModels).mockResolvedValue({
    engine: "claude",
    models: [{ id: "opus", displayName: "Opus", provider: "claude" }],
  } as never);
  vi.mocked(listEngineSkills).mockResolvedValue({
    engine: "claude",
    skills: [
      { name: "dataviz", description: "Draw the chart", path: "/p/SKILL.md" },
    ],
  } as never);
  vi.mocked(listEngineCommands).mockResolvedValue({
    engine: "claude",
    commands: [
      {
        name: "context",
        description: "Show context usage",
        argsHint: "",
        source: "builtin",
      },
      {
        name: "add-dir",
        description: "Allow another directory",
        argsHint: "path",
        source: "builtin",
      },
    ],
  } as never);
  vi.mocked(runEngineCommand).mockResolvedValue({} as never);
  vi.mocked(setEngineModel).mockResolvedValue({} as never);
});

describe("the control sheet for a claude session", () => {
  it("offers the rows injection can serve, and no longer tells the user to type it themselves", async () => {
    renderSheet();
    await capsLoaded();
    const model = screen.getByRole("button", { name: /^Model:/ });
    expect(model).toBeEnabled();
    expect(model.getAttribute("title")).toBeNull();
    expect(
      screen.getByRole("button", { name: /^Skills:/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /^Commands:/ }),
    ).toBeInTheDocument();
    // The agent row is a live switcher claude has no channel for; it stays
    // off rather than being offered and failing.
    expect(
      screen.queryByRole("button", { name: /^Agent:/ }),
    ).not.toBeInTheDocument();
  });

  it("applies a model pick through the backend", async () => {
    renderSheet();
    await capsLoaded();
    fireEvent.click(screen.getByRole("button", { name: /^Model:/ }));
    fireEvent.click(await screen.findByText("Opus"));
    await waitFor(() =>
      expect(setEngineModel).toHaveBeenCalledWith("run-1", "opus"),
    );
  });

  // A command with no argument runs on the pick — one tap, as before.
  it("runs an argument-less command straight away", async () => {
    renderSheet();
    fireEvent.click(await capsLoaded());
    fireEvent.click(await screen.findByText("/context"));
    await waitFor(() =>
      expect(runEngineCommand).toHaveBeenCalledWith("run-1", "context"),
    );
  });

  // One that takes an argument asks first: /add-dir with nothing attached
  // does nothing, and /model with nothing attached opens an interactive
  // menu in a terminal nobody is watching.
  it("asks for the argument before running a command that takes one", async () => {
    renderSheet();
    fireEvent.click(await capsLoaded());
    fireEvent.click(await screen.findByText("/add-dir"));
    expect(runEngineCommand).not.toHaveBeenCalled();

    const input = await screen.findByLabelText("add-dir argument");
    expect(input).toHaveAttribute("placeholder", "path");
    await userEvent.type(input, "/home/you/other");
    fireEvent.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() =>
      expect(runEngineCommand).toHaveBeenCalledWith(
        "run-1",
        "add-dir",
        "/home/you/other",
      ),
    );
  });

  it("runs nothing when the argument step is backed out of", async () => {
    renderSheet();
    fireEvent.click(await capsLoaded());
    fireEvent.click(await screen.findByText("/add-dir"));
    fireEvent.click(
      await screen.findByRole("button", { name: "Back to the control sheet" }),
    );
    await waitFor(() =>
      expect(
        screen.queryByLabelText("add-dir argument"),
      ).not.toBeInTheDocument(),
    );
    expect(runEngineCommand).not.toHaveBeenCalled();
  });

  // An engine that genuinely cannot switch still says so, and says it
  // without telling the user to go type it in the terminal.
  it("greys the model row for an engine with no live switch", async () => {
    vi.mocked(getEngineCaps).mockResolvedValue({
      engine: "opencode",
      caps: { ...CLAUDE_CAPS, setModel: false },
    } as never);
    renderSheet();
    await capsLoaded();
    const model = screen.getByRole("button", { name: /^Model:/ });
    expect(model).toBeDisabled();
    expect(model.getAttribute("title")).toBe(
      "This engine has no live model switch",
    );
  });
});

// --- getting back out --------------------------------------------------
//
// A picker is a full bottom sheet on top of the control sheet. On a phone
// there is no Escape key and no window chrome, so before this there was no
// way back at all: the picker covered the sheet and the browser Back button
// left the run page from under it.
describe("finding the way back from a picker", () => {
  it("returns to the sheet from the picker's back button", async () => {
    renderSheet();
    fireEvent.click(await capsLoaded());
    await screen.findByText("/context");
    fireEvent.click(
      screen.getByRole("button", { name: "Back to the control sheet" }),
    );
    await waitFor(() =>
      expect(screen.queryByText("/context")).not.toBeInTheDocument(),
    );
    // the sheet itself is still there
    expect(
      screen.getByRole("button", { name: /^Commands:/ }),
    ).toBeInTheDocument();
    expect(runEngineCommand).not.toHaveBeenCalled();
  });

  // Tapping outside a sheet is the gesture people reach for first.
  it("returns to the sheet when the backdrop is tapped", async () => {
    renderSheet();
    fireEvent.click(await capsLoaded());
    await screen.findByText("/context");
    fireEvent.click(screen.getByTestId("picker-backdrop"));
    await waitFor(() =>
      expect(screen.queryByText("/context")).not.toBeInTheDocument(),
    );
    expect(
      screen.getByRole("button", { name: /^Commands:/ }),
    ).toBeInTheDocument();
  });

  it("keeps the sheet open behind the picker rather than replacing it", async () => {
    renderSheet();
    fireEvent.click(await capsLoaded());
    await screen.findByText("/context");
    expect(
      screen.getByRole("dialog", { name: "Control sheet" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("dialog", { name: "Run command" }),
    ).toBeInTheDocument();
  });
});

// Android's back gesture and the browser back button are the same event.
// Back must peel one layer at a time: picker, then sheet, and only then
// leave the run page.
describe("the hardware back button", () => {
  const back = async () => {
    window.dispatchEvent(new PopStateEvent("popstate", { state: null }));
  };

  it("closes the picker first, leaving the sheet open", async () => {
    const onClose = vi.fn();
    renderSheet({ onClose });
    fireEvent.click(await capsLoaded());
    await screen.findByText("/context");
    await back();
    await waitFor(() =>
      expect(screen.queryByText("/context")).not.toBeInTheDocument(),
    );
    expect(onClose).not.toHaveBeenCalled();
    expect(
      screen.getByRole("button", { name: /^Commands:/ }),
    ).toBeInTheDocument();
  });

  it("closes the sheet on the next back, before leaving the page", async () => {
    const onClose = vi.fn();
    renderSheet({ onClose });
    fireEvent.click(await capsLoaded());
    await screen.findByText("/context");
    await back();
    await waitFor(() =>
      expect(screen.queryByText("/context")).not.toBeInTheDocument(),
    );
    await back();
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it("closes the sheet directly when no picker is open", async () => {
    const onClose = vi.fn();
    renderSheet({ onClose });
    await capsLoaded();
    await back();
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  // The argument step is a layer too, and Back from it returns to the sheet
  // rather than firing the command.
  it("closes the argument step without running the command", async () => {
    const onClose = vi.fn();
    renderSheet({ onClose });
    fireEvent.click(await capsLoaded());
    fireEvent.click(await screen.findByText("/add-dir"));
    await screen.findByLabelText("add-dir argument");
    await back();
    await waitFor(() =>
      expect(
        screen.queryByLabelText("add-dir argument"),
      ).not.toBeInTheDocument(),
    );
    expect(runEngineCommand).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });
});

// A sheet that pushed history entries must give them back, or Back keeps
// landing on a sheet that is no longer on screen.
//
// history.length is the wrong measure — it never shrinks on back(), it only
// truncates on the next push. The property that matters is the POSITION:
// once the sheet is gone, the current entry must not be one of its own.
describe("the history stack", () => {
  const af = () => (window.history.state as { af?: string } | null)?.af;

  it("is left where it was found once the sheet closes", async () => {
    // Anchor the position so the assertion is about THIS sheet's entries
    // and not about whatever earlier tests left in the shared jsdom history.
    window.history.pushState({ af: "anchor" }, "");
    const { unmount } = renderSheet();
    await capsLoaded();
    fireEvent.click(screen.getByRole("button", { name: /^Commands:/ }));
    await screen.findByText("/context");
    await waitFor(() => expect(af()).toBe("control-sheet"));
    fireEvent.click(
      screen.getByRole("button", { name: "Back to the control sheet" }),
    );
    await waitFor(() =>
      expect(screen.queryByText("/context")).not.toBeInTheDocument(),
    );
    unmount();
    await waitFor(() => expect(af()).toBe("anchor"), { timeout: 3000 });
  });
});
