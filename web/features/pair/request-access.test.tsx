// request-access.test.tsx — pairing without a link: ask, show the code the
// computer will show, poll until the computer decides, keep the token.
import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  AgentdError,
  pollPairRequest,
  requestPairAccess,
  setDeviceToken,
} from "@/lib/agentd";
import { POLL_MS, RequestAccess } from "./request-access";

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  requestPairAccess: vi.fn(),
  pollPairRequest: vi.fn(),
  setDeviceToken: vi.fn(),
}));

const IPHONE =
  "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1";

async function flush(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

async function askForAccess(onPaired = vi.fn()) {
  render(<RequestAccess onPaired={onPaired} />);
  fireEvent.click(screen.getByRole("button", { name: "Request access" }));
  await flush();
  return onPaired;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(window.navigator, "userAgent", "get").mockReturnValue(IPHONE);
  vi.mocked(requestPairAccess).mockReset().mockResolvedValue({
    requestId: "req-1",
    pollSecret: "secret-1",
    matchCode: "4821",
    expiresAt: Date.now() + 5 * 60_000,
  });
  vi.mocked(pollPairRequest).mockReset();
  vi.mocked(setDeviceToken).mockReset();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("requesting access", () => {
  it("shows the code, polls, and keeps the device token once approved", async () => {
    vi.mocked(pollPairRequest)
      .mockResolvedValueOnce({ status: "pending" })
      .mockResolvedValueOnce({ status: "approved", deviceToken: "dev-token", deviceId: "dev-1" });
    const onPaired = await askForAccess();

    expect(requestPairAccess).toHaveBeenCalledWith("iPhone · Safari");
    expect(screen.getByTestId("match-code")).toHaveTextContent("4821");
    expect(screen.getByText("agentflow approve 4821")).toBeInTheDocument();
    expect(pollPairRequest).not.toHaveBeenCalled();

    await flush(POLL_MS);
    expect(pollPairRequest).toHaveBeenCalledWith("req-1", "secret-1");
    expect(onPaired).not.toHaveBeenCalled();

    await flush(POLL_MS);
    expect(setDeviceToken).toHaveBeenCalledWith("dev-token");
    expect(onPaired).toHaveBeenCalledTimes(1);

    await flush(POLL_MS * 3);
    expect(pollPairRequest).toHaveBeenCalledTimes(2);
  });

  it("says so when the computer denies it, and lets the person ask again", async () => {
    vi.mocked(pollPairRequest).mockResolvedValue({ status: "denied" });
    const onPaired = await askForAccess();
    await flush(POLL_MS);

    expect(screen.getByRole("alert")).toHaveTextContent(/denied on the computer/);
    expect(setDeviceToken).not.toHaveBeenCalled();
    expect(onPaired).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Ask again" })).toBeInTheDocument();
    await flush(POLL_MS * 3);
    expect(pollPairRequest).toHaveBeenCalledTimes(1);
  });

  it("says the request expired", async () => {
    vi.mocked(pollPairRequest).mockResolvedValue({ status: "expired" });
    await askForAccess();
    await flush(POLL_MS);
    expect(screen.getByRole("alert")).toHaveTextContent(/expired/);
  });

  it("treats a request the daemon no longer knows as expired", async () => {
    vi.mocked(pollPairRequest).mockRejectedValue(new AgentdError(404, "not_found", "no such access request"));
    await askForAccess();
    await flush(POLL_MS);
    expect(screen.getByRole("alert")).toHaveTextContent(/expired/);
  });

  it("keeps polling through a dropped connection", async () => {
    vi.mocked(pollPairRequest)
      .mockRejectedValueOnce(new AgentdError(0, "unreachable", "agentflow is unreachable"))
      .mockResolvedValueOnce({ status: "approved", deviceToken: "dev-token" });
    const onPaired = await askForAccess();
    await flush(POLL_MS);
    expect(onPaired).not.toHaveBeenCalled();
    await flush(POLL_MS);
    expect(onPaired).toHaveBeenCalledTimes(1);
  });

  it("shows why a request could not be made", async () => {
    vi.mocked(requestPairAccess).mockRejectedValue(
      new AgentdError(429, "too_many_requests", "too many access requests are waiting"),
    );
    await askForAccess();
    expect(screen.getByRole("alert")).toHaveTextContent("too many access requests are waiting");
    expect(screen.getByRole("button", { name: "Request access" })).toBeEnabled();
  });
});
