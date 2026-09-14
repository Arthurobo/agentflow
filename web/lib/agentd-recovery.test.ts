import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  agentdBase,
  agentdWsUrl,
  clearDeviceToken,
  completePair,
  getDeviceToken,
  listCwds,
  runStatus,
  setDeviceToken,
} from "./agentd";
import { ttyUrl } from "./tty";

const TOKEN_KEY = "agentflow.agentd.deviceToken";

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  window.localStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("device token storage", () => {
  it("keeps the token in localStorage", () => {
    setDeviceToken("tok-123");
    expect(window.localStorage.getItem(TOKEN_KEY)).toBe("tok-123");
    expect(getDeviceToken()).toBe("tok-123");
  });

  it("clears the token", () => {
    setDeviceToken("tok-gone");
    clearDeviceToken();
    expect(getDeviceToken()).toBe("");
    expect(window.localStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("clears a rejected (revoked/unknown) device token on 401", async () => {
    setDeviceToken("tok-revoked");
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({ error: { code: "unauthorized", message: "no" } }, 401),
      ),
    );
    await runStatus("some-run").catch(() => "expected rejection");
    expect(getDeviceToken()).toBe("");
    expect(window.localStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});

describe("same-origin requests", () => {
  it("defaults the API base to the page's own origin", () => {
    expect(agentdBase).toBe("");
  });

  it("calls relative API paths with a bearer token", async () => {
    setDeviceToken("tok-abc");
    const fetchMock = vi.fn(async () => jsonResponse({ id: "r1", state: "running" }));
    vi.stubGlobal("fetch", fetchMock);
    await runStatus("r1");
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/api/v1/agentd/sessions/r1");
    const headers = init.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer tok-abc");
    expect(init.credentials).toBeUndefined();
  });

  it("pairs with only the token and name, and no machine routing header", async () => {
    const fetchMock = vi.fn(async () =>
      jsonResponse({ deviceId: "d1", deviceToken: "dev-tok" }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const res = await completePair({ token: "pair-tok", name: "phone" });
    expect(res.deviceToken).toBe("dev-tok");
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/api/v1/agentd/pair/complete");
    expect(JSON.parse(String(init.body))).toEqual({ token: "pair-tok", name: "phone" });
    const headers = init.headers as Record<string, string>;
    expect(Object.keys(headers).map((k) => k.toLowerCase())).not.toContain(
      "x-agentflow-machine",
    );
  });

  it("builds WebSocket URLs from the page location", () => {
    setDeviceToken("tok-ws");
    const host = window.location.host;
    // jsdom serves the page over http, so the socket is plain ws on that host.
    expect(agentdWsUrl("/api/v1/agentd/x")).toBe(`ws://${host}/api/v1/agentd/x`);
    expect(ttyUrl("run-1")).toBe(
      `ws://${host}/api/v1/agentd/sessions/run-1/tty/ws?token=tok-ws`,
    );
  });

  it("uses wss when the page is served over https", () => {
    vi.stubGlobal("location", { protocol: "https:", host: "agentflow-abc.example.ts.net" });
    expect(agentdWsUrl("/api/v1/agentd/sessions/r/tty/ws")).toBe(
      "wss://agentflow-abc.example.ts.net/api/v1/agentd/sessions/r/tty/ws",
    );
  });
});

describe("recent working directories", () => {
  it("reads the daemon's cwds list", async () => {
    setDeviceToken("tok-cwd");
    const fetchMock = vi.fn(async () =>
      jsonResponse({
        items: [
          { cwd: "/home/u/code/webapp", lastUsedAt: 1757000000000 },
          { cwd: "/srv/api", lastUsedAt: 1756000000000 },
        ],
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const items = await listCwds();
    expect(items.map((i) => i.cwd)).toEqual(["/home/u/code/webapp", "/srv/api"]);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/api/v1/agentd/cwds");
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer tok-cwd");
  });

  it("treats a null items list as empty", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ items: null })));
    expect(await listCwds()).toEqual([]);
  });
});
