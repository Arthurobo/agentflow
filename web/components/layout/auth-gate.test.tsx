// auth-gate.test.tsx — an unpaired visitor is sent to the pair page with a
// note of where they were going, and the pair page itself (at either spelling
// of its path) is never gated.
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const replace = vi.fn();
let pathname = "/";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace }),
  usePathname: () => pathname,
}));

import { AuthGate } from "./auth-gate";
import { setDeviceToken } from "@/lib/agentd";

function renderGate() {
  return render(
    <AuthGate>
      <p>protected</p>
    </AuthGate>,
  );
}

beforeEach(() => {
  replace.mockReset();
  window.localStorage.clear();
});

afterEach(() => {
  window.history.replaceState(null, "", "/");
});

describe("AuthGate", () => {
  it("sends an unpaired visitor to the pair page with returnTo", async () => {
    pathname = "/session/";
    window.history.replaceState(null, "", "/session/?id=run-9");
    renderGate();
    await waitFor(() =>
      expect(replace).toHaveBeenCalledWith("/pair/?returnTo=%2Fsession%2F%3Fid%3Drun-9"),
    );
    expect(screen.queryByText("protected")).not.toBeInTheDocument();
  });

  it("sends a visitor to the home page to the plain pair page", async () => {
    pathname = "/";
    window.history.replaceState(null, "", "/");
    renderGate();
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/pair/"));
  });

  it.each(["/pair", "/pair/"])("never gates the pair page at %s", (p) => {
    pathname = p;
    window.history.replaceState(null, "", p);
    renderGate();
    expect(screen.getByText("protected")).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
  });

  it.each(["/pairing/", "/login/"])("gates every other page, including %s", async (p) => {
    pathname = p;
    window.history.replaceState(null, "", p);
    renderGate();
    await waitFor(() => expect(replace).toHaveBeenCalled());
    expect(screen.queryByText("protected")).not.toBeInTheDocument();
  });

  it("renders protected pages once a token is stored", () => {
    pathname = "/session/";
    setDeviceToken("tok-in");
    renderGate();
    expect(screen.getByText("protected")).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
  });
});
