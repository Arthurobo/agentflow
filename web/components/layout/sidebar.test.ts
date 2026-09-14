// sidebar.test.ts — the nav highlight is a path boundary match. A bare
// startsWith lights a nav item from any route that merely begins with its
// href, which is how /loop lit up while the old /loops board was open.
import { describe, expect, it } from "vitest";
import { isActive, nav, navActive } from "./sidebar";

describe("isActive", () => {
  it("does not light a nav item whose href is merely a prefix", () => {
    expect(isActive("/loops", "/loop")).toBe(false);
    expect(isActive("/loops/abc", "/loop")).toBe(false);
    expect(isActive("/loopback", "/loop")).toBe(false);
  });

  it("lights the loops board on its own routes", () => {
    expect(isActive("/loop", "/loop")).toBe(true);
    expect(isActive("/loop/938493", "/loop")).toBe(true);
  });

  it("ignores the trailing slash the static export adds", () => {
    expect(isActive("/loop/", "/loop/")).toBe(true);
    expect(isActive("/loop", "/loop/")).toBe(true);
    expect(isActive("/loop/board/", "/loop/")).toBe(true);
    expect(isActive("/session/config/", "/session/")).toBe(true);
    expect(isActive("/loops/", "/loop/")).toBe(false);
  });

  it("keeps the root exact", () => {
    expect(isActive("/", "/")).toBe(true);
    expect(isActive("/stats", "/")).toBe(false);
  });
});

describe("nav", () => {
  // Two Loops entries is the state this replaced: the engine-owned board and
  // the old TTY one, side by side, one of them labelled "TTY loops".
  it("carries exactly one Loops entry, pointing at /loop", () => {
    const loops = nav.filter((n) => n.label.toLowerCase().includes("loop"));
    expect(loops).toHaveLength(1);
    expect(loops[0].href).toBe("/loop/");
    expect(loops[0].label).toBe("Loops");
  });

  it("points Sessions at the home page and has no entries for removed pages", () => {
    expect(nav.find((n) => n.label === "Sessions")?.href).toBe("/");
    const hrefs = nav.map((n) => n.href);
    expect(hrefs).not.toContain("/stats");
    expect(nav.map((n) => n.label.toLowerCase()).join(" ")).not.toMatch(/stats|approval/);
  });
});

describe("navActive", () => {
  const sessions = nav.find((n) => n.label === "Sessions")!;

  it("keeps Sessions lit on a session's terminal and config pages", () => {
    expect(navActive("/", sessions)).toBe(true);
    expect(navActive("/session/", sessions)).toBe(true);
    expect(navActive("/session/config/", sessions)).toBe(true);
  });

  it("does not light Sessions elsewhere", () => {
    expect(navActive("/loop/", sessions)).toBe(false);
    expect(navActive("/sessionsx/", sessions)).toBe(false);
  });
});
