// page.test.tsx — the sessions home: every session from the daemon's list,
// newest first as served, a search box and project / engine / model / state
// filters over what is loaded, a cursor-driven "Load more", and an empty state
// that doesn't pretend a failure is empty.
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AgentdError, listAllSessions, type AllSessionItem } from "@/lib/agentd";
import SessionsHomePage from "./page";

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  listAllSessions: vi.fn(),
}));

// The composer has registries of its own; this file is about the list.
vi.mock("@/features/run/run-modal", () => ({
  RunModal: ({
    open,
    onOpenChange,
    label,
  }: {
    open: boolean;
    onOpenChange: (o: boolean) => void;
    label?: string;
  }) => (
    <>
      <button type="button" onClick={() => onOpenChange(true)}>
        {label}
      </button>
      {open && <div data-testid="composer">composer</div>}
    </>
  ),
}));

// Radix's Select needs pointer capability jsdom does not have, so it never
// opens here. Plain elements stand in for it so the page's own filtering is
// what gets tested.
vi.mock("@/components/ui/select", () => {
  const Ctx = React.createContext<(v: string) => void>(() => {});
  return {
    Select: ({
      value,
      onValueChange,
      children,
    }: {
      value: string;
      onValueChange: (v: string) => void;
      children: React.ReactNode;
    }) => (
      <Ctx.Provider value={onValueChange}>
        <div role="group" data-value={value}>
          {children}
        </div>
      </Ctx.Provider>
    ),
    SelectTrigger: ({ children, ...rest }: { children: React.ReactNode } & Record<string, unknown>) => (
      <button type="button" {...rest}>
        {children}
      </button>
    ),
    SelectContent: ({ children }: { children: React.ReactNode }) => <div role="listbox">{children}</div>,
    SelectItem: ({ value, children }: { value: string; children: React.ReactNode }) => {
      const onValueChange = React.useContext(Ctx);
      return (
        <div role="option" aria-selected={false} onClick={() => onValueChange(value)}>
          {children}
        </div>
      );
    },
    SelectValue: ({ placeholder }: { placeholder?: string }) => <span>{placeholder}</span>,
  };
});

vi.mock("next/link", () => ({
  default: ({ children, href, ...rest }: { children: React.ReactNode; href: string }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

const item = (over: Partial<AllSessionItem>): AllSessionItem => ({
  id: "sess",
  engine: "claude",
  model: "",
  title: "",
  cwd: "",
  project: "",
  updatedAt: Date.now() - 5 * 60_000,
  state: "",
  runId: "",
  ...over,
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SessionsHomePage />
    </QueryClientProvider>,
  );
}

const rows = () => screen.getAllByRole("listitem");
// picker returns the stand-in select whose trigger has the given label.
const picker = (label: string) => {
  const group = screen.getByRole("button", { name: label }).closest<HTMLElement>('[role="group"]');
  if (!group) throw new Error(`no ${label}`);
  return group;
};
const pick = (label: string, option: string) =>
  fireEvent.click(within(picker(label)).getByRole("option", { name: option }));
const optionNames = (label: string) =>
  within(picker(label))
    .getAllByRole("option")
    .map((o) => o.textContent);
const titles = () => rows().map((r) => within(r).getByRole("link").querySelector(".truncate")?.textContent);

beforeEach(() => {
  vi.mocked(listAllSessions).mockReset();
});

describe("the sessions home", () => {
  it("lists sessions from both engines with name, folder, engine and time", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({
      items: [
        item({
          id: "sess-live",
          title: "Release review",
          cwd: "/home/u/code/webapp",
          project: "webapp",
          state: "running",
          runId: "run-7",
        }),
        item({ id: "ses_oc", engine: "opencode", title: "", project: "mobile", cwd: "/home/u/code/mobile" }),
      ],
      nextCursor: "",
    });
    renderPage();

    await waitFor(() => expect(rows()).toHaveLength(2));
    const [live, oc] = rows();
    expect(within(live).getByText("Release review")).toBeInTheDocument();
    expect(within(live).getByText("/home/u/code/webapp")).toBeInTheDocument();
    expect(within(live).getByText("Claude Code")).toBeInTheDocument();
    expect(within(live).getByText("5m ago")).toBeInTheDocument();
    expect(within(live).getByRole("img", { name: "running" })).toBeInTheDocument();
    // a live session opens straight onto the run that holds it
    expect(within(live).getByRole("link")).toHaveAttribute("href", "/session/?id=run-7");

    // no title: named after its project; not live: no dot, opened by session id
    expect(within(oc).getByText("mobile")).toBeInTheDocument();
    expect(within(oc).getByText("OpenCode")).toBeInTheDocument();
    expect(within(oc).queryByRole("img")).not.toBeInTheDocument();
    expect(within(oc).getByRole("link")).toHaveAttribute("href", "/session/?id=ses_oc");
    expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument();
  });

  it("filters loaded sessions by title or folder", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({
      items: [
        item({ id: "a", title: "Fix the export", cwd: "/home/u/code/webapp" }),
        item({ id: "b", title: "Billing bug", cwd: "/home/u/code/payments" }),
      ],
      nextCursor: "",
    });
    renderPage();
    await waitFor(() => expect(rows()).toHaveLength(2));

    const search = screen.getByRole("searchbox", { name: /search sessions/i });
    fireEvent.change(search, { target: { value: "EXPORT" } });
    expect(rows()).toHaveLength(1);
    expect(screen.getByText("Fix the export")).toBeInTheDocument();

    fireEvent.change(search, { target: { value: "payments" } });
    expect(screen.getByText("Billing bug")).toBeInTheDocument();

    fireEvent.change(search, { target: { value: "nothing like it" } });
    expect(screen.queryAllByRole("listitem")).toHaveLength(0);
    expect(screen.getByText(/no sessions match/i)).toBeInTheDocument();
  });

  it("filters loaded sessions by engine", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({
      items: [
        item({ id: "c1", title: "Claude one", cwd: "/home/u/code/webapp" }),
        item({ id: "o1", engine: "opencode", title: "OpenCode one", cwd: "/home/u/code/webapp" }),
        item({ id: "c2", title: "Claude two", cwd: "/home/u/code/payments" }),
      ],
      nextCursor: "",
    });
    renderPage();
    await waitFor(() => expect(rows()).toHaveLength(3));
    expect(optionNames("Engine filter")).toEqual(["All engines", "Claude Code", "OpenCode"]);

    pick("Engine filter", "OpenCode");
    expect(titles()).toEqual(["OpenCode one"]);
    expect(picker("Engine filter")).toHaveAttribute("data-value", "opencode");

    pick("Engine filter", "Claude Code");
    expect(titles()).toEqual(["Claude one", "Claude two"]);

    // Filters and search narrow together.
    fireEvent.change(screen.getByRole("searchbox", { name: /search sessions/i }), {
      target: { value: "payments" },
    });
    expect(titles()).toEqual(["Claude two"]);

    fireEvent.change(screen.getByRole("searchbox", { name: /search sessions/i }), {
      target: { value: "" },
    });
    pick("Engine filter", "All engines");
    expect(rows()).toHaveLength(3);
    expect(picker("Engine filter")).toHaveAttribute("data-value", "all");
  });

  it("filters by project, offering the projects among the loaded sessions", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({
      items: [
        item({ id: "a", title: "Export fix", project: "webapp" }),
        item({ id: "b", title: "Billing bug", project: "payments" }),
        item({ id: "c", title: "Login flow", project: "webapp" }),
        item({ id: "d", title: "No project" }),
      ],
      nextCursor: "",
    });
    renderPage();
    await waitFor(() => expect(rows()).toHaveLength(4));
    // Most sessions first; a row without a project offers no option.
    expect(optionNames("Project filter")).toEqual(["All projects", "webapp (2)", "payments (1)"]);

    pick("Project filter", "webapp (2)");
    expect(titles()).toEqual(["Export fix", "Login flow"]);
    pick("Project filter", "All projects");
    expect(rows()).toHaveLength(4);
  });

  it("filters by state: active while a run holds the session, idle otherwise", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({
      items: [
        item({ id: "r", title: "Running", state: "running", runId: "run-1" }),
        item({ id: "w", title: "Waiting for input", state: "awaiting", runId: "run-2" }),
        item({ id: "f", title: "Finished run", state: "finished", runId: "run-3" }),
        item({ id: "t", title: "Terminal only" }),
      ],
      nextCursor: "",
    });
    renderPage();
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(optionNames("Session state filter")).toEqual(["All states", "Active", "Idle"]);

    pick("Session state filter", "Active");
    expect(titles()).toEqual(["Running", "Waiting for input"]);
    pick("Session state filter", "Idle");
    expect(titles()).toEqual(["Finished run", "Terminal only"]);
  });

  it("offers a model filter that follows the engine once sessions report models", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({
      items: [
        item({ id: "c1", title: "Opus work", model: "claude-opus-5" }),
        item({ id: "c2", title: "Sonnet work", model: "claude-sonnet-5" }),
        item({ id: "c3", title: "More opus", model: "claude-opus-5" }),
        item({ id: "o1", engine: "opencode", title: "OpenCode work", model: "anthropic/claude-sonnet-5" }),
      ],
      nextCursor: "",
    });
    renderPage();
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(optionNames("Model filter")).toEqual([
      "All models",
      "claude-opus-5 (2)",
      "anthropic/claude-sonnet-5 (1)",
      "claude-sonnet-5 (1)",
    ]);

    pick("Model filter", "claude-opus-5 (2)");
    expect(titles()).toEqual(["Opus work", "More opus"]);

    // Choosing an engine clears the model and lists only that engine's models.
    pick("Engine filter", "OpenCode");
    expect(picker("Model filter")).toHaveAttribute("data-value", "all");
    expect(optionNames("Model filter")).toEqual(["All models", "anthropic/claude-sonnet-5 (1)"]);
    expect(titles()).toEqual(["OpenCode work"]);
  });

  it("hides the model filter while no session reports a model", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({
      items: [item({ id: "a", title: "One" }), item({ id: "b", engine: "opencode", title: "Two" })],
      nextCursor: "",
    });
    renderPage();
    await waitFor(() => expect(rows()).toHaveLength(2));
    expect(screen.queryByRole("button", { name: "Model filter" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Project filter" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Session state filter" })).toBeInTheDocument();
  });

  it("says when the filters match nothing among the loaded pages", async () => {
    vi.mocked(listAllSessions)
      .mockResolvedValueOnce({ items: [item({ id: "c1", title: "Claude one" })], nextCursor: "c1" })
      .mockResolvedValueOnce({
        items: [item({ id: "o1", engine: "opencode", title: "Older OpenCode" })],
        nextCursor: "",
      });
    renderPage();
    await waitFor(() => expect(rows()).toHaveLength(1));

    pick("Engine filter", "OpenCode");
    expect(screen.queryAllByRole("listitem")).toHaveLength(0);
    expect(
      screen.getByText("No sessions match these filters in the sessions loaded so far."),
    ).toBeInTheDocument();

    // The filter stays on while the next page loads, and its sessions show.
    fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await waitFor(() => expect(rows()).toHaveLength(1));
    expect(screen.getByText("Older OpenCode")).toBeInTheDocument();

    pick("Session state filter", "Active");
    expect(screen.getByText("No sessions match these filters.")).toBeInTheDocument();
  });

  it("loads the next page with the cursor the daemon returned", async () => {
    vi.mocked(listAllSessions)
      .mockResolvedValueOnce({ items: [item({ id: "a", title: "First" })], nextCursor: "c1" })
      .mockResolvedValueOnce({ items: [item({ id: "b", title: "Second" })], nextCursor: "" });
    renderPage();
    await waitFor(() => expect(rows()).toHaveLength(1));
    expect(listAllSessions).toHaveBeenLastCalledWith({ limit: 100, cursor: "" });

    fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await waitFor(() => expect(rows()).toHaveLength(2));
    expect(listAllSessions).toHaveBeenLastCalledWith({ limit: 100, cursor: "c1" });
    expect(screen.getByText("Second")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument();
  });

  it("says there are no sessions yet when the machine has none", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({ items: [], nextCursor: "" });
    renderPage();
    expect(await screen.findByText("No sessions yet.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("reports a failed load instead of an empty list", async () => {
    vi.mocked(listAllSessions).mockRejectedValue(
      new AgentdError(0, "unreachable", "agentflow is unreachable — is the daemon running?"),
    );
    renderPage();
    expect(await screen.findByRole("alert")).toHaveTextContent(/unreachable/);
    expect(screen.queryByText("No sessions yet.")).not.toBeInTheDocument();
  });

  it("opens the run composer from New session", async () => {
    vi.mocked(listAllSessions).mockResolvedValue({ items: [], nextCursor: "" });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "New session" }));
    expect(screen.getByTestId("composer")).toBeInTheDocument();
  });
});
