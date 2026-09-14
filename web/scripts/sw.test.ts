// @vitest-environment node
//
// sw.test.ts — runs the real public/sw.js in a sandbox with a fake worker
// global, and the real scripts/stamp-sw.mjs against a throwaway build tree.
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import vm from "node:vm";
import { afterEach, describe, expect, it, vi } from "vitest";

const ROOT = join(__dirname, "..");
const SW_SOURCE = readFileSync(join(ROOT, "public", "sw.js"), "utf8");
const ORIGIN = "https://agentflow-3fa9c1.tail1234.ts.net";

type Listener = (event: unknown) => void;

function loadWorker(source = SW_SOURCE) {
  const listeners: Record<string, Listener> = {};
  const put = vi.fn().mockResolvedValue(undefined);
  const match = vi.fn().mockResolvedValue(undefined);
  const cacheNames: string[] = [];
  const caches = {
    open: vi.fn(async (name: string) => {
      cacheNames.push(name);
      return { put, match };
    }),
    keys: vi.fn(async () => ["agentflow-old", "agentflow-__BUILD_ID__", "someone-else"]),
    delete: vi.fn(async () => true),
  };
  const fetchMock = vi.fn(async () => ({
    status: 200,
    type: "basic",
    clone() {
      return this;
    },
  }));
  const self = {
    location: { origin: ORIGIN },
    addEventListener: (type: string, fn: Listener) => {
      listeners[type] = fn;
    },
    skipWaiting: vi.fn(async () => undefined),
    clients: { claim: vi.fn(async () => undefined) },
  };
  vm.runInNewContext(source, { self, caches, fetch: fetchMock, URL, Promise });

  async function request(url: string, method = "GET") {
    let responded: Promise<unknown> | null = null;
    listeners.fetch({
      request: { url, method },
      respondWith: (p: Promise<unknown>) => {
        responded = p;
      },
    });
    if (responded) await responded;
    return { handled: responded !== null };
  }

  return { listeners, caches, cacheNames, put, match, fetchMock, request };
}

describe("service worker fetch routing", () => {
  it.each([
    `${ORIGIN}/api/v1/agentd/sessions`,
    `${ORIGIN}/api/v1/agentd/cwds`,
    `${ORIGIN}/api/v1/agentd/devices`,
    `${ORIGIN}/api`,
  ])("never touches API responses (%s)", async (url) => {
    const w = loadWorker();
    expect((await w.request(url)).handled).toBe(false);
    expect(w.put).not.toHaveBeenCalled();
    expect(w.caches.open).not.toHaveBeenCalled();
  });

  it.each([
    `${ORIGIN}/`,
    `${ORIGIN}/session/?id=run-1`,
    `${ORIGIN}/session/config/?id=run-1`,
    `${ORIGIN}/pair/`,
    `${ORIGIN}/manifest.json`,
    `${ORIGIN}/sw.js`,
  ])("leaves pages and other files to the network (%s)", async (url) => {
    const w = loadWorker();
    expect((await w.request(url)).handled).toBe(false);
    expect(w.put).not.toHaveBeenCalled();
  });

  it("ignores other origins, even for asset-looking paths", async () => {
    const w = loadWorker();
    expect((await w.request("https://cdn.example.com/_next/static/chunks/a.js")).handled).toBe(
      false,
    );
    expect((await w.request("https://evil.example/icons/icon-192.png")).handled).toBe(false);
  });

  it("ignores non-GET requests to cacheable paths", async () => {
    const w = loadWorker();
    expect((await w.request(`${ORIGIN}/_next/static/chunks/a.js`, "POST")).handled).toBe(false);
  });

  it("caches build assets and icons", async () => {
    const w = loadWorker();
    expect((await w.request(`${ORIGIN}/_next/static/chunks/app-123.js`)).handled).toBe(true);
    expect((await w.request(`${ORIGIN}/icons/icon-192.png`)).handled).toBe(true);
    expect(w.put).toHaveBeenCalledTimes(2);
  });

  it("does not store a failed asset response", async () => {
    const w = loadWorker();
    w.fetchMock.mockResolvedValueOnce({
      status: 404,
      type: "basic",
      clone() {
        return this;
      },
    });
    await w.request(`${ORIGIN}/_next/static/chunks/missing.js`);
    expect(w.put).not.toHaveBeenCalled();
  });

  it("precaches nothing on install", async () => {
    const w = loadWorker();
    let waited: Promise<unknown> = Promise.resolve();
    w.listeners.install({ waitUntil: (p: Promise<unknown>) => (waited = p) });
    await waited;
    expect(w.caches.open).not.toHaveBeenCalled();
  });

  it("deletes older agentflow caches on activate, and only those", async () => {
    const w = loadWorker(SW_SOURCE.replace("__BUILD_ID__", "build-2"));
    let waited: Promise<unknown> = Promise.resolve();
    w.listeners.activate({ waitUntil: (p: Promise<unknown>) => (waited = p) });
    await waited;
    const deleted = w.caches.delete.mock.calls.map((c) => (c as unknown[])[0]);
    expect(deleted).toEqual(["agentflow-old", "agentflow-__BUILD_ID__"]);
  });
});

describe("stamp-sw", () => {
  let dir = "";
  afterEach(() => {
    if (dir) rmSync(dir, { recursive: true, force: true });
    dir = "";
  });

  function tree(buildIdFile: string | null, staticDirs: string[]) {
    dir = mkdtempSync(join(tmpdir(), "stamp-sw-"));
    mkdirSync(join(dir, "out", "_next", "static"), { recursive: true });
    for (const d of staticDirs) mkdirSync(join(dir, "out", "_next", "static", d));
    writeFileSync(join(dir, "out", "sw.js"), SW_SOURCE);
    if (buildIdFile !== null) {
      mkdirSync(join(dir, ".next"));
      writeFileSync(join(dir, ".next", "BUILD_ID"), buildIdFile);
    }
    return dir;
  }

  function stamp(root: string) {
    return execFileSync(process.execPath, [join(ROOT, "scripts", "stamp-sw.mjs"), root], {
      stdio: "pipe",
    });
  }

  it("writes the build id into the cache name", () => {
    const root = tree("AbC_123-xyz\n", ["chunks", "AbC_123-xyz"]);
    stamp(root);
    const out = readFileSync(join(root, "out", "sw.js"), "utf8");
    expect(out).toContain('CACHE_PREFIX + "AbC_123-xyz"');
    expect(out).not.toContain("__BUILD_ID__");
  });

  it("falls back to the build directory under out/_next/static", () => {
    const root = tree(null, ["chunks", "css", "media", "q9Build"]);
    stamp(root);
    expect(readFileSync(join(root, "out", "sw.js"), "utf8")).toContain('"q9Build"');
  });

  it("fails the build when there is no build id", () => {
    const root = tree(null, ["chunks"]);
    expect(() => stamp(root)).toThrow();
  });
});
