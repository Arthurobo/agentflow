// prompt-editor.test.tsx — one editor, three scopes, prose only. A dropped
// play is a disabled card with its reason rather than an absence, and an
// orphaned edit is retrievable rather than deleted.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PromptEditor, type EditorScope } from "./prompt-editor";
import { deleteOverride, getPrompts, putOverride } from "@/lib/prompts";

vi.mock("@/lib/prompts", async (o) => ({
  ...(await o<typeof import("@/lib/prompts")>()),
  getPrompts: vi.fn(),
  putOverride: vi.fn(),
  deleteOverride: vi.fn(),
}));

const PLAY = {
  Name: "deep",
  Title: "Deep",
  Purpose: "Investigate, review, implement, review again.",
  Entry: "brief",
  Steps: [
    { ID: "brief", Actor: "ORCHESTRATOR", Note: "brief the investigator", Brief: "Send one brief.", FanIn: false },
    { ID: "review_work", Actor: "REVIEW", Note: "adversarial review", Brief: "Read the diff.", FanIn: false },
  ],
};

const RULES = {
  shared: [
    { name: "engineer-drives-git", text: "The engineer drives ALL git.", binding: true },
    { name: "no-scope-expansion", text: "Do what the brief asks.", binding: false },
  ],
  worker: [{ name: "notes-for-a-stranger", text: "Write for a stranger.", binding: false }],
  orchestrator: [{ name: "forward-verbatim", text: "Forward by id.", binding: false }],
  solo: [{ name: "reporting-contract-solo", text: "Report per change.", binding: false }],
};

const BASE = {
  plays: [PLAY],
  rules: RULES,
  solo: { Intro: "You are working directly for the engineer.", Rules: ["engineer-drives-git"] },
  overrides: [],
  dropped: {},
  defaultPlays: [PLAY],
  defaultRules: RULES,
};

// saveFor finds the Save button belonging to the SAME leaf as a given box.
// Indexing the save buttons positionally couples every test to how many leaves
// the editor happens to render, which is exactly what broke when two more were
// added.
function saveFor(box: HTMLElement): HTMLElement {
  const leaf = box.closest(".af-subtle-panel");
  if (!leaf) throw new Error("the box is not inside a leaf");
  const btn = within(leaf as HTMLElement).getByRole("button", { name: /^save$/i });
  return btn;
}

function renderEditor(scope: EditorScope) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <PromptEditor scope={scope} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(getPrompts).mockResolvedValue(BASE as never);
  vi.mocked(putOverride).mockReset();
  vi.mocked(putOverride).mockResolvedValue({ id: "ovr_1" } as never);
  vi.mocked(deleteOverride).mockReset();
  vi.mocked(deleteOverride).mockResolvedValue(undefined);
});

describe("the play scope", () => {
  // All five prose leaves the round names: title and purpose at the play
  // level, brief and note per step, and rule text elsewhere.
  it("offers the title, the purpose, and every step's brief and note", async () => {
    renderEditor({ kind: "play", play: "deep" });
    expect(await screen.findByLabelText("Title text")).toHaveValue(PLAY.Title);
    expect(screen.getByLabelText("Purpose text")).toHaveValue(PLAY.Purpose);
    expect(screen.getByText("brief · ORCHESTRATOR")).toBeInTheDocument();
    expect(screen.getByText("review_work · REVIEW")).toBeInTheDocument();
    expect(screen.getAllByLabelText("Brief text")).toHaveLength(2);
    expect(screen.getAllByLabelText("Note text")).toHaveLength(2);
    expect(screen.getAllByLabelText("Note text")[1]).toHaveValue("adversarial review");
  });

  it("saves a note and a title against their own leaves", async () => {
    const user = userEvent.setup();
    renderEditor({ kind: "play", play: "deep" });
    const notes = await screen.findAllByLabelText("Note text");
    await user.type(notes[1], " x");
    await user.click(saveFor(notes[1]));
    await waitFor(() => expect(putOverride).toHaveBeenCalled());
    expect(vi.mocked(putOverride).mock.calls[0][0]).toEqual({
      kind: "play", play: "deep", step: "review_work", field: "note",
    });

    vi.mocked(putOverride).mockClear();
    const title = screen.getByLabelText("Title text");
    await user.type(title, "!");
    await user.click(saveFor(title));
    await waitFor(() => expect(putOverride).toHaveBeenCalled());
    expect(vi.mocked(putOverride).mock.calls[0][0]).toEqual({
      kind: "play", play: "deep", field: "title",
    });
  });

  // Editing a guarded seven-step graph at 390px is not a form-design problem
  // worth attempting, and structural edits are the ones that trip Validate.
  it("offers nothing structural", async () => {
    renderEditor({ kind: "play", play: "deep" });
    await screen.findByLabelText("Purpose text");
    const body = document.body.textContent ?? "";
    for (const forbidden of ["Entry step", "Max rounds", "Add step", "Remove step", "Guard", "Fan-out"]) {
      expect(body).not.toContain(forbidden);
    }
    expect(screen.queryByLabelText(/entry/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/maxRounds/i)).not.toBeInTheDocument();
  });

  it("saves a brief against its own leaf", async () => {
    const user = userEvent.setup();
    renderEditor({ kind: "play", play: "deep" });
    const boxes = await screen.findAllByLabelText("Brief text");
    await user.type(boxes[1], " Then say clean or changes.");
    await user.click(saveFor(boxes[1]));
    await waitFor(() => expect(putOverride).toHaveBeenCalled());
    expect(vi.mocked(putOverride).mock.calls[0][0]).toEqual({
      kind: "play",
      play: "deep",
      step: "review_work",
      field: "brief",
    });
  });

  // A play dropped at load time is not an absence: an engineer whose play
  // vanished will not guess why.
  it("explains a dropped play instead of hiding it", async () => {
    vi.mocked(getPrompts).mockResolvedValue({
      ...BASE,
      plays: [],
      dropped: { deep: "store: invalid play: deep step review_work has no brief" },
    } as never);
    renderEditor({ kind: "play", play: "deep" });
    expect(await screen.findByText(/this play is not being offered/i)).toBeInTheDocument();
    expect(screen.getByText(/has no brief/)).toBeInTheDocument();
    expect(screen.getByText(/reverting the one that broke it puts the play back/i)).toBeInTheDocument();
  });
});

describe("the rules scope", () => {
  it("shows only the groups this role actually follows", async () => {
    renderEditor({ kind: "rules", role: "REVIEW" });
    expect(await screen.findByText("Every member")).toBeInTheDocument();
    expect(screen.getByText("Workers only")).toBeInTheDocument();
    expect(screen.queryByText("The orchestrator only")).not.toBeInTheDocument();
  });

  it("gives the orchestrator its own group and not the workers'", async () => {
    renderEditor({ kind: "rules", role: "ORCHESTRATOR" });
    expect(await screen.findByText("The orchestrator only")).toBeInTheDocument();
    expect(screen.queryByText("Workers only")).not.toBeInTheDocument();
  });

  it("marks the binding rule and leaves the advisory one plain", async () => {
    renderEditor({ kind: "rules", role: "REVIEW" });
    await screen.findByText("Every member");
    expect(screen.getAllByText(/add only/i)).toHaveLength(1);
    expect(screen.getByText(/ships fixed and cannot be replaced/i)).toBeInTheDocument();
  });
});

describe("the solo scope", () => {
  it("edits the opening and not which rules it names", async () => {
    renderEditor({ kind: "solo" });
    expect(await screen.findByLabelText("Solo session opening text")).toHaveValue(BASE.solo.Intro);
    expect(screen.getByText(/the rules it carries are set in the file/i)).toBeInTheDocument();
  });

  it("saves against the solo intro leaf", async () => {
    const user = userEvent.setup();
    renderEditor({ kind: "solo" });
    const box = await screen.findByLabelText("Solo session opening text");
    await user.type(box, " Ask before migrations.");
    await user.click(saveFor(box));
    await waitFor(() => expect(putOverride).toHaveBeenCalled());
    expect(vi.mocked(putOverride).mock.calls[0][0]).toEqual({ kind: "solo", field: "intro" });
  });
});

// We do not delete an engineer's writing because we renamed a step.
describe("orphaned edits", () => {
  it("lists them with their text so they can be retrieved", async () => {
    vi.mocked(getPrompts).mockResolvedValue({
      ...BASE,
      overrides: [
        {
          id: "ovr_9",
          kind: "play",
          play: "deep",
          step: "a_step_we_renamed",
          field: "brief",
          value: "Something he spent time writing.",
          status: "orphaned",
        },
      ],
    } as never);
    renderEditor({ kind: "solo" });
    expect(await screen.findByText(/edits with nowhere to go/i)).toBeInTheDocument();
    expect(screen.getByText("Something he spent time writing.")).toBeInTheDocument();
    expect(screen.getByText(/kept because they are yours/i)).toBeInTheDocument();
  });

  // ONLY the orphans. A working edit listed here comes with a Discard button
  // that would delete an override which is currently in effect.
  it("lists nothing that is still working", async () => {
    vi.mocked(getPrompts).mockResolvedValue({
      ...BASE,
      overrides: [
        {
          id: "ovr_live", kind: "solo", field: "intro",
          value: "An edit that is in effect right now.", status: "applied",
        },
        {
          id: "ovr_stale", kind: "rule", rule: "no-scope-expansion", field: "text",
          value: "An edit the default moved under.", status: "stale",
          base: "Do what the brief asks.",
        },
        {
          id: "ovr_dead", kind: "rule", rule: "a-rule-we-removed", field: "text",
          value: "An edit with nowhere to go.", status: "orphaned",
        },
      ],
    } as never);
    renderEditor({ kind: "solo" });
    const panel = (await screen.findByText(/edits with nowhere to go/i)).closest("div")!;
    expect(within(panel).getByText("An edit with nowhere to go.")).toBeInTheDocument();
    expect(within(panel).queryByText("An edit that is in effect right now.")).not.toBeInTheDocument();
    expect(within(panel).queryByText("An edit the default moved under.")).not.toBeInTheDocument();
    // one orphan, one Discard: nothing working is offered for deletion
    expect(within(panel).getAllByRole("button", { name: /discard/i })).toHaveLength(1);
  });

  it("discards one only when he asks", async () => {
    const user = userEvent.setup();
    vi.mocked(getPrompts).mockResolvedValue({
      ...BASE,
      overrides: [
        { id: "ovr_9", kind: "rule", rule: "a-rule-we-removed", field: "text", value: "mine", status: "orphaned" },
      ],
    } as never);
    renderEditor({ kind: "solo" });
    await screen.findByText(/edits with nowhere to go/i);
    expect(deleteOverride).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: /discard/i }));
    await waitFor(() => expect(deleteOverride).toHaveBeenCalledWith("ovr_9"));
  });
});
