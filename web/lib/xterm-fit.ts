// xterm-fit — verified fitting for xterm.js.
//
// Why this exists: FitAddon.fit() is a SILENT no-op in the one case that
// matters most, the first fit after Terminal.open(). Internally fit() calls
// proposeDimensions(), which returns `undefined` while the renderer has not
// measured the font yet (`_renderService.dimensions.css.cell.height === 0`),
// and then returns without resizing and without throwing. A caller that does
//
//     try { fit.fit() } catch { /* ResizeObserver will fix it later */ }
//
// therefore cannot tell "fitted" from "did nothing" — and a ResizeObserver
// only fires when the CONTAINER changes size, which on a freshly opened
// session it never does. The terminal stays at its default 80x24: the TUI
// renders into a fraction of the pane until the user rotates the phone, opens
// the keyboard, or reloads the page. Because it is a race with font
// measurement, it presents as "sometimes I have to refresh".
//
// fitOnce() reports whether a fit actually landed, so callers can retry.

export interface FitLike {
  proposeDimensions(): { cols: number; rows: number } | undefined;
  fit(): void;
}

export interface TermLike {
  readonly cols: number;
  readonly rows: number;
}

/**
 * Attempt one fit. Returns true only when the terminal's geometry is known to
 * match its container afterwards; false means the measurement was not
 * available and the caller must try again on a later frame.
 */
export function fitOnce(fit: FitLike | null | undefined, term: TermLike | null | undefined): boolean {
  if (!fit || !term) return false;
  let dims: { cols: number; rows: number } | undefined;
  try {
    dims = fit.proposeDimensions();
  } catch {
    return false;
  }
  // undefined => cell metrics not measured yet (fit() would be a no-op)
  if (!dims) return false;
  if (!Number.isFinite(dims.cols) || !Number.isFinite(dims.rows)) return false;
  // xterm's own floor; anything smaller means a collapsed container
  if (dims.cols < 2 || dims.rows < 1) return false;
  if (term.cols !== dims.cols || term.rows !== dims.rows) {
    try {
      fit.fit();
    } catch {
      return false;
    }
  }
  return true;
}
