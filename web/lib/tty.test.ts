import { describe, expect, it, beforeEach } from "vitest";
import {
  ttyUrl,
  storeLastTtyGeom,
  lastTtyGeom,
  estimateTtyGeom,
  currentTtyGeom,
  isPromptTap,
  markTapHintSeen,
  tapHintSeen,
  TTY_KEYS,
} from "./tty";

// The birth-geometry contract: spawn-time requests carry the viewer's real
// size so agentd births PTYs at it (no more 80×24 mangled resume replays).
describe("tty geometry", () => {
  beforeEach(() => localStorage.clear());

  it("ttyUrl appends clamped geometry when valid", () => {
    const url = ttyUrl("run-1", { cols: 52, rows: 30 });
    expect(url).toContain("cols=52");
    expect(url).toContain("rows=30");
    expect(url).toContain("/api/v1/agentd/sessions/run-1/tty/ws");
  });

  it("ttyUrl omits nonsense geometry", () => {
    expect(ttyUrl("run-1", { cols: 5, rows: 30 })).not.toContain("cols=");
    expect(ttyUrl("run-1", null)).not.toContain("cols=");
  });

  it("stores and returns the last fitted geometry", () => {
    expect(lastTtyGeom()).toBeNull();
    storeLastTtyGeom({ cols: 44, rows: 82 });
    expect(lastTtyGeom()).toEqual({ cols: 44, rows: 82 });
    // degenerate fits never poison the cache
    storeLastTtyGeom({ cols: 1, rows: 1 });
    expect(lastTtyGeom()).toEqual({ cols: 44, rows: 82 });
  });

  it("estimateTtyGeom stays within server clamps", () => {
    const g = estimateTtyGeom();
    expect(g.cols).toBeGreaterThanOrEqual(20);
    expect(g.cols).toBeLessThanOrEqual(500);
    expect(g.rows).toBeGreaterThanOrEqual(5);
    expect(g.rows).toBeLessThanOrEqual(300);
  });

  // --- spawn-time geometry sanity (the "TUI not full height" bug) ----------
  //
  // The PTY's birth geometry decides how claude paints its FIRST frame,
  // including the whole --resume replay. Fitting the pane afterwards does not
  // reflow an already-painted frame, so a wrong birth size shows up as a TUI
  // stuck in a fraction of the screen until a manual reload.

  it("currentTtyGeom uses the viewport estimate when nothing is remembered", () => {
    expect(currentTtyGeom()).toEqual(estimateTtyGeom());
  });

  it("currentTtyGeom reuses a remembered geometry that still fits", () => {
    const est = estimateTtyGeom();
    const close = { cols: est.cols, rows: est.rows - 1 };
    storeLastTtyGeom(close);
    expect(currentTtyGeom()).toEqual(close);
  });

  it("currentTtyGeom discards a stale 80x24 on a narrow phone viewport", () => {
    // 80x24 is a *valid* stored value, so the old code happily birthed phone
    // PTYs at it — this is the exact regression.
    storeLastTtyGeom({ cols: 80, rows: 24 });
    const est = estimateTtyGeom();
    const got = currentTtyGeom();
    if (80 > est.cols * 1.6 || 24 < est.rows * 0.5) {
      expect(got).toEqual(est);
    } else {
      // viewport genuinely close to 80x24 — reuse is correct
      expect(got).toEqual({ cols: 80, rows: 24 });
    }
  });

  it("currentTtyGeom discards a desktop geometry remembered on a phone", () => {
    storeLastTtyGeom({ cols: 240, rows: 70 });
    const est = estimateTtyGeom();
    expect(est.cols).toBeLessThan(240 * 0.6); // jsdom viewport is narrow
    expect(currentTtyGeom()).toEqual(est);
  });

  it("currentTtyGeom never returns a geometry ttyUrl would reject", () => {
    for (const g of [null, { cols: 80, rows: 24 }, { cols: 240, rows: 70 }]) {
      localStorage.clear();
      if (g) storeLastTtyGeom(g);
      const cur = currentTtyGeom();
      expect(cur.cols).toBeGreaterThanOrEqual(20);
      expect(cur.rows).toBeGreaterThanOrEqual(5);
      expect(ttyUrl("run-1", cur)).toContain("cols=");
    }
  });
});

// --- the mobile key row -------------------------------------------------
//
// The arrows above a text box read as "the last thing I typed". They used to
// send ESC[A to the PTY, which recalls the TUI's history instead — a
// different list, in a different place, that the user cannot see from the
// composer. They drive the composer now, and the TUI's own arrows keep a
// place on the row.
describe("the key row", () => {
  const byLabel = (label: string) => TTY_KEYS.find((k) => k.label === label);

  it("gives the plain arrows to the composer's history", () => {
    expect(byLabel("↑")?.action).toBe("history-prev");
    expect(byLabel("↑")?.seq).toBeUndefined();
    expect(byLabel("↓")?.action).toBe("history-next");
    expect(byLabel("↓")?.seq).toBeUndefined();
  });

  it("keeps the TUI's own arrows reachable for menus and select lists", () => {
    expect(byLabel("TUI ↑")?.seq).toBe("\x1b[A");
    expect(byLabel("TUI ↓")?.seq).toBe("\x1b[B");
    expect(byLabel("TUI ↑")?.action).toBeUndefined();
  });

  it("leaves every other key writing bytes, and none ambiguous", () => {
    for (const k of TTY_KEYS) {
      expect(Boolean(k.seq) !== Boolean(k.action)).toBe(true);
    }
    expect(byLabel("Enter")?.seq).toBe("\r");
    expect(byLabel("Ctrl+C")?.seq).toBe("\x03");
  });
});

// --- tap to type --------------------------------------------------------
//
// The composer opens from a tap on the TUI's own prompt box: the bottom rows
// of the grid. Everything else the finger does on the terminal must stay the
// terminal's.
describe("the prompt tap", () => {
  const base = {
    movedPx: 2,
    durationMs: 90,
    offsetY: 0,
    gridHeight: 600, // 40 rows of 15px
    rows: 40,
    // -1 is "unknown caret": the bottom-band rule alone.
    cursorRow: -1,
    scrolled: false,
    keyboardEnabled: false,
  };
  const atRow = (row: number) => ({ ...base, offsetY: row * 15 + 7 });

  it("opens from a tap in the bottom rows", () => {
    expect(isPromptTap(atRow(39))).toBe(true);
    expect(isPromptTap(atRow(37))).toBe(true);
  });

  it("ignores a tap anywhere above them", () => {
    expect(isPromptTap(atRow(36))).toBe(false);
    expect(isPromptTap(atRow(0))).toBe(false);
    expect(isPromptTap(atRow(20))).toBe(false);
  });

  it("ignores a drag, however it ends", () => {
    expect(isPromptTap({ ...atRow(39), movedPx: 40 })).toBe(false);
    // a finger that scrolled the TUI and came back to where it started is
    // still a scroll
    expect(isPromptTap({ ...atRow(39), movedPx: 1, scrolled: true })).toBe(
      false,
    );
  });

  it("ignores a long press, which is a selection gesture", () => {
    expect(isPromptTap({ ...atRow(39), durationMs: 800 })).toBe(false);
  });

  it("does nothing when the composer is already open", () => {
    expect(isPromptTap({ ...atRow(39), keyboardEnabled: true })).toBe(false);
  });

  it("stays silent on a terminal that has not been measured yet", () => {
    expect(isPromptTap({ ...atRow(39), rows: 0 })).toBe(false);
    expect(isPromptTap({ ...atRow(39), gridHeight: 0 })).toBe(false);
    // a tap below the grid (safe-area padding) is not a prompt tap
    expect(isPromptTap({ ...base, offsetY: 900 })).toBe(false);
  });
});

describe("the tap hint", () => {
  beforeEach(() => localStorage.clear());

  it("is shown once and then remembered", () => {
    expect(tapHintSeen()).toBe(false);
    markTapHintSeen();
    expect(tapHintSeen()).toBe(true);
  });
});

// A TUI does not have to keep its prompt at the bottom of the screen.
//
// Claude Code renders relatively, so its prompt rides the last rows and the
// bottom band finds it. OpenCode draws a full-screen layout and leaves its
// caret several rows up — measured on this machine at row 35 of 40 with a
// conversation on screen, row 21 on an empty one. Tapping its visible prompt
// did nothing, which is what "cannot open the keyboard" was.
describe("the prompt tap follows the TUI's caret", () => {
  const at = (row: number, cursorRow: number) => ({
    movedPx: 2,
    durationMs: 90,
    offsetY: row * 15 + 7,
    gridHeight: 600,
    rows: 40,
    cursorRow,
    scrolled: false,
    keyboardEnabled: false,
  });

  it("opens from a tap on a mid-screen prompt box", () => {
    // OpenCode's measured layout: caret at row 35 (0-based 34).
    expect(isPromptTap(at(34, 34))).toBe(true);
    expect(isPromptTap(at(36, 34))).toBe(true);
    // one row of slack above, for a widget that draws its border there
    expect(isPromptTap(at(33, 34))).toBe(true);
  });

  it("still ignores the transcript above it", () => {
    expect(isPromptTap(at(32, 34))).toBe(false);
    expect(isPromptTap(at(10, 34))).toBe(false);
    expect(isPromptTap(at(0, 34))).toBe(false);
  });

  it("keeps working for a caret near the top, on an empty screen", () => {
    // OpenCode with nothing in the transcript: caret at row 21 (0-based 20).
    expect(isPromptTap(at(20, 20))).toBe(true);
    expect(isPromptTap(at(18, 20))).toBe(false);
  });

  it("keeps the bottom band when the caret is unknown or bottom-anchored", () => {
    // Claude: caret already at the bottom, so both rules agree.
    expect(isPromptTap(at(39, 39))).toBe(true);
    expect(isPromptTap(at(38, -1))).toBe(true);
    expect(isPromptTap(at(20, -1))).toBe(false);
  });

  it("does not let a caret outside the grid widen the zone", () => {
    expect(isPromptTap(at(5, 999))).toBe(false);
    expect(isPromptTap(at(5, -5))).toBe(false);
  });

  // Everything the gesture rules refuse, they still refuse.
  it("is still not a tap when it was a drag, a long press, or the composer is open", () => {
    expect(isPromptTap({ ...at(34, 34), movedPx: 40 })).toBe(false);
    expect(isPromptTap({ ...at(34, 34), scrolled: true })).toBe(false);
    expect(isPromptTap({ ...at(34, 34), durationMs: 800 })).toBe(false);
    expect(isPromptTap({ ...at(34, 34), keyboardEnabled: true })).toBe(false);
  });
});
