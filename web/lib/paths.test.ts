import { describe, expect, it } from "vitest";
import {
  HOME_PATH,
  isRoute,
  normalizePath,
  pairUrlFor,
  safeReturnTo,
  sessionConfigPath,
  sessionPath,
} from "./paths";

describe("normalizePath", () => {
  it("treats a trailing slash as the same route", () => {
    expect(normalizePath("/pair/")).toBe("/pair");
    expect(normalizePath("/pair")).toBe("/pair");
    expect(normalizePath("/")).toBe("/");
    expect(normalizePath("")).toBe("/");
    expect(normalizePath(null)).toBe("/");
    expect(isRoute("/pair/", "/pair")).toBe(true);
    expect(isRoute("/pair", "/pair/")).toBe(true);
    expect(isRoute("/pairing/", "/pair/")).toBe(false);
  });
});

describe("safeReturnTo", () => {
  it("keeps a local path with its query string", () => {
    expect(safeReturnTo("/session/?id=run-1")).toBe("/session/?id=run-1");
    expect(safeReturnTo("/loop/")).toBe("/loop/");
  });

  it("rejects anything that could leave the origin", () => {
    expect(safeReturnTo("//evil.example/")).toBeNull();
    expect(safeReturnTo("/\\evil.example")).toBeNull();
    expect(safeReturnTo("/\t/evil.example")).toBeNull();
    expect(safeReturnTo("https://evil.example/")).toBeNull();
    expect(safeReturnTo("javascript:alert(1)")).toBeNull();
    expect(safeReturnTo("run/")).toBeNull();
    expect(safeReturnTo("")).toBeNull();
    expect(safeReturnTo(null)).toBeNull();
  });

  it("will not send a paired device back to the pair page", () => {
    expect(safeReturnTo("/pair/")).toBeNull();
    expect(safeReturnTo("/pair?returnTo=/session/")).toBeNull();
  });
});

describe("pairUrlFor", () => {
  it("remembers where the visitor was headed", () => {
    expect(pairUrlFor("/session/?id=run-1")).toBe(
      "/pair/?returnTo=%2Fsession%2F%3Fid%3Drun-1",
    );
  });

  it("adds nothing for the home page or an unsafe path", () => {
    expect(pairUrlFor("/")).toBe("/pair/");
    expect(pairUrlFor("//evil.example")).toBe("/pair/");
  });
});

describe("session routes", () => {
  it("puts the sessions list at the root", () => {
    expect(HOME_PATH).toBe("/");
  });

  it("carries the id in the query string, escaped", () => {
    expect(sessionPath("run-1")).toBe("/session/?id=run-1");
    expect(sessionPath("a#b&c")).toBe("/session/?id=a%23b%26c");
    expect(sessionConfigPath("run-1")).toBe("/session/config/?id=run-1");
  });
});
