// routes.test.ts — the sessions home is "/" and a session's terminal is
// /session/. The old /run/ and /sessions/ pages are gone, so nothing in the
// app may still link to them: a stale link would land on a 404.
import { existsSync, readdirSync, readFileSync, statSync } from "fs";
import path from "path";
import { describe, expect, it } from "vitest";

const ROOT = path.resolve(__dirname, "..");
const SKIP = new Set(["node_modules", "out", ".next"]);

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    if (SKIP.has(name)) continue;
    const full = path.join(dir, name);
    if (statSync(full).isDirectory()) out.push(...sourceFiles(full));
    else if (/\.(tsx?|jsx?|mjs|json)$/.test(name) && name !== "package-lock.json") out.push(full);
  }
  return out;
}

// A route reference is a string that starts with the path: "/run/…",
// `/sessions/…` or '/run/…'. Import paths such as "@/features/run/…" and API
// paths such as "/api/v1/agentd/sessions/…" don't start with it.
const STALE_ROUTE = /["'`]\/(run|sessions)(\/|["'`?])/;

describe("routes", () => {
  it("has no pages left at /run/ or /sessions/", () => {
    expect(existsSync(path.join(ROOT, "app", "run"))).toBe(false);
    expect(existsSync(path.join(ROOT, "app", "sessions"))).toBe(false);
    expect(existsSync(path.join(ROOT, "app", "session", "page.tsx"))).toBe(true);
    expect(existsSync(path.join(ROOT, "app", "session", "config", "page.tsx"))).toBe(true);
  });

  it("has no link to /run/ or /sessions/ anywhere in the app", () => {
    const self = path.join(ROOT, "lib", "routes.test.ts");
    const stale = sourceFiles(ROOT)
      .filter((f) => f !== self)
      .flatMap((f) =>
        readFileSync(f, "utf8")
          .split("\n")
          .map((line, i) => ({ line, at: `${path.relative(ROOT, f)}:${i + 1}` }))
          .filter(({ line }) => STALE_ROUTE.test(line))
          .map(({ at, line }) => `${at}: ${line.trim()}`),
      );
    expect(stale).toEqual([]);
  });

  it("starts the installed app on the sessions home", () => {
    const manifest = JSON.parse(readFileSync(path.join(ROOT, "public", "manifest.json"), "utf8"));
    expect(manifest.start_url).toBe("/");
  });
});
