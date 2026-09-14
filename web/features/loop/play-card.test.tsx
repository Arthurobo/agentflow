// play-card.test.tsx — picking a play shapes the entire loop, and it used to
// look like picking a radio button. The bar for these tests is the one the
// round set: from arm's length, on a phone, with the grid half scrolled, the
// chosen card has to be obvious.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { PlayCard, spawnedRoles, stepChain } from "./play-card";
import type { Play } from "@/lib/loop";

function play(over: Partial<Play> = {}): Play {
  return {
    name: "deep",
    title: "Deep",
    purpose: "Investigate, review, implement, and keep reviewing until it comes back clean.",
    entry: "brief",
    maxRounds: 3,
    hasCycle: true,
    roles: ["ORCHESTRATOR", "INVESTIGATION", "REVIEW", "IMPLEMENTATION", "ENGINEER"],
    steps: [
      { id: "brief", actor: "ORCHESTRATOR", note: "brief", fanIn: false },
      { id: "investigate", actor: "INVESTIGATION", note: "research", fanIn: false },
      { id: "review_work", actor: "REVIEW", note: "review", fanIn: false },
    ],
    ...over,
  } as Play;
}

function renderCard(selected: boolean, over: Partial<Play> = {}) {
  const onSelect = vi.fn();
  const { container } = render(
    <PlayCard play={play(over)} selected={selected} onSelect={onSelect} />,
  );
  return { onSelect, card: container.querySelector("button")! };
}

describe("a selected play", () => {
  // Not colour: a word, so it survives sunlight, a dimmed screen and anyone
  // who does not separate hues.
  it("says it is selected in words, not only in colour", () => {
    renderCard(true);
    expect(screen.getByText("Selected")).toBeInTheDocument();
    expect(screen.getByRole("button")).toHaveAttribute("aria-pressed", "true");
  });

  it("carries the play's shape: the order, who holds each step", () => {
    renderCard(true);
    const list = screen.getByRole("list");
    expect(within(list).getByText("brief")).toBeInTheDocument();
    expect(within(list).getByText("investigate")).toBeInTheDocument();
    expect(within(list).getAllByText("ORCHESTRATOR").length).toBeGreaterThan(0);
    expect(within(list).getAllByText("REVIEW").length).toBeGreaterThan(0);
  });

  it("says how many agents it will staff, and which", () => {
    renderCard(true);
    // ENGINEER is an address, not a process, so it is not one of them.
    expect(screen.getByText(/Staffs/)).toHaveTextContent("ORCHESTRATOR, INVESTIGATION, REVIEW, IMPLEMENTATION");
    expect(screen.getByText(/Staffs/)).not.toHaveTextContent("ENGINEER");
    expect(screen.getByText("4")).toBeInTheDocument();
  });

  it("explains the cycle rather than only badging it", () => {
    renderCard(true);
    expect(screen.getByText(/cycles up to 3/)).toBeInTheDocument();
    expect(screen.getByText(/until the review reports clean, up to 3 rounds/i)).toBeInTheDocument();
  });

  // Size is the signal that reads from across a room, so the chosen card
  // takes the whole row rather than sitting in a column beside its siblings.
  it("spans the grid and is lifted off it", () => {
    const { card } = renderCard(true);
    expect(card.className).toContain("sm:col-span-2");
    // The SHARED idiom, not a bespoke one: the whole point of the round is
    // that selection looks the same everywhere it appears.
    expect(card.className).toContain("af-selected");
    expect(card).toHaveAttribute("data-selected", "yes");
  });
});

describe("an unselected play", () => {
  it("stays a title and one line", () => {
    renderCard(false);
    expect(screen.queryByText("Selected")).not.toBeInTheDocument();
    expect(screen.getByText("Deep")).toBeInTheDocument();
    // the shape is the chosen card's privilege; showing it on every card is
    // most of why they all looked alike
    expect(screen.queryByRole("list")).not.toBeInTheDocument();
    expect(screen.queryByText(/Staffs/)).not.toBeInTheDocument();
  });

  it("does not take the whole row", () => {
    const { card } = renderCard(false);
    expect(card.className).not.toContain("sm:col-span-2");
    expect(card.className).toContain("af-unselected");
    expect(card.className).not.toContain("af-selected");
    expect(card).toHaveAttribute("data-selected", "no");
  });

  it("still picks when tapped", async () => {
    const user = userEvent.setup();
    const { onSelect, card } = renderCard(false);
    await user.click(card);
    expect(onSelect).toHaveBeenCalled();
  });
});

describe("the pieces the card reads from", () => {
  it("never counts the engineer as an agent to spawn", () => {
    expect(spawnedRoles(play())).toEqual([
      "ORCHESTRATOR",
      "INVESTIGATION",
      "REVIEW",
      "IMPLEMENTATION",
    ]);
    expect(spawnedRoles(play({ roles: ["ENGINEER"] }))).toEqual([]);
  });

  // A play whose steps did not come down the wire still has to show a shape,
  // and its role chain is the same question answered less precisely.
  it("falls back to the role chain when no steps arrived", () => {
    const chain = stepChain(play({ steps: [] }));
    expect(chain.map((s) => s.actor)).toContain("INVESTIGATION");
    expect(chain.every((s) => s.id === "")).toBe(true);
  });

  it("prefers the real steps when they did", () => {
    expect(stepChain(play()).map((s) => s.id)).toEqual(["brief", "investigate", "review_work"]);
  });
});

// The card renders with no steps and no purpose without falling over, which is
// what a play added later and not yet fully described looks like.
it("survives a sparse play", () => {
  render(
    <PlayCard
      play={play({ steps: [], purpose: "", hasCycle: false, roles: [] })}
      selected
      onSelect={vi.fn()}
    />,
  );
  expect(screen.getByText("Selected")).toBeInTheDocument();
  expect(screen.getByText(/Staffs/)).toHaveTextContent("0 agents");
});
