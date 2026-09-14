// tty.ts — the terminal-mode client helpers: the PTY WebSocket URL (on the
// same origin that served the page, whether that is 127.0.0.1 or the remote
// HTTPS address) and the tappable key-row sequences for mobile.
import { agentdWsUrl, getDeviceToken } from "./agentd";

// TtyGeom is the viewer's terminal geometry. Sending it on every attach lets
// agentd birth a fresh PTY at the real size, so claude's first output (the
// --resume replay) wraps correctly instead of at 80×24 (mangled scrollback).
export interface TtyGeom {
  cols: number;
  rows: number;
}

export function ttyUrl(runId: string, geom?: TtyGeom | null): string {
  const params = new URLSearchParams();
  const tok = getDeviceToken();
  if (tok) params.set("token", tok);
  if (geom && geom.cols >= 20 && geom.rows >= 5) {
    params.set("cols", String(Math.round(geom.cols)));
    params.set("rows", String(Math.round(geom.rows)));
  }
  return agentdWsUrl(
    `/api/v1/agentd/sessions/${encodeURIComponent(runId)}/tty/ws?${params.toString()}`,
  );
}

// --- geometry memory ----------------------------------------------------------------
// Every successful fit persists here; spawn requests made BEFORE a terminal
// exists (the /sessions resume bridge) reuse it so the PTY is born right.

const GEOM_KEY = "af-tty-geom";

export function storeLastTtyGeom(geom: TtyGeom): void {
  if (geom.cols < 20 || geom.rows < 5) return;
  try {
    localStorage.setItem(GEOM_KEY, JSON.stringify(geom));
  } catch {
    // private mode / quota — estimation fallback still works
  }
}

export function lastTtyGeom(): TtyGeom | null {
  try {
    const raw = localStorage.getItem(GEOM_KEY);
    if (!raw) return null;
    const g = JSON.parse(raw) as Partial<TtyGeom>;
    if (
      typeof g.cols === "number" &&
      typeof g.rows === "number" &&
      g.cols >= 20 &&
      g.rows >= 5
    ) {
      return { cols: Math.round(g.cols), rows: Math.round(g.rows) };
    }
  } catch {
    // corrupted entry — fall through to estimation
  }
  return null;
}

// estimateTtyGeom approximates the pane size from the viewport for the rare
// first-ever attach (no stored geometry yet). Ballpark is enough: the server
// clamps, and the first real fit resizes immediately after.
export function estimateTtyGeom(): TtyGeom {
  const w = typeof window !== "undefined" ? window.innerWidth : 390;
  const h = typeof window !== "undefined" ? window.innerHeight : 780;
  // 13px ui-monospace ≈ 8px advance, ~17px line cell; padding + header chrome
  const cols = Math.max(20, Math.min(500, Math.floor((w - 20) / 8)));
  const rows = Math.max(5, Math.min(300, Math.floor((h - 120) / 17)));
  return { cols, rows };
}

// currentTtyGeom is the best-known geometry for spawn-time requests.
//
// A remembered geometry is only useful while it still plausibly describes the
// CURRENT viewport. Reusing it unconditionally birthed the PTY at whatever was
// last stored — a desktop's geometry, the other orientation, or a stale 80x24
// left behind by a terminal that had never actually fitted. claude then drew
// its first frame (including the whole --resume replay) at that size, and
// because the pane fitting itself afterwards does not reflow an
// already-painted frame, the TUI just sat in a fraction of the screen until a
// reload. So: sanity-check the memory against this viewport's estimate and
// fall back to the estimate when it no longer fits.
export function currentTtyGeom(): TtyGeom {
  const est = estimateTtyGeom();
  const last = lastTtyGeom();
  if (!last) return est;
  const plausible =
    last.cols >= est.cols * 0.6 &&
    last.cols <= est.cols * 1.6 &&
    last.rows >= est.rows * 0.5 &&
    last.rows <= est.rows * 1.8;
  return plausible ? last : est;
}

// TTY_KEYS — the mobile key row: escape sequences and plain chars the
// TUIs expect. Order matters: the first ones render on-screen by default
// and the rest are reachable by horizontal swipe (the row has
// overflow-x-auto).
//
// Scroll keys (PgUp/PgDn) are at the FRONT so they're visible by default
// — they're the only way to scroll an OpenCode TUI from a phone, and
// without them a long chat/output session is effectively read-only. Home/
// End jump to scrollback bounds. Claude ignores these when its menu is
// active (the cursor moves), so they're safe to keep always-visible.
// A key either writes a sequence to the PTY (seq) or drives the composer
// itself (action). The plain arrows are the second kind: sending ESC[A to
// the PTY recalled the TUI's history, which is not what a person tapping ↑
// above a text box is asking for. The TUI's own arrows are still reachable,
// further along the row, as "TUI ↑" / "TUI ↓".
export type TtyKeyAction = "history-prev" | "history-next";

export interface TtyKey {
  label: string;
  seq?: string;
  action?: TtyKeyAction;
  wide?: boolean;
}

export const TTY_KEYS: TtyKey[] = [
  { label: "PgUp", seq: "\x1b[5~" },
  { label: "PgDn", seq: "\x1b[6~" },
  { label: "↑", action: "history-prev" },
  { label: "↓", action: "history-next" },
  { label: "Esc", seq: "\x1b" },
  { label: "Tab", seq: "\t" },
  { label: "←", seq: "\x1b[D" },
  { label: "→", seq: "\x1b[C" },
  { label: "Home", seq: "\x1b[H" },
  { label: "End", seq: "\x1b[F" },
  { label: "TUI ↑", seq: "\x1b[A" },
  { label: "TUI ↓", seq: "\x1b[B" },
  { label: "Enter", seq: "\r", wide: true },
  { label: "Ctrl+C", seq: "\x03" },
  { label: "Ctrl+D", seq: "\x04" },
  { label: "/", seq: "/" },
  { label: "-", seq: "-" },
  ...["1", "2", "3", "4", "5", "6", "7", "8", "9"].map((d) => ({
    label: d,
    seq: d,
  })),
];

// --- tap-to-type -------------------------------------------------------------
//
// On a phone the composer used to open only from a header icon: the hardest
// spot to reach one-handed, and the furthest from where the composer appears.
// A tap on the TUI's own prompt box opens it instead. The prompt box is
// drawn by the TUI in its bottom rows and is inert to us, so "the prompt" is
// a geometric guess: the last few rows of the grid.
export const PROMPT_TAP_ROWS = 3;
// How far ABOVE the caret still counts as the prompt box: enough for a
// widget that draws a border on the line above its input.
export const CURSOR_TAP_SLACK_ROWS = 1;
// Above this much movement the gesture was a scroll, not a tap.
export const TAP_SLOP_PX = 10;
// Above this long it was a press (selection, context menu), not a tap.
export const TAP_MAX_MS = 300;

export interface PromptTap {
  // movedPx is the total distance from touchstart to touchend.
  movedPx: number;
  durationMs: number;
  // offsetY is the tap's distance from the top of the terminal grid.
  offsetY: number;
  gridHeight: number;
  rows: number;
  // cursorRow is the TUI's own caret row in the viewport (0-based), or -1
  // when unknown. It is where the prompt box actually IS, which is not
  // always the bottom: Claude Code renders relatively so its prompt rides
  // the last rows, but OpenCode draws a full-screen layout and leaves its
  // caret several rows up (row 35 of 40 with a conversation on screen,
  // row 21 on an empty one). A bottom-rows-only test therefore missed
  // OpenCode's prompt entirely and tapping it did nothing.
  cursorRow?: number;
  // scrolled is set when the gesture drove the TUI's scroll bridge, which
  // makes it a drag whatever the endpoints say.
  scrolled: boolean;
  // keyboardEnabled is the composer's state: an open composer has nothing
  // to open.
  keyboardEnabled: boolean;
}

// isPromptTap decides whether one touch gesture was a tap on the TUI's
// prompt box. Everything it needs is measured by the caller, so the rule
// itself is testable without a terminal.
export function isPromptTap(t: PromptTap): boolean {
  if (t.keyboardEnabled || t.scrolled) return false;
  if (t.movedPx > TAP_SLOP_PX || t.durationMs > TAP_MAX_MS) return false;
  if (t.rows <= 0 || t.gridHeight <= 0) return false;
  const cell = t.gridHeight / t.rows;
  if (cell <= 0) return false;
  const row = Math.floor(t.offsetY / cell);
  if (row < 0 || row >= t.rows) return false;

  // The bottom band, which is where a relatively-rendered TUI keeps its
  // prompt.
  if (row >= t.rows - PROMPT_TAP_ROWS) return true;

  // Or at/below the TUI's own caret, which is where the prompt is for a
  // TUI that draws a full-screen layout. One row of slack above it covers
  // a box border drawn over the caret line.
  const cursor = t.cursorRow ?? -1;
  if (cursor >= 0 && cursor < t.rows && row >= cursor - CURSOR_TAP_SLACK_ROWS) {
    return true;
  }
  // Above the caret is transcript, and tapping what you are reading must
  // not throw a keyboard over it.
  return false;
}

// TAP_HINT_KEY marks that this device has been told how to open the
// composer. The hint is shown once and never again.
export const TAP_HINT_KEY = "af:hint:tap-prompt";

export function tapHintSeen(): boolean {
  try {
    return localStorage.getItem(TAP_HINT_KEY) === "1";
  } catch {
    // private mode: show it, it costs one tap
    return false;
  }
}

export function markTapHintSeen(): void {
  try {
    localStorage.setItem(TAP_HINT_KEY, "1");
  } catch {
    // nothing to remember it with — the hint reappears, which is harmless
  }
}
