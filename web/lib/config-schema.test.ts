// config-schema.test.ts — the session config page said "Failed to load
// config." for every Claude run on this machine.
//
// The backend answered 200 in 1ms. The failure was here: a nil Go slice
// marshals as `null`, not `[]`, and zod's `.default()` only fills in for
// `undefined`. So `"skills": null` was a hard parse failure and `req()`
// threw. Absent, null and empty all mean "there are none" to a reader, so
// they have to mean the same thing to the schema.
import { describe, expect, it } from "vitest";
import { sessionConfig } from "./agentd";

// The exact shape the orchestrator probed off this machine.
const REAL_CLAUDE_PAYLOAD = {
  engine: "claude",
  cwd: "/home/user/Desktop/code/agent_flow",
  summary: {
    files: [
      { path: "/home/a/.claude/settings.json", origin: "settings.json", loaded: true, bytes: 12 },
    ],
    settings: { model: "", permissionMode: "", allowedTools: null, hooks: null },
    skills: null,
    agents: null,
  },
  engineExtras: null,
};

describe("the session config schema", () => {
  it("parses the payload a Claude run actually sends", () => {
    const parsed = sessionConfig.parse(REAL_CLAUDE_PAYLOAD);
    expect(parsed.engine).toBe("claude");
    expect(parsed.summary.skills).toEqual([]);
    expect(parsed.summary.agents).toEqual([]);
    expect(parsed.summary.settings.hooks).toEqual([]);
    expect(parsed.summary.settings.allowedTools).toEqual([]);
    expect(parsed.summary.files).toHaveLength(1);
  });

  it("treats absent, null and empty as the same thing", () => {
    const absent = sessionConfig.parse({
      engine: "claude",
      cwd: "/tmp",
      summary: { settings: {} },
    });
    expect(absent.summary.skills).toEqual([]);
    expect(absent.summary.agents).toEqual([]);
    expect(absent.summary.files).toEqual([]);
    expect(absent.summary.settings.model).toBe("");

    const empty = sessionConfig.parse({
      engine: "claude",
      cwd: "/tmp",
      summary: { files: [], settings: { hooks: [], allowedTools: [] }, skills: [], agents: [] },
    });
    expect(empty.summary).toEqual(absent.summary);
  });

  it("still carries engineExtras when an engine has them", () => {
    const parsed = sessionConfig.parse({
      engine: "opencode",
      cwd: "/tmp",
      summary: { settings: {} },
      engineExtras: { engine: "opencode", reachable: true },
    });
    expect(parsed.engineExtras).toEqual({ engine: "opencode", reachable: true });
  });

  it("still rejects a payload that is genuinely wrong", () => {
    // A missing engine is a real error and must not be smoothed over.
    expect(() => sessionConfig.parse({ cwd: "/tmp", summary: { settings: {} } })).toThrow();
    // A skill without a name is malformed, not merely empty.
    expect(() =>
      sessionConfig.parse({
        engine: "claude",
        cwd: "/tmp",
        summary: { settings: {}, skills: [{ description: "no name" }] },
      }),
    ).toThrow();
  });
});
