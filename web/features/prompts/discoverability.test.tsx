// discoverability.test.tsx — an edit made three weeks ago silently shapes
// every session he starts. Discoverability of your own past decisions is the
// difference between a feature and a trap.
import { describe, expect, it } from "vitest";
import { playIsEdited, standingIsEdited, overrideFor, type PromptOverride } from "@/lib/prompts";

const o = (over: Partial<PromptOverride>): PromptOverride =>
  ({
    id: "x", kind: "play", play: "", step: "", rule: "", field: "brief", value: "v",
    baseDigest: "", status: "applied", base: "", updatedAt: 0, ...over,
  }) as PromptOverride;

describe("playIsEdited", () => {
  it("marks a play any of whose leaves is overridden", () => {
    expect(playIsEdited([o({ play: "deep", step: "review_work" })], "deep")).toBe(true);
    expect(playIsEdited([o({ play: "deep", field: "purpose" })], "deep")).toBe(true);
  });

  it("does not mark a different play", () => {
    expect(playIsEdited([o({ play: "deep" })], "build")).toBe(false);
  });

  // An orphan is not applied to anything, so calling the play edited would be
  // pointing at a change that is not in effect.
  it("ignores an orphaned edit", () => {
    expect(playIsEdited([o({ play: "deep", status: "orphaned" })], "deep")).toBe(false);
  });

  it("still marks a stale edit, because a stale edit IS in effect", () => {
    expect(playIsEdited([o({ play: "deep", status: "stale" })], "deep")).toBe(true);
  });
});

describe("standingIsEdited", () => {
  const soloRules = ["engineer-drives-git", "house-style"];

  it("notices an edited solo opening", () => {
    expect(standingIsEdited([o({ kind: "solo", field: "intro" })], soloRules)).toBe(true);
  });

  // A rule the solo block names shapes every session too, even though the
  // edit was made from the member panel.
  it("notices a rule the solo block references", () => {
    expect(standingIsEdited([o({ kind: "rule", rule: "house-style", field: "text" })], soloRules)).toBe(true);
  });

  it("ignores a rule no solo session carries", () => {
    expect(standingIsEdited([o({ kind: "rule", rule: "forward-verbatim", field: "text" })], soloRules)).toBe(false);
  });

  it("ignores a play edit, which shapes loops rather than solo sessions", () => {
    expect(standingIsEdited([o({ kind: "play", play: "deep" })], soloRules)).toBe(false);
  });

  it("ignores an orphan", () => {
    expect(standingIsEdited([o({ kind: "solo", field: "intro", status: "orphaned" })], soloRules)).toBe(false);
  });
});

describe("overrideFor", () => {
  it("matches a leaf exactly, never a neighbouring step", () => {
    const list = [
      o({ play: "deep", step: "brief", field: "brief", value: "A" }),
      o({ play: "deep", step: "review_work", field: "brief", value: "B" }),
    ];
    expect(overrideFor(list, { kind: "play", play: "deep", step: "review_work", field: "brief" })?.value).toBe("B");
    expect(overrideFor(list, { kind: "play", play: "deep", step: "plan", field: "brief" })).toBeUndefined();
    // same step, different field
    expect(overrideFor(list, { kind: "play", play: "deep", step: "brief", field: "note" })).toBeUndefined();
  });
});
