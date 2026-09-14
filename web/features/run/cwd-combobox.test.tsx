import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CwdCombobox, filterCwds } from "./cwd-combobox";
import type { CwdEntry } from "@/lib/agentd";

const NOW = Date.now();

// What GET /api/v1/agentd/cwds returns: most recent first.
const entries: CwdEntry[] = [
  { cwd: "/home/u/code/webapp", lastUsedAt: NOW - 5 * 60_000 },
  { cwd: "/home/u/code/agent_flow", lastUsedAt: NOW - 3 * 3_600_000 },
  { cwd: "/tmp/opencode/e2e-ws", lastUsedAt: NOW - 2 * 86_400_000 },
];

let fetchMock: ReturnType<typeof vi.fn>;

function respondWith(body: unknown, status = 200) {
  fetchMock.mockImplementation(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
  );
}

// Stateful harness mirrors how run-form owns the value (controlled input), so
// typing accumulates and filtering can actually narrow the list.
function Harness({ onChange }: { onChange?: (cwd: string) => void }) {
  const [value, setValue] = useState("");
  return (
    <CwdCombobox
      value={value}
      onChange={(v) => {
        setValue(v);
        onChange?.(v);
      }}
    />
  );
}

function renderCombo(props: { onChange?: (cwd: string) => void } = {}) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <Harness {...props} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem("agentflow.agentd.deviceToken", "tok-dev");
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  respondWith({ items: entries });
});

afterEach(() => vi.unstubAllGlobals());

describe("filterCwds", () => {
  it("matches a case-insensitive substring of the path", () => {
    const hits = filterCwds(entries, "WEBAPP");
    expect(hits).toHaveLength(1);
    expect(hits[0].cwd).toBe("/home/u/code/webapp");
  });

  it("returns everything for an empty query", () => {
    expect(filterCwds(entries, "")).toHaveLength(3);
    expect(filterCwds(entries, "   ")).toHaveLength(3);
  });

  it("returns nothing for a non-matching path fragment", () => {
    expect(filterCwds(entries, "zzz-nope")).toHaveLength(0);
  });
});

describe("CwdCombobox", () => {
  it("loads the daemon's recent directories with the device token", async () => {
    const user = userEvent.setup();
    renderCombo();
    await user.click(screen.getByRole("combobox"));
    expect(await screen.findByText("/home/u/code/webapp")).toBeInTheDocument();
    expect(screen.getByText("/home/u/code/agent_flow")).toBeInTheDocument();
    // when each was last used
    expect(screen.getByText("5m ago")).toBeInTheDocument();
    expect(screen.getByText("2d ago")).toBeInTheDocument();
    expect(screen.getByText("Type a custom path…")).toBeInTheDocument();

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/agentd/cwds");
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer tok-dev");
  });

  it("keeps the daemon's most-recent-first order", async () => {
    const user = userEvent.setup();
    renderCombo();
    await user.click(screen.getByRole("combobox"));
    await screen.findByText("/home/u/code/webapp");
    const options = screen
      .getAllByRole("option")
      .map((o) => o.textContent ?? "")
      .filter((t) => t.startsWith("/"));
    expect(options[0]).toContain("/home/u/code/webapp");
    expect(options[2]).toContain("/tmp/opencode/e2e-ws");
  });

  it("selecting an option fills the value with that path", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderCombo({ onChange });
    await user.click(screen.getByRole("combobox"));
    await user.click(await screen.findByText("/tmp/opencode/e2e-ws"));
    expect(onChange).toHaveBeenCalledWith("/tmp/opencode/e2e-ws");
  });

  it("filters options as the user types", async () => {
    const user = userEvent.setup();
    renderCombo();
    const input = screen.getByRole("combobox");
    await user.click(input);
    await screen.findByText("/home/u/code/webapp");
    await user.type(input, "agent_flow");
    expect(screen.getByText("/home/u/code/agent_flow")).toBeInTheDocument();
    expect(screen.queryByText("/home/u/code/webapp")).not.toBeInTheDocument();
  });

  it("accepts free text as the value", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderCombo({ onChange });
    const input = screen.getByRole("combobox");
    await user.type(input, "/srv/scratch/custom-repo");
    expect(onChange).toHaveBeenLastCalledWith("/srv/scratch/custom-repo");
    expect(input).toHaveValue("/srv/scratch/custom-repo");
    expect(screen.getByText(/No known paths match/)).toBeInTheDocument();
    await user.click(screen.getByText("Type a custom path…"));
    expect(input).toHaveValue("/srv/scratch/custom-repo");
  });

  it("still accepts a typed path when the daemon cannot list directories", async () => {
    respondWith({ error: { code: "not_found", message: "no" } }, 404);
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderCombo({ onChange });
    const input = screen.getByRole("combobox");
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await user.type(input, "/opt/project");
    expect(input).toHaveValue("/opt/project");
    expect(onChange).toHaveBeenLastCalledWith("/opt/project");
  });

  it("supports keyboard navigation: arrow down + enter selects", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderCombo({ onChange });
    const input = screen.getByRole("combobox");
    await user.click(input);
    await screen.findByText("/home/u/code/webapp");
    await user.keyboard("{ArrowDown}{Enter}");
    expect(onChange).toHaveBeenCalledWith("/home/u/code/webapp");
  });
});
