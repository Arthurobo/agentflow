// wizard.test.tsx — the wizard renders the backend registries rather than a
// hardcoded list, SAYS SO when a registry fails instead of rendering an empty
// grid, says plainly that an unprobed engine's models were never checked,
// picks a working directory instead of asking for one to be typed, refuses to
// submit until it can, and shows each prompt with its token exactly once.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { LoopWizard, readLastCrew } from "./wizard";
import { createLoop, listPlays, listToolModels, listTools, LoopError } from "@/lib/loop";
import { listCwds } from "@/lib/agentd";

vi.mock("@/lib/loop", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/loop")>()),
  listPlays: vi.fn(),
  listTools: vi.fn(),
  listToolModels: vi.fn(),
  createLoop: vi.fn(),
}));

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  listCwds: vi.fn(),
}));

const PLAYS = [
  {
    name: "build",
    title: "Build",
    purpose: "Make a change and have the diff reviewed.",
    entry: "brief",
    maxRounds: 0,
    hasCycle: false,
    roles: ["ORCHESTRATOR", "IMPLEMENTATION", "REVIEW"],
    steps: [],
  },
  {
    name: "audit",
    title: "Audit",
    purpose: "Ask one question and have the answer attacked.",
    entry: "brief",
    maxRounds: 3,
    hasCycle: false,
    roles: ["ORCHESTRATOR", "INVESTIGATION", "REVIEW"],
    steps: [],
  },
  {
    name: "deep",
    title: "Deep",
    purpose: "Investigate, review, implement, review again.",
    entry: "brief",
    maxRounds: 3,
    hasCycle: true,
    roles: ["ORCHESTRATOR", "INVESTIGATION", "REVIEW", "IMPLEMENTATION"],
    steps: [],
  },
];

const TOOLS = [
  {
    id: "claude",
    title: "Claude Code",
    models: ["opus", "sonnet", "haiku", "fable"],
    probed: true,
    defaultModelAllowed: true,
    note: "the CLI's own aliases",
  },
  {
    id: "opencode",
    title: "OpenCode",
    models: [],
    probed: false,
    defaultModelAllowed: true,
    note: "opencode's model list is a property of this machine, so it is asked for rather than listed here",
  },
];

const CWDS = [
  { cwd: "/home/you/code/agent_flow", lastUsedAt: 2 },
  { cwd: "/home/you/code/webapp", lastUsedAt: 1 },
];

function renderWizard() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LoopWizard />
    </QueryClientProvider>,
  );
}

// Two fields now: the name, which every surface reads, and the instruction,
// which the orchestrator is sent. Both are filled on every create path.
const nameField = () => screen.getByLabelText(/name this loop/i);
const taskField = () => screen.getByLabelText(/what should this loop do/i);

const INSTRUCTION = "Find why the guest export goes flaky above 500 rows";

// fillForm gives the form the minimum that actually clears the server floor.
async function fillForm(user: ReturnType<typeof userEvent.setup>) {
  await user.type(nameField(), "Flaky export");
  await user.type(taskField(), INSTRUCTION);
}

// pickFrom opens a picker trigger and chooses one of its options.
async function pickFrom(user: ReturnType<typeof userEvent.setup>, trigger: string, option: string) {
  await user.click(screen.getByLabelText(trigger));
  const sheet = await screen.findByRole("dialog", { name: trigger });
  await user.click(within(sheet).getByText(option));
}

beforeEach(() => {
  window.localStorage.clear();
  vi.mocked(listPlays).mockResolvedValue(PLAYS);
  vi.mocked(listTools).mockResolvedValue(TOOLS);
  vi.mocked(listCwds).mockResolvedValue(CWDS);
  vi.mocked(listToolModels).mockResolvedValue({ models: [], probed: false, live: false, note: "" });
  vi.mocked(createLoop).mockReset();
});

describe("the wizard", () => {
  it("renders the plays the backend reports, not a list of its own", async () => {
    renderWizard();
    expect(await screen.findByText("Audit")).toBeInTheDocument();
    expect(screen.getByText("Deep")).toBeInTheDocument();
    // The cycle cap comes from the registry too.
    expect(screen.getByText(/cycles up to 3/)).toBeInTheDocument();
  });

  it("says the plays failed to load instead of rendering an empty picker", async () => {
    vi.mocked(listPlays).mockRejectedValue(new LoopError(401, "unauthorized", "invalid or revoked device token"));
    renderWizard();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/could not load the plays/i);
    expect(alert).toHaveTextContent(/invalid or revoked device token/);
    expect(alert).toHaveTextContent(/unauthorized/);
    // and it must not look like "there are no plays"
    expect(screen.queryByText(/the engine reports no plays/i)).not.toBeInTheDocument();
  });

  it("says the engines failed to load rather than rendering a role with no controls", async () => {
    vi.mocked(listTools).mockRejectedValue(new LoopError(0, "unreachable", "agentd unreachable at http://x"));
    const user = userEvent.setup();
    renderWizard();
    await user.click(await screen.findByText("Audit"));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/could not load the engines/i);
    expect(alert).toHaveTextContent(/agentd unreachable/);
    expect(screen.queryByLabelText("REVIEW engine")).not.toBeInTheDocument();
  });

  it("staffs the AGENTS the chosen play needs, and not the human", async () => {
    const user = userEvent.setup();
    renderWizard();
    await user.click(await screen.findByText("Audit"));
    await waitFor(() => expect(screen.getByLabelText("INVESTIGATION engine")).toBeInTheDocument());
    expect(screen.getByLabelText("REVIEW engine")).toBeInTheDocument();
    expect(screen.queryByLabelText("IMPLEMENTATION engine")).not.toBeInTheDocument();
    // The engineer runs on no engine and there is nothing to configure for
    // him, so he is a sentence rather than a row with a model picker.
    expect(screen.queryByLabelText("ENGINEER engine")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("ENGINEER model")).not.toBeInTheDocument();
    expect(screen.getByText(/You are in this loop too, as ENGINEER/)).toBeInTheDocument();
  });

  it("picks a working directory from the folders this machine knows", async () => {
    const user = userEvent.setup();
    renderWizard();
    const cwd = screen.getByPlaceholderText("/home/you/code/project");
    await user.click(cwd);
    expect(await screen.findByText("/home/you/code/agent_flow")).toBeInTheDocument();
    await user.click(screen.getByText("/home/you/code/webapp"));
    expect(cwd).toHaveValue("/home/you/code/webapp");
  });

  it("preselects the folder and the play used last time", async () => {
    window.localStorage.setItem("agentflow.run.lastCwd", "/home/you/code/webapp");
    window.localStorage.setItem("agentflow.loop.lastPlay", "deep");
    renderWizard();
    expect(screen.getByPlaceholderText("/home/you/code/project")).toHaveValue("/home/you/code/webapp");
    // the remembered play is already staffed, so the crew is on screen
    expect(await screen.findByLabelText("IMPLEMENTATION engine")).toBeInTheDocument();
  });

  // The typed box is the EXCEPTION now: it is what is left when the machine
  // itself named nothing, which is the honest empty of a machine with no
  // authenticated provider.
  it("falls back to typing only when the engine names nothing, and warns once", async () => {
    const user = userEvent.setup();
    renderWizard();
    await user.click(await screen.findByText("Audit"));
    await waitFor(() => expect(screen.getByLabelText("REVIEW engine")).toBeInTheDocument());

    // Claude is probed: the models are a closed list from the registry.
    expect(screen.getByLabelText("REVIEW model").tagName).toBe("BUTTON");

    await pickFrom(user, "REVIEW engine", "OpenCode");
    await waitFor(() => expect(screen.getByLabelText("REVIEW model").tagName).toBe("INPUT"));

    // ONE sentence, from the registry. It used to print the note and then
    // append a second sentence saying the same thing.
    const warnings = screen.getAllByText(/model list is a property/i);
    expect(warnings).toHaveLength(1);
    expect(document.body.textContent).not.toMatch(/fails when the agent launches/);
  });

  it("will not submit without a name", async () => {
    const user = userEvent.setup();
    renderWizard();
    await screen.findByText("Build");
    const create = screen.getByRole("button", { name: /create loop/i });
    // A play is preselected, so the two text fields are what is left.
    expect(create).toBeDisabled();

    // A name alone is exactly the failure this round exists to stop: it is
    // what every loop was created with, and the orchestrator got nothing.
    await user.type(nameField(), "New PR Test");
    expect(create).toBeDisabled();

    // And a task that only repeats the name is still a name.
    await user.type(taskField(), "New PR Test");
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/repeats the name/i),
    );
    expect(create).toBeDisabled();

    await user.clear(taskField());
    await user.type(taskField(), INSTRUCTION);
    await waitFor(() => expect(create).toBeEnabled());

    await user.clear(taskField());
    expect(create).toBeDisabled();
    expect(createLoop).not.toHaveBeenCalled();
  });

  // The preselect reads a remembered name; a name the backend no longer ships
  // resolves to no play at all, and that must not be submittable.
  it("will not submit when the remembered play no longer exists", async () => {
    const user = userEvent.setup();
    window.localStorage.setItem("agentflow.loop.lastPlay", "a-play-that-was-removed");
    renderWizard();
    await screen.findByText("Build");
    await fillForm(user);
    expect(screen.getByRole("button", { name: /create loop/i })).toBeDisabled();
  });

  it("creates with the play and the per-role engine and model", async () => {
    const user = userEvent.setup();
    vi.mocked(createLoop).mockResolvedValue({
      loop: { id: "loop_1", task: "Fix it" } as never,
      handoffs: [],
      spawning: false,
    });
    renderWizard();
    await fillForm(user);
    await user.click(await screen.findByText("Audit"));
    await waitFor(() => expect(screen.getByLabelText("REVIEW engine")).toBeInTheDocument());
    await pickFrom(user, "REVIEW model", "haiku");
    await user.click(screen.getByRole("button", { name: /create loop/i }));

    await waitFor(() => expect(createLoop).toHaveBeenCalled());
    const arg = vi.mocked(createLoop).mock.calls[0][0];
    // Both fields go on the wire, and they are different things.
    expect(arg.title).toBe("Flaky export");
    expect(arg.task).toBe(INSTRUCTION);
    expect(arg.play).toBe("audit");
    // the wire field is still `tool`; only the word in front of the user changed
    expect(arg.crew.find((c) => c.role === "REVIEW")?.tool).toBe("claude");
    expect(arg.crew.find((c) => c.role === "REVIEW")?.model).toBe("haiku");
    // The crew is the AGENTS. The engineer is added by the surface itself, so
    // the wizard neither sends him nor asks which engine he runs on.
    expect(arg.crew.map((c) => c.role)).not.toContain("ENGINEER");
  });

  it("remembers the crew it just created so the next loop starts from it", async () => {
    const user = userEvent.setup();
    vi.mocked(createLoop).mockResolvedValue({
      loop: { id: "loop_1", task: "Fix it" } as never,
      handoffs: [],
      spawning: false,
    });
    renderWizard();
    await fillForm(user);
    await user.click(await screen.findByText("Audit"));
    await waitFor(() => expect(screen.getByLabelText("REVIEW engine")).toBeInTheDocument());
    await pickFrom(user, "REVIEW model", "sonnet");
    await user.click(screen.getByRole("button", { name: /create loop/i }));

    await waitFor(() => expect(createLoop).toHaveBeenCalled());
    const crew = readLastCrew(window.localStorage.getItem("agentflow.loop.lastCrew") ?? "");
    expect(crew.REVIEW).toEqual({ role: "REVIEW", tool: "claude", model: "sonnet" });
    expect(crew.ENGINEER).toBeUndefined();
    expect(window.localStorage.getItem("agentflow.loop.lastPlay")).toBe("audit");
  });

  it("shows each prompt once and says the token cannot be read again", async () => {
    const user = userEvent.setup();
    vi.mocked(createLoop).mockResolvedValue({
      loop: { id: "loop_1", task: "Fix it" } as never,
      handoffs: [
        {
          role: "REVIEW",
          agentId: "r1",
          tool: "claude",
          model: "opus",
          token: "af_secret_token",
          prompt: "You are REVIEW in the Audit play.\nWAIT\n  while true; do ...",
        },
        { role: "ENGINEER", agentId: "e1", tool: "", model: "", token: "", prompt: "" },
      ],
      spawning: false,
    });
    renderWizard();
    await fillForm(user);
    await user.click(await screen.findByText("Audit"));
    await user.click(screen.getByRole("button", { name: /create loop/i }));

    expect(await screen.findByText(/only time these tokens are readable/i)).toBeInTheDocument();
    expect(screen.getByText(/You are REVIEW in the Audit play/)).toBeInTheDocument();
    // The engineer holds no token, so it gets no card to paste.
    expect(screen.queryByText("ENGINEER")).not.toBeInTheDocument();
  });
});

describe("readLastCrew", () => {
  it("ignores anything that is not a role to draft map", () => {
    expect(readLastCrew("")).toEqual({});
    expect(readLastCrew("not json")).toEqual({});
    expect(readLastCrew("[1,2]")).toEqual({});
    expect(readLastCrew('{"REVIEW": 4}')).toEqual({});
  });

  it("defaults a half-written entry rather than handing back undefined", () => {
    expect(readLastCrew('{"REVIEW":{"tool":"opencode"}}')).toEqual({
      REVIEW: { role: "REVIEW", tool: "opencode", model: "" },
    });
  });

  // With spawning there is nothing to paste: every member already has its
  // brief on argv, so the handoff screen is not the happy path any more.
  it("goes straight to the loop when agentd spawns the crew", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    vi.mocked(createLoop).mockResolvedValue({
      loop: { id: "loop_9", task: "Fix it" } as never,
      handoffs: [
        { role: "REVIEW", agentId: "r1", tool: "claude", model: "opus", token: "af_tok", prompt: "You are REVIEW." },
      ],
      spawning: true,
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <LoopWizard onCreated={onCreated} />
      </QueryClientProvider>,
    );
    await fillForm(user);
    await user.click(await screen.findByText("Audit"));
    await user.click(screen.getByRole("button", { name: /create loop/i }));

    await waitFor(() => expect(onCreated).toHaveBeenCalledWith("loop_9"));
    // and the token screen never appears, so nobody is invited to paste
    expect(screen.queryByText(/only time these tokens are readable/i)).not.toBeInTheDocument();
  });

  // The interstitial exists to let him copy prompts and tokens by hand. A
  // response that does not carry the spawning flag at all must not resurrect
  // it when there is nothing in it to copy.
  it("still goes straight to the loop when nothing is pasteable", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    vi.mocked(createLoop).mockResolvedValue({
      loop: { id: "loop_7", task: "Fix it" } as never,
      // no token and no prompt: the handoff screen would be empty
      handoffs: [{ role: "REVIEW", agentId: "r1", tool: "claude", model: "", token: "", prompt: "" }],
      spawning: false,
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <LoopWizard onCreated={onCreated} />
      </QueryClientProvider>,
    );
    await fillForm(user);
    await user.click(await screen.findByText("Audit"));
    await user.click(screen.getByRole("button", { name: /create loop/i }));

    await waitFor(() => expect(onCreated).toHaveBeenCalledWith("loop_7"));
    expect(screen.queryByText(/only time these tokens are readable/i)).not.toBeInTheDocument();
  });

  // And it still appears for a deployment that genuinely does not spawn.
  it("shows the prompts when there really is something to paste", async () => {
    const user = userEvent.setup();
    vi.mocked(createLoop).mockResolvedValue({
      loop: { id: "loop_8", task: "Fix it" } as never,
      handoffs: [
        { role: "REVIEW", agentId: "r1", tool: "claude", model: "opus", token: "af_tok", prompt: "You are REVIEW." },
      ],
      spawning: false,
    });
    renderWizard();
    await fillForm(user);
    await user.click(await screen.findByText("Audit"));
    await user.click(screen.getByRole("button", { name: /create loop/i }));
    expect(await screen.findByText(/only time these tokens are readable/i)).toBeInTheDocument();
  });

  // An unprobed engine's model set belongs to THIS install, so it is asked for
  // rather than assumed, and the typed escape hatch stays.
  // When the machine DOES answer, the row is a picker like every other row,
  // and the engine's own default is one of the choices. Before this the
  // engineer had to type a model id from memory, which is how "minimax"
  // reached a spawn.
  it("offers the live list and the engine's own default as a picker", async () => {
    const user = userEvent.setup();
    vi.mocked(listToolModels).mockResolvedValue({
      models: [
        { id: "opencode/omen-alpha", displayName: "Omen Alpha", provider: "opencode", status: "active" },
        { id: "opencode-go/grok", displayName: "Grok", provider: "opencode-go", status: "active" },
      ],
      probed: true,
      live: true,
      note: "",
    });
    renderWizard();
    await user.click(await screen.findByText("Audit"));
    await waitFor(() => expect(screen.getByLabelText("REVIEW engine")).toBeInTheDocument());
    await pickFrom(user, "REVIEW engine", "OpenCode");

    // A picker, not a text box, and no warning: the list is real.
    await waitFor(() => expect(screen.getByLabelText("REVIEW model").tagName).toBe("BUTTON"));
    expect(screen.queryByText(/model list is a property/i)).not.toBeInTheDocument();

    await user.click(screen.getByLabelText("REVIEW model"));
    const sheet = await screen.findByRole("dialog", { name: "REVIEW model" });
    // The engine's own default is a choice, which is what makes typing
    // optional rather than mandatory.
    expect(within(sheet).getByText(/OpenCode's own default/i)).toBeInTheDocument();
    await user.click(within(sheet).getByText("Omen Alpha"));

    await waitFor(() =>
      expect(screen.getByLabelText("REVIEW model")).toHaveTextContent("opencode/omen-alpha"),
    );
    expect(vi.mocked(listToolModels).mock.calls[0][0]).toBe("opencode");
  });

  // This field IS the instruction now. There is no composer left to type it
  // into afterwards, and typing into a member's terminal reaches nothing,
  // so a field that reads like a label would leave the orchestrator with no
  // idea what it was started for.
  it("asks for the instruction, says it is delivered, and does not mention pasting", async () => {
    renderWizard();
    expect(await screen.findByText("What should this loop do")).toBeInTheDocument();
    expect(screen.getByText(/delivered as its first message/i)).toBeInTheDocument();
    expect(taskField().tagName).toBe("TEXTAREA");
    // The crew hint is the other place that said "paste", and it only exists
    // once the plays query has resolved and staffed a play.
    await screen.findByLabelText("REVIEW engine");
    expect(screen.getByText(/each is launched with its own brief/i)).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/paste/i);
  });

  it("says what each play is FOR, not only which roles it uses", async () => {
    renderWizard();
    expect(await screen.findByText("Ask one question and have the answer attacked.")).toBeInTheDocument();
    expect(screen.getByText("Investigate, review, implement, review again.")).toBeInTheDocument();
  });

  // Seven flat cards with nothing chosen is a decision he has to make before
  // he can do anything.
  it("preselects a default play, and the one he used last beats it", async () => {
    const pressed = () =>
      Array.from(document.querySelectorAll('button[aria-pressed="true"]')).map((b) => b.textContent);

    const first = renderWizard();
    await screen.findByText("Build");
    // A first-time engineer lands on something chosen, and its crew is staffed
    // already, so the screen is not a decision before it is a form.
    await waitFor(() => expect(pressed().some((t) => t?.includes("Build"))).toBe(true));
    expect(await screen.findByLabelText("IMPLEMENTATION engine")).toBeInTheDocument();
    first.unmount();

    window.localStorage.setItem("agentflow.loop.lastPlay", "deep");
    renderWizard();
    await waitFor(() => expect(pressed().some((t) => t?.includes("Deep"))).toBe(true));
    expect(pressed().some((t) => t?.includes("Build"))).toBe(false);
  });

  // Both of these are the same complaint in a different place: once a play is
  // chosen, everything below it should read as belonging to that play.
  it("names the play on the crew section, so it is not a detached list", async () => {
    const user = userEvent.setup();
    renderWizard();
    await user.click(await screen.findByText("Audit"));
    expect(await screen.findByText("Who runs Audit")).toBeInTheDocument();
    expect(screen.queryByText("Who runs it")).not.toBeInTheDocument();
  });

  // The last moment before real processes start on his machine.
  it("says on the button which play it will start and how many agents", async () => {
    const user = userEvent.setup();
    renderWizard();
    await user.click(await screen.findByText("Audit"));
    // Audit staffs ORCHESTRATOR, INVESTIGATION and REVIEW; ENGINEER is an
    // address rather than a process and is not one of them.
    expect(
      await screen.findByRole("button", { name: /create loop: audit, starting 3 agents/i }),
    ).toBeInTheDocument();

    await user.click(screen.getByText("Deep"));
    expect(
      await screen.findByRole("button", { name: /create loop: deep, starting 4 agents/i }),
    ).toBeInTheDocument();
  });

  it("shows the chosen play's shape and only the chosen one's", async () => {
    const user = userEvent.setup();
    renderWizard();
    await user.click(await screen.findByText("Audit"));
    // one selected card, one shape
    await waitFor(() =>
      expect(document.querySelectorAll('[data-selected="yes"]')).toHaveLength(1),
    );
    expect(screen.getAllByText("Selected")).toHaveLength(1);
    const chosen = document.querySelector('[data-selected="yes"]')!;
    expect(chosen).toHaveTextContent("Audit");
    expect(chosen).toHaveTextContent(/Staffs/);
  });
});