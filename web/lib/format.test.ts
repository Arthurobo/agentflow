import { describe, expect, it } from "vitest";
import { formatTs, relTime } from "./format";

describe("formatTs", () => {
  it("renders a dash for an absent timestamp", () => {
    expect(formatTs(0)).toBe("—");
  });
});

describe("relTime", () => {
  it("renders nothing for an absent timestamp", () => {
    expect(relTime(0)).toBe("");
    expect(relTime(undefined)).toBe("");
  });

  it("floors rather than rounds", () => {
    const now = Date.now();
    expect(relTime(now - 119_000)).toBe("1m ago");
    expect(relTime(now - 59_000)).toBe("59s ago");
    expect(relTime(now - 7_100_000)).toBe("1h ago");
  });
});
