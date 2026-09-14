// page.test.tsx — /session/?id= opens a terminal for either kind of id: a
// managed run id attaches, an engine session id is resumed into a run and the
// URL moves onto that run.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  AgentdError,
  ensureTtyBySession,
  runForSession,
  runStatus,
  startRunTty,
  type RunSession,
} from "@/lib/agentd";
import SessionPage from "./page";

let search = new URLSearchParams();
const replace = vi.fn();
vi.mock("next/navigation", () => ({
  useSearchParams: () => search,
  useRouter: () => ({ replace }),
}));

vi.mock("next/link", () => ({
  default: ({ children, href }: { children: React.ReactNode; href: string }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("@/features/run/terminal-shell", () => ({
  TerminalShell: ({ runId }: { runId: string }) => <div data-testid="terminal">pty {runId}</div>,
}));

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  runStatus: vi.fn(),
  ensureTtyBySession: vi.fn(),
  runForSession: vi.fn(),
  startRunTty: vi.fn(),
}));

function renderAt(id: string) {
  search = new URLSearchParams({ id });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SessionPage />
    </QueryClientProvider>,
  );
}

const notFound = () => new AgentdError(404, "not_found", "run not found");

beforeEach(() => {
  replace.mockReset();
  vi.mocked(runStatus).mockReset();
  vi.mocked(ensureTtyBySession).mockReset();
  vi.mocked(runForSession).mockReset();
  vi.mocked(startRunTty).mockReset();
});

describe("the session page", () => {
  it("attaches to a managed run id without resuming anything", async () => {
    vi.mocked(runStatus).mockResolvedValue({ id: "run-7", state: "running" } as RunSession);
    renderAt("run-7");
    expect(await screen.findByTestId("terminal")).toHaveTextContent("pty run-7");
    expect(runStatus).toHaveBeenCalledWith("run-7");
    expect(ensureTtyBySession).not.toHaveBeenCalled();
    expect(replace).not.toHaveBeenCalled();
  });

  it("resumes an engine session id, then moves the URL onto its run", async () => {
    vi.mocked(runStatus).mockRejectedValue(notFound());
    vi.mocked(ensureTtyBySession).mockResolvedValue({ id: "run-9", sessionId: "sess-abc" });
    renderAt("sess-abc");
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/session/?id=run-9"));
    expect(ensureTtyBySession).toHaveBeenCalledWith("sess-abc", expect.anything());
    expect(screen.queryByTestId("terminal")).not.toBeInTheDocument();
  });

  it("offers a restart for a session whose run was stopped on purpose", async () => {
    vi.mocked(runStatus).mockRejectedValue(notFound());
    vi.mocked(ensureTtyBySession).mockRejectedValue(
      new AgentdError(410, "run_stopped", "the run was stopped"),
    );
    vi.mocked(runForSession).mockResolvedValue({ id: "run-3", state: "stopped" } as RunSession);
    vi.mocked(startRunTty).mockResolvedValue({ run: "run-3", tty: true });
    renderAt("sess-stopped");

    // the resume is retried once before the page gives up on it
    expect(
      await screen.findByText("this session was stopped", undefined, { timeout: 4000 }),
    ).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /restart session/i }));
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/session/?id=run-3"));
    expect(startRunTty).toHaveBeenCalledWith("run-3", expect.anything(), { restart: true });
  });

  it("shows why it could not open instead of resuming on other errors", async () => {
    vi.mocked(runStatus).mockRejectedValue(
      new AgentdError(0, "unreachable", "agentflow is unreachable — is the daemon running?"),
    );
    renderAt("whatever");
    expect(await screen.findByText("could not open the terminal")).toBeInTheDocument();
    expect(screen.getByText(/unreachable/)).toBeInTheDocument();
    expect(ensureTtyBySession).not.toHaveBeenCalled();
    expect(screen.getByText("Sessions").closest("a")).toHaveAttribute("href", "/");
  });
});
