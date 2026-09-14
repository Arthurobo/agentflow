// leaf-editor.test.tsx — one editable paragraph, and the three things it has
// to get right: a binding rule teaches its own constraint, the validator's
// message arrives verbatim under the field, and a stale edit is never
// resolved for the engineer.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { LeafEditor } from "./leaf-editor";
import { PromptError, type PromptOverride } from "@/lib/prompts";

const SHIPPED = "The engineer drives ALL git. Everything stays UNSTAGED.";

function renderLeaf(over: Partial<React.ComponentProps<typeof LeafEditor>> = {}) {
  const onSave = vi.fn().mockResolvedValue(undefined);
  const onRevert = vi.fn().mockResolvedValue(undefined);
  render(
    <LeafEditor
      label="engineer-drives-git"
      target={{ kind: "rule", rule: "engineer-drives-git", field: "text" }}
      shipped={SHIPPED}
      current={SHIPPED}
      onSave={onSave}
      onRevert={onRevert}
      {...over}
    />,
  );
  return { onSave, onRevert };
}

describe("a binding rule", () => {
  // The interaction teaches the constraint instead of a 409 explaining it
  // after he has typed a replacement.
  it("shows the shipped text as fixed with an add box beneath it", () => {
    renderLeaf({ binding: true });
    expect(screen.getByText(/ships fixed and cannot be replaced from here/i)).toBeInTheDocument();
    expect(screen.getByText(SHIPPED)).toBeInTheDocument();
    expect(screen.getByText("Add to this rule")).toBeInTheDocument();
    expect(screen.getByText(/add only/i)).toBeInTheDocument();
    // and the box starts EMPTY: it holds the addition, never the whole rule,
    // so he cannot accidentally edit a copy of the shipped sentence.
    expect(screen.getByLabelText("engineer-drives-git text")).toHaveValue("");
  });

  it("sends only the addition", async () => {
    const user = userEvent.setup();
    const { onSave } = renderLeaf({ binding: true });
    await user.type(screen.getByLabelText("engineer-drives-git text"), "Never touch main.");
    await user.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith("Never touch main."));
    expect(onSave.mock.calls[0][0]).not.toContain("UNSTAGED");
  });

  it("keeps an existing addition in the box, not the merged rule", () => {
    renderLeaf({
      binding: true,
      current: SHIPPED + "\nNever touch main.",
      override: { value: "Never touch main.", status: "applied" } as PromptOverride,
    });
    expect(screen.getByLabelText("engineer-drives-git text")).toHaveValue("Never touch main.");
  });
});

describe("an advisory rule", () => {
  it("edits as a plain replacement", async () => {
    const user = userEvent.setup();
    const { onSave } = renderLeaf({ label: "no-scope-expansion", current: "Do what the brief asks." });
    const box = screen.getByLabelText("no-scope-expansion text");
    expect(box).toHaveValue("Do what the brief asks.");
    expect(screen.queryByText(/add only/i)).not.toBeInTheDocument();
    await user.clear(box);
    await user.type(box, "Use judgement.");
    await user.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith("Use judgement."));
  });
});

// The validator's messages were written for humans; a rewritten version will
// be worse.
it("puts the validator's own message under the field it belongs to", async () => {
  const user = userEvent.setup();
  const onSave = vi
    .fn()
    .mockRejectedValue(
      new PromptError(400, "invalid_override", "store: invalid play: deep step review_work has no brief; a step with nothing to say to whoever holds it is a gap"),
    );
  render(
    <LeafEditor
      label="Brief"
      target={{ kind: "play", play: "deep", step: "review_work", field: "brief" }}
      shipped="shipped brief"
      current="shipped brief"
      onSave={onSave}
      onRevert={vi.fn()}
    />,
  );
  await user.type(screen.getByLabelText("Brief text"), " x");
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent("has no brief; a step with nothing to say to whoever holds it is a gap");
});

describe("a stale edit", () => {
  const stale = {
    value: "Mine, written a while ago.",
    base: "Ours, rewritten since.",
    status: "stale",
    id: "ovr_1",
  } as PromptOverride;

  it("shows both and resolves neither", () => {
    renderLeaf({ override: stale, current: stale.value });
    expect(screen.getByText(/the default changed/i)).toBeInTheDocument();
    // Side by side and labelled, never a merge: these are prose paragraphs and
    // a welded sentence neither of them wrote would ship to an agent.
    const yours = screen.getByText("Yours").parentElement!;
    const ours = screen.getByText("Ours, now").parentElement!;
    expect(yours).toHaveTextContent("Mine, written a while ago.");
    expect(ours).toHaveTextContent("Ours, rewritten since.");
    // His text is still what runs; nothing was reverted for him.
    expect(screen.getByText(/yours is still what runs/i)).toBeInTheDocument();
  });

  it("keeps his when he says so", async () => {
    const user = userEvent.setup();
    const { onSave, onRevert } = renderLeaf({ override: stale, current: stale.value });
    await user.click(screen.getByRole("button", { name: /keep mine/i }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith("Mine, written a while ago."));
    expect(onRevert).not.toHaveBeenCalled();
  });

  it("takes ours when he says so, which is a revert to the shipped text", async () => {
    const user = userEvent.setup();
    const { onSave, onRevert } = renderLeaf({ override: stale, current: stale.value });
    await user.click(screen.getByRole("button", { name: /take the new one/i }));
    await waitFor(() => expect(onRevert).toHaveBeenCalled());
    expect(onSave).not.toHaveBeenCalled();
  });
});

// Cheap, and it is the thing that makes editing safe to try.
describe("revert", () => {
  it("is offered wherever an override exists and nowhere else", async () => {
    const user = userEvent.setup();
    const { onRevert } = renderLeaf({
      override: { id: "ovr_1", value: "mine", status: "applied" } as PromptOverride,
      current: "mine",
    });
    await user.click(screen.getByRole("button", { name: /revert/i }));
    await waitFor(() => expect(onRevert).toHaveBeenCalled());
  });

  it("is absent on an unedited leaf", () => {
    renderLeaf();
    expect(screen.queryByRole("button", { name: /revert/i })).not.toBeInTheDocument();
    expect(screen.getByText(/shipped default, unedited/i)).toBeInTheDocument();
  });
});
