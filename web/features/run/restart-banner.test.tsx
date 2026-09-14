import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { RestartBanner } from "./restart-banner";
import { AgentdError, isRunStopped, startRunTty } from "@/lib/agentd";

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  startRunTty: vi.fn(),
}));

function renderBanner(onRestarted = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <RestartBanner runId="run-1" state="stopped" onRestarted={onRestarted} />
    </QueryClientProvider>,
  );
  return onRestarted;
}

beforeEach(() => {
  vi.mocked(startRunTty).mockReset();
});

describe("restarting a stopped session", () => {
  it("asks the daemon for an explicit restart and redials the terminal", async () => {
    vi.mocked(startRunTty).mockResolvedValue({ run: "run-1", tty: true });
    const onRestarted = renderBanner();
    expect(screen.getByRole("status")).toHaveTextContent(/stopped/i);
    fireEvent.click(screen.getByRole("button", { name: /restart session/i }));
    await waitFor(() => expect(onRestarted).toHaveBeenCalledTimes(1));
    expect(startRunTty).toHaveBeenCalledWith("run-1", expect.anything(), { restart: true });
  });

  it("shows why a restart failed and does not redial", async () => {
    vi.mocked(startRunTty).mockRejectedValue(new AgentdError(409, "session_busy", "another run holds this session"));
    const onRestarted = renderBanner();
    fireEvent.click(screen.getByRole("button", { name: /restart session/i }));
    expect(await screen.findByText(/another run holds this session/i)).toBeInTheDocument();
    expect(onRestarted).not.toHaveBeenCalled();
  });
});

describe("isRunStopped", () => {
  it("recognizes only the daemon's run_stopped answer", () => {
    expect(isRunStopped(new AgentdError(410, "run_stopped", "stopped"))).toBe(true);
    expect(isRunStopped(new AgentdError(404, "not_found", "no run"))).toBe(false);
    expect(isRunStopped(new Error("run_stopped"))).toBe(false);
  });
});
