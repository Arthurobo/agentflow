// pair-redirect.test.tsx — the pair page reads a token from its link, strips
// it from the address bar straight away, refuses an expired link, and after
// pairing goes where the visitor was headed without anything in between.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const replace = vi.fn();
const prefetch = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace, prefetch, push: vi.fn() }),
  usePathname: () => "/pair/",
}));

// jsqr pulls a decode path we never exercise here.
vi.mock("jsqr", () => ({ default: vi.fn() }));

const pairComplete = vi.fn();
const setDeviceToken = vi.fn();
const listDevices = vi.fn().mockResolvedValue({ devices: [] });
const refreshDeviceCookie = vi.fn().mockResolvedValue(undefined);
const agentdReady = vi.fn(() => false);

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  // the page imports it as `completePair as pairComplete`
  completePair: (...a: unknown[]) => pairComplete(...a),
  setDeviceToken: (...a: unknown[]) => setDeviceToken(...a),
  listDevices: () => listDevices(),
  revokeDevice: vi.fn(),
  listPairRequests: vi.fn().mockResolvedValue([]),
  refreshDeviceCookie: () => refreshDeviceCookie(),
  agentdReady: () => agentdReady(),
}));

// The request flow has its own tests; here it only has to hand over to the
// page's redirect once the computer approves.
vi.mock("@/features/pair/request-access", () => ({
  RequestAccess: ({ onPaired }: { onPaired: () => void }) => (
    <button type="button" onClick={onPaired}>
      pretend the computer approved
    </button>
  ),
}));

import PairPage from "./page";

function renderPair() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <PairPage />
    </QueryClientProvider>,
  );
}

async function pair() {
  const field = screen.getByPlaceholderText(/paste token from/i);
  fireEvent.change(field, { target: { value: "tok-abc" } });
  fireEvent.click(screen.getByRole("button", { name: /pair device/i }));
}

function visit(url: string) {
  window.history.replaceState(null, "", url);
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.useRealTimers();
  visit("/pair/");
  listDevices.mockResolvedValue({ devices: [] });
  pairComplete.mockResolvedValue({
    deviceId: "dev12345",
    deviceToken: "device-token",
    machineId: "nuc",
  });
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  visit("/");
});

describe("pairing redirects immediately", () => {
  it("goes to the sessions list as soon as the token is stored", async () => {
    renderPair();
    await pair();
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/"));
    expect(setDeviceToken).toHaveBeenCalledWith("device-token");
    expect(pairComplete).toHaveBeenCalledWith({ token: "tok-abc", name: undefined });
  });

  it("does not wait on the devices list before leaving", async () => {
    renderPair();
    listDevices.mockClear();
    await pair();
    await waitFor(() => expect(replace).toHaveBeenCalled());
    expect(listDevices).not.toHaveBeenCalledWith(expect.anything());
  });

  // Asserted by never advancing the clock: with fake timers installed, only
  // promise resolutions can move this along. If anything on the path waited
  // on a timer, replace() would still not have happened.
  it("reaches the redirect without any timer firing", async () => {
    vi.useFakeTimers();
    try {
      renderPair();
      await pair();
      for (let i = 0; i < 20; i++) await Promise.resolve();
      expect(replace).toHaveBeenCalledWith("/");
    } finally {
      vi.useRealTimers();
    }
  });

  it("warms the destination so the redirect is a paint, not a fetch", async () => {
    renderPair();
    await waitFor(() => expect(prefetch).toHaveBeenCalledWith("/"));
    prefetch.mockClear();
    fireEvent.change(screen.getByPlaceholderText(/paste token from/i), {
      target: { value: "tok-abc" },
    });
    await waitFor(() => expect(prefetch).toHaveBeenCalledWith("/"));
  });

  it("returns to the page the visitor was sent here from", async () => {
    visit("/pair/?returnTo=%2Fsession%2F%3Fid%3Drun-7");
    renderPair();
    await pair();
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/session/?id=run-7"));
  });

  it.each([
    ["//evil.example/session/"],
    ["/\\evil.example"],
    ["https://evil.example/"],
    ["javascript:alert(1)"],
  ])("ignores an unsafe returnTo (%s)", async (target) => {
    visit(`/pair/?returnTo=${encodeURIComponent(target)}`);
    renderPair();
    await pair();
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/"));
  });
});

describe("a pairing link", () => {
  it("reads the token from the fragment, strips it, then pairs", async () => {
    const expires = Date.now() + 10 * 60_000;
    visit(`/pair/#token=tok-from-link&expires=${expires}`);
    const replaceState = vi.spyOn(window.history, "replaceState");
    renderPair();
    await waitFor(() =>
      expect(pairComplete).toHaveBeenCalledWith({ token: "tok-from-link", name: undefined }),
    );
    expect(replaceState.mock.calls[0][2]).toBe("/pair/");
    // stripped before the token was sent anywhere
    expect(replaceState.mock.invocationCallOrder[0]).toBeLessThan(
      pairComplete.mock.invocationCallOrder[0],
    );
    expect(window.location.hash).toBe("");
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/"));
  });

  it("keeps returnTo while stripping the token", async () => {
    visit(`/pair/?returnTo=%2Floop%2F#token=tok-2&expires=${Date.now() + 60_000}`);
    renderPair();
    await waitFor(() => expect(pairComplete).toHaveBeenCalled());
    expect(window.location.hash).toBe("");
    expect(window.location.search).toBe("?returnTo=%2Floop%2F");
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/loop/"));
  });

  it("still accepts the older ?token= form, and strips that too", async () => {
    visit("/pair/?token=tok-query");
    renderPair();
    await waitFor(() =>
      expect(pairComplete).toHaveBeenCalledWith({ token: "tok-query", name: undefined }),
    );
    expect(window.location.search).toBe("");
  });

  it("pairs only once when the page renders again", async () => {
    visit(`/pair/#token=tok-once&expires=${Date.now() + 60_000}`);
    const { rerender } = renderPair();
    rerender(
      <QueryClientProvider client={new QueryClient()}>
        <PairPage />
      </QueryClientProvider>,
    );
    await waitFor(() => expect(pairComplete).toHaveBeenCalled());
    expect(pairComplete).toHaveBeenCalledTimes(1);
  });

  it("says the link has expired instead of trying it", async () => {
    visit(`/pair/#token=tok-old&expires=${Date.now() - 1000}`);
    renderPair();
    expect(await screen.findByRole("alert")).toHaveTextContent(/expired/i);
    expect(screen.getByRole("alert")).toHaveTextContent("agentflow pair");
    expect(pairComplete).not.toHaveBeenCalled();
    // the dead token is still removed from the address bar
    expect(window.location.hash).toBe("");
  });
});

describe("what the page says", () => {
  it("carries the progress in the button, with no paragraph to read", async () => {
    renderPair();
    const button = screen.getByRole("button", { name: /pair device/i });
    expect(button).toHaveAttribute("data-state", "idle");

    await pair();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /opening console/i })).toHaveAttribute(
        "data-state",
        "paired",
      ),
    );
  });

  it("returns to idle and explains itself when pairing fails", async () => {
    pairComplete.mockRejectedValue(new Error("pairing token invalid or expired"));
    renderPair();
    await pair();
    await waitFor(() => expect(screen.getByText(/pairing failed|invalid/i)).toBeInTheDocument());
    expect(replace).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: /pair device/i })).toHaveAttribute(
      "data-state",
      "idle",
    );
  });

  it("names the real command and explains adding to the home screen", () => {
    const { container } = renderPair();
    const text = container.textContent ?? "";
    expect(text).toContain("agentflow pair");
    expect(text).not.toMatch(/agentd pair|approvals|E2E|encrypted/i);
    expect(text).toMatch(/Add to Home Screen/);
    expect(text).toMatch(/Share → Add to Home Screen/);
    expect(text).toMatch(/Install app/);
  });
});

describe("requesting access instead of using a link", () => {
  it("offers it on a page opened without a link, and redirects once approved", async () => {
    visit("/pair/?returnTo=%2Floop%2F");
    renderPair();
    fireEvent.click(screen.getByRole("button", { name: /pretend the computer approved/i }));
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/loop/"));
    expect(pairComplete).not.toHaveBeenCalled();
  });
});

describe("a browser that is already paired", () => {
  it("fetches the cookie the public address needs", async () => {
    agentdReady.mockReturnValue(true);
    try {
      renderPair();
      await waitFor(() => expect(refreshDeviceCookie).toHaveBeenCalledTimes(1));
    } finally {
      agentdReady.mockReturnValue(false);
    }
  });

  it("is not asked for when there is no token yet", async () => {
    renderPair();
    await waitFor(() => expect(prefetch).toHaveBeenCalled());
    expect(refreshDeviceCookie).not.toHaveBeenCalled();
  });
});
