import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { RunForm } from "./run-form";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), prefetch: vi.fn() }),
}));

const LAST_CWD_KEY = "agentflow.run.lastCwd";

function renderForm(props: { resumeSessionId?: string } = {}) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <RunForm {...props} />
    </QueryClientProvider>,
  );
}

// The cwd combobox input; Radix Select triggers also carry role=combobox.
function cwdInput(): HTMLInputElement {
  return screen.getByRole("combobox", { name: "Working directory" });
}

describe("RunForm last-used cwd preselect", () => {
  beforeEach(() => window.localStorage.clear());

  it("preselects the remembered cwd when the form opens empty", () => {
    window.localStorage.setItem(LAST_CWD_KEY, "/home/u/code/webapp");
    renderForm();
    expect(cwdInput()).toHaveValue("/home/u/code/webapp");
  });

  it("keeps an empty cwd when nothing was remembered", () => {
    renderForm();
    expect(cwdInput()).toHaveValue("");
  });

  it("preselects the remembered cwd in the resume path too", () => {
    window.localStorage.setItem(LAST_CWD_KEY, "/home/u/code/webapp");
    // The resume form shares the same composer; the remembered cwd is shown
    // (an empty value would mean "keep the session's original cwd").
    renderForm({ resumeSessionId: "sess-abc" });
    expect(cwdInput()).toHaveValue("/home/u/code/webapp");
  });
});
