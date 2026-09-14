// device-cookie.test.ts — a browser paired before the public address needed
// a cookie asks for one with its device token; a token the daemon rejects is
// dropped like any other 401.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { getDeviceToken, refreshDeviceCookie, setDeviceToken } from "./agentd";

const fetchMock = vi.fn();

beforeEach(() => {
  window.localStorage.clear();
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("refreshDeviceCookie", () => {
  it("posts the stored device token", async () => {
    setDeviceToken("tok-1");
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    await refreshDeviceCookie();
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/agentd/pair/cookie",
      expect.objectContaining({ method: "POST", headers: { Authorization: "Bearer tok-1" } }),
    );
    expect(getDeviceToken()).toBe("tok-1");
  });

  it("drops a token the daemon no longer accepts", async () => {
    setDeviceToken("revoked");
    fetchMock.mockResolvedValue(new Response(null, { status: 401 }));
    await refreshDeviceCookie();
    expect(getDeviceToken()).toBe("");
  });

  it("does nothing without a token", async () => {
    await refreshDeviceCookie();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
