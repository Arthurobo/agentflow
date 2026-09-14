// run-form-mode.test.tsx — the standing-rules toggle. Not all sessions are
// loops, and a run-form session spawns with permissions bypassed exactly as a
// loop member does, so it carries the same floor.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { RunForm } from "./run-form";
import { listCwds, listEngines, spawnRun } from "@/lib/agentd";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

vi.mock("@/lib/agentd", async (o) => ({
  ...(await o<typeof import("@/lib/agentd")>()),
  spawnRun: vi.fn(),
  listEngines: vi.fn(),
  listCwds: vi.fn(),
}));

function renderForm() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <RunForm />
    </QueryClientProvider>,
  );
}

async function fillAndSubmit(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText(/session name/i), "A run");
  await user.type(
    screen.getByPlaceholderText(/home\/you|\/path|folder|code/i),
    "/tmp/work",
  );
  await user.click(screen.getByRole("button", { name: /start run/i }));
}

beforeEach(() => {
  window.localStorage.clear();
  push.mockReset();
  vi.mocked(spawnRun).mockReset();
  vi.mocked(spawnRun).mockResolvedValue({ id: "run-1" } as never);
  vi.mocked(listEngines).mockResolvedValue({ engines: [] } as never);
  vi.mocked(listCwds).mockResolvedValue([]);
});

describe("the standing rules toggle", () => {
  it("defaults to the full set", async () => {
    const user = userEvent.setup();
    renderForm();
    expect(
      screen.getByRole("button", { name: "Standing rules" }),
    ).toHaveAttribute("aria-pressed", "true");
    await fillAndSubmit(user);
    await waitFor(() => expect(spawnRun).toHaveBeenCalled());
    expect(vi.mocked(spawnRun).mock.calls[0][0].promptMode).toBe("standing");
  });

  it("sends scratch when he asks for it", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.click(screen.getByRole("button", { name: "Scratch" }));
    await fillAndSubmit(user);
    await waitFor(() => expect(spawnRun).toHaveBeenCalled());
    expect(vi.mocked(spawnRun).mock.calls[0][0].promptMode).toBe("scratch");
  });

  // Scratch is not off, and the form has to say so, because the whole point is
  // that the floor is not a default someone can talk themselves out of.
  it("says plainly that git and deploys survive scratch", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.click(screen.getByRole("button", { name: "Scratch" }));
    expect(
      screen.getByText(/git and deploys stay locked/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/not a default, it is a floor/i),
    ).toBeInTheDocument();
  });

  it("remembers the choice per device, like the engine and the cwd", async () => {
    const user = userEvent.setup();
    const first = renderForm();
    await user.click(screen.getByRole("button", { name: "Scratch" }));
    await waitFor(() =>
      expect(window.localStorage.getItem("agentflow.run.lastPromptMode")).toBe(
        "scratch",
      ),
    );
    first.unmount();

    renderForm();
    expect(screen.getByRole("button", { name: "Scratch" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });
});
