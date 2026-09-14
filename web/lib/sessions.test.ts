// sessions.test.ts — what a row in the sessions home is called, and what the
// search box and the filters match, and which filter options the loaded rows
// offer.
import { describe, expect, it } from "vitest";
import type { AllSessionItem } from "@/lib/agentd";
import {
  hasFilters,
  matchesEngine,
  matchesFilters,
  matchesSearch,
  modelOptions,
  NO_FILTERS,
  projectOptions,
  sessionLabel,
} from "@/lib/sessions";

const item = (over: Partial<AllSessionItem>): AllSessionItem => ({
  id: "0123456789abcdef",
  engine: "claude",
  model: "",
  title: "",
  cwd: "",
  project: "",
  updatedAt: 0,
  state: "",
  runId: "",
  ...over,
});

describe("sessionLabel", () => {
  it("prefers the session's own name", () => {
    expect(sessionLabel(item({ title: "Release review", project: "webapp" }))).toBe("Release review");
  });

  it("falls back to the project, then the short id", () => {
    expect(sessionLabel(item({ title: "  ", project: "webapp" }))).toBe("webapp");
    expect(sessionLabel(item({}))).toBe("01234567");
  });
});

describe("matchesSearch", () => {
  const s = item({ title: "Fix the Export", cwd: "/home/u/code/webapp", project: "webapp" });

  it("matches the title or the folder, ignoring case", () => {
    expect(matchesSearch(s, "export")).toBe(true);
    expect(matchesSearch(s, "CODE/WEB")).toBe(true);
  });

  it("matches everything for an empty query and nothing unrelated", () => {
    expect(matchesSearch(s, "  ")).toBe(true);
    expect(matchesSearch(s, "billing")).toBe(false);
  });
});

describe("matchesEngine", () => {
  it("keeps every engine when no engine is chosen", () => {
    expect(matchesEngine(item({ engine: "claude" }), "")).toBe(true);
    expect(matchesEngine(item({ engine: "opencode" }), "")).toBe(true);
  });

  it("keeps only the chosen engine", () => {
    expect(matchesEngine(item({ engine: "opencode" }), "opencode")).toBe(true);
    expect(matchesEngine(item({ engine: "claude" }), "opencode")).toBe(false);
    expect(matchesEngine(item({ engine: "opencode" }), "claude")).toBe(false);
  });

  it("counts a row without an engine as Claude Code", () => {
    expect(matchesEngine(item({ engine: "" }), "claude")).toBe(true);
    expect(matchesEngine(item({ engine: "" }), "opencode")).toBe(false);
  });
});

describe("matchesFilters", () => {
  const row = item({ engine: "opencode", model: "anthropic/claude-sonnet-5", project: "mobile", state: "running" });

  it("keeps every row with no filters", () => {
    expect(hasFilters(NO_FILTERS)).toBe(false);
    expect(matchesFilters(row, NO_FILTERS)).toBe(true);
  });

  it("requires every chosen filter to match", () => {
    const f = { project: "mobile", engine: "opencode" as const, model: "anthropic/claude-sonnet-5", state: "active" as const };
    expect(hasFilters(f)).toBe(true);
    expect(matchesFilters(row, f)).toBe(true);
    expect(matchesFilters(row, { ...f, project: "webapp" })).toBe(false);
    expect(matchesFilters(row, { ...f, engine: "claude" })).toBe(false);
    expect(matchesFilters(row, { ...f, model: "claude-opus-5" })).toBe(false);
    expect(matchesFilters(row, { ...f, state: "idle" })).toBe(false);
  });

  it("treats live run states as active and everything else as idle", () => {
    for (const state of ["starting", "running", "awaiting"]) {
      expect(matchesFilters(item({ state }), { ...NO_FILTERS, state: "active" })).toBe(true);
    }
    for (const state of ["", "finished", "stopped", "failed"]) {
      expect(matchesFilters(item({ state }), { ...NO_FILTERS, state: "idle" })).toBe(true);
      expect(matchesFilters(item({ state }), { ...NO_FILTERS, state: "active" })).toBe(false);
    }
  });
});

describe("filter options", () => {
  const rows = [
    item({ project: "webapp", model: "claude-opus-5" }),
    item({ project: "payments", model: "claude-opus-5" }),
    item({ project: "webapp", model: "" }),
    item({ project: "", engine: "opencode", model: "openai/gpt-5" }),
  ];

  it("lists projects by how many loaded sessions have them, skipping blanks", () => {
    expect(projectOptions(rows)).toEqual([
      { name: "webapp", count: 2 },
      { name: "payments", count: 1 },
    ]);
  });

  it("lists the models of the chosen engine only", () => {
    expect(modelOptions(rows, "")).toEqual([
      { name: "claude-opus-5", count: 2 },
      { name: "openai/gpt-5", count: 1 },
    ]);
    expect(modelOptions(rows, "claude")).toEqual([{ name: "claude-opus-5", count: 2 }]);
    expect(modelOptions(rows, "opencode")).toEqual([{ name: "openai/gpt-5", count: 1 }]);
  });
});
