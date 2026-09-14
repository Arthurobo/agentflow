// access-requests.test.tsx — a paired browser on this computer can approve or
// deny requests; where the daemon doesn't serve the list, nothing shows.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AgentdError, decidePairRequest, listPairRequests } from "@/lib/agentd";
import { AccessRequests } from "./access-requests";

vi.mock("@/lib/agentd", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/agentd")>()),
  listPairRequests: vi.fn(),
  decidePairRequest: vi.fn(),
}));

const REQUEST = {
  id: "req-1",
  name: "iPhone · Safari",
  matchCode: "4821",
  clientIp: "203.0.113.7",
  createdAt: Date.now() - 30_000,
  expiresAt: Date.now() + 270_000,
};

function renderIt() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AccessRequests />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(listPairRequests).mockReset();
  vi.mocked(decidePairRequest).mockReset().mockResolvedValue({ decision: "approved" });
});

describe("access requests", () => {
  it("lists requests with their code and approves one", async () => {
    vi.mocked(listPairRequests).mockResolvedValueOnce([REQUEST]).mockResolvedValue([]);
    renderIt();
    expect(await screen.findByText("4821")).toBeInTheDocument();
    expect(screen.getByText("iPhone · Safari")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Approve iPhone · Safari" }));
    await waitFor(() => expect(decidePairRequest).toHaveBeenCalledWith("req-1", true));
    await waitFor(() => expect(screen.queryByText("4821")).not.toBeInTheDocument());
  });

  it("denies one", async () => {
    vi.mocked(listPairRequests).mockResolvedValue([REQUEST]);
    renderIt();
    fireEvent.click(await screen.findByRole("button", { name: "Deny iPhone · Safari" }));
    await waitFor(() => expect(decidePairRequest).toHaveBeenCalledWith("req-1", false));
  });

  it("renders nothing where the daemon doesn't offer approvals", async () => {
    vi.mocked(listPairRequests).mockRejectedValue(new AgentdError(404, "not_found", "no such API route"));
    const { container } = renderIt();
    await waitFor(() => expect(listPairRequests).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when no one is waiting", async () => {
    vi.mocked(listPairRequests).mockResolvedValue([]);
    const { container } = renderIt();
    await waitFor(() => expect(listPairRequests).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });
});
