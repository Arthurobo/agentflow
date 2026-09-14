import { describe, expect, it, vi } from "vitest";
import { fitOnce } from "./xterm-fit";

describe("fitOnce", () => {
  it("reports failure when the font has not been measured yet", () => {
    // This is the exact production case: right after Terminal.open() the
    // renderer has not measured the cell, so proposeDimensions() returns
    // undefined and FitAddon.fit() would silently do nothing. Treating that
    // as success is what left the TUI at 80x24 until a reload.
    const fit = { proposeDimensions: () => undefined, fit: vi.fn() };
    const term = { cols: 80, rows: 24 };
    expect(fitOnce(fit, term)).toBe(false);
    expect(fit.fit).not.toHaveBeenCalled();
  });

  it("fits and reports success when dimensions differ", () => {
    const fit = { proposeDimensions: () => ({ cols: 120, rows: 46 }), fit: vi.fn() };
    const term = { cols: 80, rows: 24 };
    expect(fitOnce(fit, term)).toBe(true);
    expect(fit.fit).toHaveBeenCalledTimes(1);
  });

  it("reports success without resizing when already correct", () => {
    // Avoids a pointless SIGWINCH: every resize makes the TUI repaint its
    // whole frame, which is what stacked ghost copies of the transcript.
    const fit = { proposeDimensions: () => ({ cols: 100, rows: 40 }), fit: vi.fn() };
    const term = { cols: 100, rows: 40 };
    expect(fitOnce(fit, term)).toBe(true);
    expect(fit.fit).not.toHaveBeenCalled();
  });

  it("rejects a collapsed container", () => {
    const fit = { proposeDimensions: () => ({ cols: 0, rows: 0 }), fit: vi.fn() };
    expect(fitOnce(fit, { cols: 80, rows: 24 })).toBe(false);
    expect(fit.fit).not.toHaveBeenCalled();
  });

  it("rejects NaN dimensions", () => {
    const fit = { proposeDimensions: () => ({ cols: NaN, rows: NaN }), fit: vi.fn() };
    expect(fitOnce(fit, { cols: 80, rows: 24 })).toBe(false);
  });

  it("survives a throwing addon and a missing terminal", () => {
    const throwing = {
      proposeDimensions: () => {
        throw new Error("disposed mid-measure");
      },
      fit: vi.fn(),
    };
    expect(fitOnce(throwing, { cols: 80, rows: 24 })).toBe(false);
    expect(fitOnce(null, { cols: 80, rows: 24 })).toBe(false);
    expect(fitOnce({ proposeDimensions: () => ({ cols: 10, rows: 10 }), fit: vi.fn() }, null)).toBe(false);
  });
});
