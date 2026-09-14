import { describe, it, expect } from "vitest";
import { promptsSchema } from "./prompts";

describe("promptsSchema null tolerance", () => {
  it("accepts null collections the Go backend emits for empties", () => {
    const parsed = promptsSchema.parse({
      plays: [{ Name: "recon", Title: "Recon" }],
      rules: { ORCH: [{ name: "r", text: "t" }] },
      roles: { X: "y" }, // extra key is ignored
      solo: { Intro: "hi", Rules: null },
      overrides: null, // <- the field that was crashing the run-agent editor
      dropped: null,
      defaultPlays: null,
      defaultRules: null,
    });
    expect(parsed.overrides).toEqual([]);
    expect(parsed.dropped).toEqual({});
    expect(parsed.defaultPlays).toEqual([]);
    expect(parsed.solo.Rules).toEqual([]);
  });
});
