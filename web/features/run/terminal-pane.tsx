"use client";

// TerminalPane — the xterm.js view onto a run's PTY. Binary ws frames are raw
// TUI output; term.onData goes back as {"type":"input"} control frames. The
// pane exposes sendInput() imperatively so the run-view composer and the
// mobile key row can inject text into the same terminal.
import {
  forwardRef,
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
} from "react";
import type { Terminal as XTerm } from "@xterm/xterm";
import type { FitAddon as FitAddonT } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Loader2, AlertCircle, RefreshCw } from "lucide-react";
import {
  isPromptTap,
  markTapHintSeen,
  storeLastTtyGeom,
  tapHintSeen,
  ttyUrl,
} from "@/lib/tty";
import { fitOnce } from "@/lib/xterm-fit";

export interface SendInputOptions {
  // focusTerminal moves focus back to xterm's hidden textarea after the
  // write. It is what makes the NEXT keystroke go to the TUI, which is
  // right for the key row on a desktop and wrong for anything the composer
  // sends: with the composer open the hidden textarea is keyboard-enabled,
  // so focusing it silently retargets the phone's keyboard at the raw PTY
  // and the composer's Enter stops being the path the text takes.
  focusTerminal?: boolean;
}

export interface TerminalHandle {
  // sendInput injects raw bytes into the PTY (key row, direct writes).
  sendInput: (data: string, opts?: SendInputOptions) => void;
  // submitInput sends one composed line as a submit frame: the backend
  // writes the text, pauses, then writes the carriage return separately, so
  // the TUI reads the CR as "submit" rather than as a newline inside a
  // pasted chunk. bracketed follows the TUI's own paste mode.
  submitInput: (text: string) => void;
  connected: () => boolean;
  // reconnect tears down the current socket and redials NOW. Use this when the
  // user explicitly asks to revive a dead session (Refresh connection) or when
  // a higher layer knows the hub has been re-registered and the in-flight
  // socket is stale.
  reconnect: () => void;
}

type ConnState = "idle" | "connecting" | "open" | "reconnecting" | "failed";

// applyTermKeyboard toggles xterm's hidden textarea between "taps pop the OS
// keyboard" (composer open / desktop) and inert (composer closed on touch).
function applyTermKeyboard(enabled: boolean, host?: HTMLElement | null) {
  // Scope the lookup to THIS pane's host. A document-wide querySelector grabs
  // whichever helper textarea happens to be first in the DOM, so with a
  // second terminal mounted anywhere (the session drawer, a loop member pane)
  // one pane's keyboard toggle silently reconfigured the other's.
  const root: ParentNode = host ?? document;
  const ta = root.querySelector<HTMLInputElement>(
    "textarea.xterm-helper-textarea",
  );
  if (!ta) return;
  if (enabled) {
    ta.removeAttribute("readonly");
    ta.setAttribute("inputmode", "text");
  } else {
    ta.setAttribute("readonly", "readonly");
    ta.setAttribute("inputmode", "none");
    ta.blur();
  }
}

// themeFromVars reads the app's CSS variables so the terminal matches the
// Agent Flow dark/light theme exactly.
function themeFromVars(): Record<string, string> {
  const styles = getComputedStyle(document.documentElement);
  const v = (name: string, fallback: string) =>
    styles.getPropertyValue(name).trim() || fallback;
  return {
    background: v("--background", "#09090b"),
    foreground: v("--foreground", "#fafafa"),
    cursor: v("--primary", "#7c3aed"),
    cursorAccent: v("--primary-foreground", "#ffffff"),
    selectionBackground: v("--accent", "#3b82f6") + "55",
    black: v("--background", "#09090b"),
    red: v("--destructive", "#ef4444"),
    green: v("--success", "#22c55e"),
    blue: v("--primary", "#7c3aed"),
    cyan: v("--primary", "#7c3aed"),
    magenta: v("--primary", "#7c3aed"),
    white: v("--muted-foreground", "#a1a1aa"),
  };
}

// ---- TTY socket lifecycle ---------------------------------------------------
//
// xterm stays mounted across reconnect cycles (scrollback survives). Only the
// WebSocket comes and goes: on close we redial with exponential backoff, the
// server replays missed frames, and the next onmessage paints them into the
// existing terminal. The pane never loses its visual state — only the wire
// connection comes and goes.
//
// manualClose is the unmount signal: set by dispose(), it suppresses the
// backoff timer so a teardown that races with a transient close doesn't
// schedule a phantom reconnect.

const INITIAL_BACKOFF_MS = 500;
const MAX_BACKOFF_MS = 15_000;
// Per-socket open timeout: the browser will eventually emit onclose on its
// own for unreachable hosts, but it can take a minute or more for the
// underlying TCP to give up. Bound each attempt so we don't sit in
// CONNECTING for an unbounded time before we can decide to give up.
const OPEN_TIMEOUT_MS = 10_000;
// A socket that opened but then closed within this window counts as a
// failed attempt. The server happily accepts the WS upgrade and then
// closes the connection a beat later when its own Subscribe fails (the
// PTY isn't registered yet — common during the run's "Starting" phase
// while the spawner is still bringing the TUI up). Without this grace,
// every such cycle would look like a clean reconnect and the give-up
// threshold would never trigger.
const OPEN_GRACE_MS = 5_000;
// Give up after this many consecutive failed attempts. With 15s between
// tries at the backoff ceiling, 4 failures is ~60s of trying — long
// enough that transient issues clear, short enough that the user isn't
// left staring at a spinner for minutes.
const GIVE_UP_AFTER = 4;
// How long the container height must hold still before we re-fit. The
// on-screen keyboard slides over ~250ms and the ResizeObserver fires on every
// frame of it; fitting per frame sent one PTY resize per intermediate height,
// and each SIGWINCH makes claude repaint its whole frame (that is what stacks
// ghost copies of the transcript). One fit at the settled size instead.
const FIT_SETTLE_MS = 120;
// Bounded budget for landing the FIRST fit before we dial the PTY, so the
// session is born at the right geometry and never needs a corrective
// SIGWINCH+repaint on open.
const FIRST_FIT_BUDGET_MS = 1500;

export const TerminalPane = forwardRef<
  TerminalHandle,
  {
    runId: string;
    keyboardEnabled?: boolean;
    // onRequestKeyboard fires when the user taps the TUI's own prompt box
    // while the composer is closed. It runs INSIDE the touchend handler, so
    // a focus() made from it still counts as a user gesture on iOS.
    onRequestKeyboard?: () => void;
  }
>(function TerminalPane(
  { runId, keyboardEnabled = true, onRequestKeyboard },
  ref,
) {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<XTerm | null>(null);
  // termRefForFocus is an alias for termRef used by sendInput: clicking
  // a key-row button steals focus from the terminal, and the TUI stops
  // processing keystrokes until focus returns. Refocusing after every
  // sendInput makes the next tap go to the TUI, not the button.
  const termRefForFocus = termRef;
  const wsRef = useRef<WebSocket | null>(null);
  const [conn, setConn] = useState<ConnState>("idle");
  const [attempt, setAttempt] = useState(0);
  const [termReady, setTermReady] = useState(false);
  // latest flag for the async open path (a fresh terminal's textarea needs
  // the attributes applied immediately, not on the next toggle)
  const kbRef = useRef(keyboardEnabled);
  kbRef.current = keyboardEnabled;
  // the WS lifecycle lives inside the effect; this ref is how the imperative
  // reconnect() (exposed via useImperativeHandle) reaches the same socket
  // machinery that the effect owns.
  const forceReconnectRef = useRef<() => void>(() => {});
  // the touch handlers live inside the [runId] effect; this ref is how
  // they reach the CURRENT callback without re-creating the terminal.
  const requestKeyboardRef = useRef<(() => void) | undefined>(undefined);
  requestKeyboardRef.current = onRequestKeyboard;
  const [showTapHint, setShowTapHint] = useState(false);
  // read by the touch handler, which lives inside the [runId] effect and
  // would otherwise close over the first value of showTapHint forever
  const hintUpRef = useRef(false);
  hintUpRef.current = showTapHint;

  useImperativeHandle(ref, () => ({
    sendInput: (data: string, opts?: SendInputOptions) => {
      const ws = wsRef.current;
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "input", data }));
      }
      if (opts?.focusTerminal === false) return;
      // The OpenCode TUI (and any ratatui/bubbletea TUI) only processes
      // keyboard input while the terminal pane is FOCUSED. After the
      // key-row button or composer submit takes focus, the next keystroke
      // the TUI sees is whatever the user typed into the new focused
      // element — usually nothing. Refocus the terminal's hidden textarea
      // so the user's next tap on the keyboard hits the PTY, not a
      // button they didn't mean to press.
      const termRef = termRefForFocus.current;
      termRef?.focus();
    },
    submitInput: (text: string) => {
      const ws = wsRef.current;
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      // A TUI that has enabled DECSET 2004 wants pasted text wrapped, and
      // a multi-line draft depends on it: without the markers the
      // embedded newlines submit the draft one line at a time.
      const bracketed =
        termRefForFocus.current?.modes.bracketedPasteMode === true;
      ws.send(JSON.stringify({ type: "submit", data: text, bracketed }));
    },
    connected: () => wsRef.current?.readyState === WebSocket.OPEN,
    reconnect: () => forceReconnectRef.current(),
  }));

  useEffect(() => {
    const hostEl = hostRef.current;
    let disposed = false;
    let resizeObs: ResizeObserver | null = null;
    let term: XTerm | null = null;
    let fit: FitAddonT | null = null;
    let onTouchStartRef: ((e: TouchEvent) => void) | null = null;
    let onTouchMoveRef: ((e: TouchEvent) => void) | null = null;
    let onTouchEndRef: ((e: TouchEvent) => void) | null = null;
    let onViewportResizeRef: (() => void) | null = null;
    let onVisibilityRef: (() => void) | null = null;
    let manualClose = false;
    let backoff = INITIAL_BACKOFF_MS;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let openTimer: ReturnType<typeof setTimeout> | null = null;
    let fitTimer: ReturnType<typeof setTimeout> | null = null;
    let consecutiveFailures = 0;
    // these refs let the imperative `reconnect()` (and a visibility health
    // check) reach the same socket machinery that the effect owns, without
    // re-creating it on every parent re-render.
    let openSocket: () => void = () => {};
    let closeSocket: () => void = () => {};

    const clearReconnectTimer = () => {
      if (reconnectTimer) {
        clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
    };

    const clearOpenTimer = () => {
      if (openTimer) {
        clearTimeout(openTimer);
        openTimer = null;
      }
    };

    (async () => {
      // xterm touches the DOM at construction — import lazily so SSR never
      // evaluates it.
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import("@xterm/xterm"),
        import("@xterm/addon-fit"),
      ]);
      if (disposed || !hostRef.current) return;

      term = new Terminal({
        convertEol: false,
        cursorBlink: true,
        scrollback: 5000,
        fontSize: 13,
        fontFamily:
          "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
        theme: themeFromVars(),
        allowProposedApi: false,
      });
      termRef.current = term;
      fit = new FitAddon();
      term.loadAddon(fit);
      term.open(hostRef.current);
      applyTermKeyboard(kbRef.current, hostRef.current);
      setTermReady(true);

      // ---- fitting ------------------------------------------------------
      // FitAddon.fit() is a SILENT no-op until xterm has measured the font:
      // proposeDimensions() returns undefined while
      // _renderService.dimensions.css.cell.height is still 0, and fit()
      // returns without throwing. Calling it synchronously right after
      // open() therefore left the terminal at its default 80x24 whenever
      // measurement had not landed yet — and because the ResizeObserver
      // below only fires when the CONTAINER changes size, nothing ever
      // corrected it. The TUI stayed the wrong height until a rotation, a
      // keyboard open, or a full reload. That is the "sometimes I have to
      // refresh before the TUI is full height" bug, and it was a race, which
      // is why it looked intermittent.
      //
      // fitNow() reports whether a fit actually landed; ensureFitted()
      // retries across frames until one does.
      const fitNow = (): boolean => fitOnce(fit, term);

      // rAF is starved while the tab is hidden (iOS Safari backgrounds it
      // aggressively), so a pure-rAF retry loop would never advance and the
      // awaited first fit below would never resolve — the terminal would sit
      // there having never dialed. Race a timer so the loop always ticks.
      const nextFrame = () =>
        new Promise<void>((resolve) => {
          let settled = false;
          const finish = () => {
            if (settled) return;
            settled = true;
            resolve();
          };
          requestAnimationFrame(finish);
          setTimeout(finish, 50);
        });

      const ensureFitted = async (budgetMs: number): Promise<boolean> => {
        // Webfont swaps change cell metrics after the first measurement, so a
        // fit taken before fonts settle is wrong even when it is non-zero.
        try {
          await document.fonts?.ready;
        } catch {
          /* no font loading API — measurement is already final */
        }
        const deadline = Date.now() + budgetMs;
        while (!disposed) {
          if (fitNow()) return true;
          if (Date.now() > deadline) return false;
          await nextFrame();
        }
        return false;
      };

      // Land the geometry BEFORE dialing: the socket URL carries birth
      // geometry, so fitting first means the PTY is created at the size the
      // screen actually is — no corrective resize, no full-frame repaint, no
      // ghost copies on open.
      if (!(await ensureFitted(FIRST_FIT_BUDGET_MS))) {
        // Non-fatal: connect anyway and let the settle-fit correct it once
        // the container reports a usable size.
        void ensureFitted(8000);
      }
      if (disposed) return;

      // resize gating (the duplicated-text fix): sentGeom mirrors what the
      // server-side PTY most recently had applied — the dial URL above
      // already carried these dims as birth geometry. Every resize frame
      // is gated on ACTUALLY changing them: each SIGWINCH makes claude's
      // renderer repaint its whole frame, and redundant repaints stack
      // ghost copies of the transcript into scrollback.
      let sentGeom: { cols: number; rows: number } | null = {
        cols: term.cols,
        rows: term.rows,
      };
      const syncResize = () => {
        const ws = wsRef.current;
        if (!term || !ws || ws.readyState !== WebSocket.OPEN) return;
        const next = { cols: term.cols, rows: term.rows };
        if (
          sentGeom &&
          sentGeom.cols === next.cols &&
          sentGeom.rows === next.rows
        ) {
          return;
        }
        sentGeom = next;
        ws.send(JSON.stringify({ type: "resize", ...next }));
      };

      // During a touch-burst the bridge coalesces everything the synthetic wheel
      // events produce into ONE input frame (onTouchMove below).
      let batchingInput = false;
      let batchedInput = "";
      term.onData((data) => {
        const ws = wsRef.current;
        if (!ws || ws.readyState !== WebSocket.OPEN) return;
        if (batchingInput) {
          batchedInput += data;
          return;
        }
        ws.send(JSON.stringify({ type: "input", data }));
      });

      // ---- socket lifecycle ----
      //
      // openSocket re-dials and wires handlers. Reconnect runs on close (unless
      // manualClose is set). forceReconnect is the imperative escape hatch for
      // the Refresh-connection control and the visibility health check.
      const scheduleReconnect = () => {
        if (manualClose || reconnectTimer) return;
        const wait = backoff;
        backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
        setConn("reconnecting");
        reconnectTimer = setTimeout(() => {
          reconnectTimer = null;
          if (manualClose) return;
          openSocket();
        }, wait);
      };

      openSocket = () => {
        if (manualClose || !term) return;
        clearReconnectTimer();
        clearOpenTimer();
        setConn("connecting");
        let socket: WebSocket;
        try {
          socket = new WebSocket(
            ttyUrl(runId, { cols: term.cols, rows: term.rows }),
          );
        } catch (err) {
          console.error("tty: construct failed", err);
          // construction errors are deterministic (bad URL, bad port) — no
          // point in retrying, the user must intervene.
          clearReconnectTimer();
          setConn("failed");
          return;
        }
        socket.binaryType = "arraybuffer";
        wsRef.current = socket;
        // tracks whether THIS socket ever opened; a close without an open
        // counts as a failed attempt for the give-up threshold.
        let opened = false;
        // wall-clock time of the last successful onopen; used by onclose to
        // distinguish a sustained session that ended normally from one
        // that the server closed immediately (the kick-on-empty-subscribe
        // pattern that previously slipped past the give-up threshold).
        let openedAt = 0;
        openTimer = setTimeout(() => {
          if (manualClose || opened) return;
          // the socket never opened within the timeout — close it; onclose
          // will then run through the scheduleReconnect + give-up logic.
          try {
            socket.close();
          } catch {
            // already dead
          }
        }, OPEN_TIMEOUT_MS);
        socket.onopen = () => {
          opened = true;
          openedAt = performance.now();
          clearOpenTimer();
          // a successful open resets the backoff so the next failure starts
          // the gentle half-second ramp again, not the 15s ceiling.
          backoff = INITIAL_BACKOFF_MS;
          consecutiveFailures = 0;
          setAttempt(0);
          setConn("open");
          // heal any fit drift since dial time — no-op frame when it matches
          syncResize();
        };
        socket.onmessage = (ev) => {
          if (typeof ev.data === "string") return;
          term?.write(new Uint8Array(ev.data));
        };
        socket.onerror = () => {
          // onclose follows; do not schedule here or we double up.
        };
        socket.onclose = () => {
          clearOpenTimer();
          if (manualClose) return;
          // a close after the socket was open for at least OPEN_GRACE_MS
          // is a healthy disconnect (server-side shutdown, navigation
          // away from the page, etc.) — reset the counter and just retry.
          // Anything else (never opened, or opened then immediately
          // closed) is a real failure that should tick toward the
          // give-up threshold.
          const sustained =
            opened && performance.now() - openedAt >= OPEN_GRACE_MS;
          if (sustained) {
            consecutiveFailures = 0;
            setAttempt(0);
            scheduleReconnect();
            return;
          }
          consecutiveFailures += 1;
          setAttempt(consecutiveFailures);
          if (consecutiveFailures >= GIVE_UP_AFTER) {
            clearReconnectTimer();
            setConn("failed");
            return;
          }
          scheduleReconnect();
        };
      };

      closeSocket = () => {
        manualClose = true;
        clearReconnectTimer();
        clearOpenTimer();
        const ws = wsRef.current;
        wsRef.current = null;
        if (ws) {
          ws.onclose = null;
          ws.onerror = null;
          try {
            ws.close();
          } catch {
            // already dead
          }
        }
      };

      const forceReconnect = () => {
        if (manualClose) return;
        clearReconnectTimer();
        clearOpenTimer();
        // an explicit user/visibility-initiated retry gets a fresh slate:
        // any give-up is undone and the failure counter resets so we start
        // the gentle ramp again instead of staying at the ceiling.
        consecutiveFailures = 0;
        setAttempt(0);
        backoff = INITIAL_BACKOFF_MS;
        const ws = wsRef.current;
        wsRef.current = null;
        if (ws) {
          ws.onclose = null;
          ws.onerror = null;
          try {
            ws.close();
          } catch {
            // already dead
          }
        }
        openSocket();
      };
      forceReconnectRef.current = forceReconnect;

      // ---- mobile touch → wheel bridge (alt buffer only) -------------------
      // Normal scrollback is fully native: touch-action: pan-y lets the
      // browser pan .xterm-viewport, which drives xterm's own wheel handler
      // and calls scrollLines() for us. We never intercept it.
      //
      // The alt buffer (TUI) is a fixed grid that doesn't scroll visually,
      // so a finger pan produces zero native wheel events and the TUI's
      // mouse-report scroll never fires. We bridge it here: a passive
      // touchmove (we do not preventDefault — the host's touch-action is
      // pan-y and the browser keeps doing its thing) reads the cell size
      // on demand, and dispatches a synthetic wheel for every whole cell
      // the finger has crossed since the last sample. No accumulator
      // floor, no fling, no quantization cap: the very first cell of
      // movement fires a wheel event, so drags feel 1:1 instead of having
      // the ~50px dead zone the old accumulator had. isTrusted guards
      // against any browser that ever re-emits a touch as a wheel.
      const WHEEL_UNIT_PX = 53; // > xterm's 50px small-delta damping floor
      const cellPxRef = () => {
        const el = term?.element;
        if (!el || !term || term.rows <= 0) return 0;
        const px = el.getBoundingClientRect().height / term.rows;
        return px >= 8 ? px : 0;
      };
      const lastTouchY = new Map<number, number>();
      let altTouchAcc = 0;
      // ---- tap-to-type -------------------------------------------------
      // A tap on the bottom rows — where the TUI draws its own prompt box —
      // opens the composer. The gesture is measured here rather than with a
      // click handler because iOS only pops the keyboard for a focus() made
      // synchronously inside a user gesture, and touchend is one.
      let tapStart: { x: number; y: number; t: number } | null = null;
      let tapScrolled = false;
      const onTouchStart = (ev: TouchEvent) => {
        if (ev.touches.length !== 1) {
          tapStart = null;
          return;
        }
        const t = ev.touches[0];
        tapStart = { x: t.clientX, y: t.clientY, t: performance.now() };
        tapScrolled = false;
      };
      const onTouchMove = (ev: TouchEvent) => {
        if (ev.isTrusted !== true) return;
        if (ev.touches.length !== 1) return; // pinch: let browser handle it
        const t = ev.touches[0];
        if (!t || !term?.element) return;
        if (term.buffer.active.type !== "alternate") return; // normal buf is native
        const px = cellPxRef();
        if (px < 8) return; // not fitted yet
        const prev = lastTouchY.get(t.identifier);
        if (prev === undefined) {
          lastTouchY.set(t.identifier, t.clientY);
          return;
        }
        const dy = prev - t.clientY;
        lastTouchY.set(t.identifier, t.clientY);
        if (dy === 0) return;
        // signed accumulator so a direction flip must pay back the
        // sub-cell remainder instead of getting a free click.
        altTouchAcc += dy / px;
        const clicks = Math.trunc(altTouchAcc);
        if (clicks === 0) return;
        tapScrolled = true; // the gesture moved the TUI: not a tap
        altTouchAcc -= clicks;
        batchingInput = true;
        batchedInput = "";
        try {
          const count = Math.abs(clicks);
          const delta = clicks > 0 ? WHEEL_UNIT_PX : -WHEEL_UNIT_PX;
          // Dispatch on the .xterm-screen child, not term.element (the .xterm
          // root). xterm registers its mouse-report wheel listener on the
          // screen element; a WheelEvent dispatched on the root never reaches a
          // listener on a child (dispatch bubbles up from the target, not
          // down), so the report — and OpenCode's scroll — never fired.
          const wheelTarget =
            term.element.querySelector(".xterm-screen") ?? term.element;
          for (let i = 0; i < count; i++) {
            wheelTarget.dispatchEvent(
              // The FINGER's position, not the origin.
              //
              // A WheelEvent built without coordinates carries clientX/Y = 0,
              // and xterm does not reject that — it CLAMPS it into the grid
              // and emits a mouse report at cell 1;1. Measured against
              // OpenCode 1.18.29 under a real PTY: a wheel report at 1;1
              // produces zero response, while the identical report at 10;10
              // or 50;15 scrolls the transcript. So every finger-scroll was
              // being delivered to the one corner the TUI ignores.
              //
              // Claude never showed it because it does not use the alt
              // screen, so this bridge is skipped for it entirely and its
              // scrolling is the browser's own.
              new WheelEvent("wheel", {
                deltaY: delta,
                cancelable: true,
                clientX: t.clientX,
                clientY: t.clientY,
                screenX: t.screenX,
                screenY: t.screenY,
              }),
            );
          }
        } finally {
          batchingInput = false;
        }
        const ws = wsRef.current;
        if (batchedInput && ws && ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: "input", data: batchedInput }));
        }
      };
      const onTouchEnd = (ev: TouchEvent) => {
        for (const t of ev.changedTouches) lastTouchY.delete(t.identifier);
        const start = tapStart;
        tapStart = null;
        const request = requestKeyboardRef.current;
        if (!start || !request || ev.type !== "touchend") return;
        const t = ev.changedTouches[0];
        if (!t || ev.touches.length > 0) return;
        const grid = term?.element?.getBoundingClientRect();
        if (!term || !grid) return;
        const dx = t.clientX - start.x;
        const dy = t.clientY - start.y;
        const gesture = {
          movedPx: Math.hypot(dx, dy),
          durationMs: performance.now() - start.t,
          offsetY: t.clientY - grid.top,
          gridHeight: grid.height,
          rows: term.rows,
          // Where the TUI's own caret sits, which is where its prompt box
          // is. xterm gives it in viewport coordinates, the same space the
          // tap is measured in.
          cursorRow: term.buffer.active.cursorY,
          scrolled: tapScrolled,
          keyboardEnabled: kbRef.current,
        };
        // the hint goes on the FIRST tap anywhere, whether or not it landed
        // on the prompt: it has been read by then.
        if (hintUpRef.current) {
          markTapHintSeen();
          setShowTapHint(false);
        }
        if (!isPromptTap(gesture)) return;
        // synchronous, inside the gesture: iOS opens the keyboard only for
        // a focus() made here, never for one made from a later effect.
        request();
      };

      // keep the terminal fitted to its container (pane swaps, rotations,
      // mobile keyboard opening/closing via visualViewport)
      // Debounced so a keyboard slide or a rotation produces ONE resize
      // frame at the settled height instead of one per animation frame.
      const doFit = () => {
        if (fitTimer) clearTimeout(fitTimer);
        fitTimer = setTimeout(() => {
          fitTimer = null;
          if (disposed) return;
          if (!fitNow()) {
            // Metrics still unavailable (container mid-collapse, fonts
            // swapping). Keep retrying in the background rather than
            // leaving the terminal stuck at the wrong size.
            void ensureFitted(4000).then((ok) => {
              if (!ok || disposed || !term) return;
              storeLastTtyGeom({ cols: term.cols, rows: term.rows });
              syncResize();
            });
            return;
          }
          if (term) {
            // remember the real geometry: spawn-time requests made before
            // any terminal exists (the /sessions resume bridge) reuse it
            storeLastTtyGeom({ cols: term.cols, rows: term.rows });
          }
          syncResize();
        }, FIT_SETTLE_MS);
      };
      resizeObs = new ResizeObserver(doFit);
      resizeObs.observe(hostRef.current);
      onViewportResizeRef = doFit;
      window.visualViewport?.addEventListener("resize", doFit);
      // visualViewport does not fire on every Android browser, and neither
      // viewport event covers a desktop window resize or an orientation
      // change that keeps the visual viewport identical.
      window.addEventListener("resize", doFit);
      window.addEventListener("orientationchange", doFit);

      const host = hostRef.current;
      onTouchStartRef = onTouchStart;
      onTouchMoveRef = onTouchMove;
      onTouchEndRef = onTouchEnd;
      host.addEventListener("touchstart", onTouchStart, { passive: true });
      host.addEventListener("touchmove", onTouchMove, { passive: true });
      host.addEventListener("touchend", onTouchEnd, { passive: true });
      host.addEventListener("touchcancel", onTouchEnd, { passive: true });

      // kick off the first connection now that everything is wired
      openSocket();

      // visibility health check: iOS Safari freezes JS timers and silently
      // kills sockets in the background. On resume we cannot trust the
      // reported status — the close event may never arrive — so force a
      // redial when we come back from a long hide.
      const onVisibility = () => {
        if (disposed || manualClose) return;
        if (document.visibilityState !== "visible") return;
        const ws = wsRef.current;
        if (!ws || ws.readyState === WebSocket.CLOSED) forceReconnect();
      };
      document.addEventListener("visibilitychange", onVisibility);
      onVisibilityRef = onVisibility;
    })();

    return () => {
      disposed = true;
      manualClose = true;
      clearReconnectTimer();
      if (hostEl && onTouchStartRef) {
        hostEl.removeEventListener("touchstart", onTouchStartRef);
      }
      if (hostEl && onTouchMoveRef) {
        hostEl.removeEventListener("touchmove", onTouchMoveRef);
      }
      if (hostEl && onTouchEndRef) {
        hostEl.removeEventListener("touchend", onTouchEndRef);
        hostEl.removeEventListener("touchcancel", onTouchEndRef);
      }
      if (fitTimer) {
        clearTimeout(fitTimer);
        fitTimer = null;
      }
      if (onViewportResizeRef) {
        window.visualViewport?.removeEventListener(
          "resize",
          onViewportResizeRef,
        );
        window.removeEventListener("resize", onViewportResizeRef);
        window.removeEventListener("orientationchange", onViewportResizeRef);
      }
      if (onVisibilityRef) {
        document.removeEventListener("visibilitychange", onVisibilityRef);
      }
      resizeObs?.disconnect();
      closeSocket();
      termRef.current?.dispose();
      termRef.current = null;
    };
  }, [runId]);

  // Keyboard suppression: xterm focuses a hidden textarea on every tap, which
  // pops the phone's OS keyboard over the TUI — terrible while reading or
  // scrolling. While the composer is closed the textarea is made readonly
  // with inputMode none (focusable, but no keyboard); direct typing returns
  // when the caller re-enables it.
  useEffect(() => {
    if (termReady) applyTermKeyboard(keyboardEnabled, hostRef.current);
  }, [keyboardEnabled, termReady]);

  // One-time hint: tapping the TUI's prompt box is the way in, and nothing
  // on screen says so. Shown once per device, on the first run where the
  // composer is closed and a tap would do something.
  useEffect(() => {
    if (!termReady || keyboardEnabled || !onRequestKeyboard) return;
    if (tapHintSeen()) return;
    setShowTapHint(true);
  }, [termReady, keyboardEnabled, onRequestKeyboard]);

  return (
    <div className="relative min-h-0 flex-1">
      <div
        ref={hostRef}
        className="h-full w-full overflow-hidden overscroll-contain touch-action-[pan-y] px-2 pt-2"
      />
      {/* Tap hint — an overlay, never a row: a reserved strip would shrink
          the terminal and force the refit the birth-geometry work exists to
          avoid. pointer-events-none so the tap it describes goes straight
          through to the terminal, which is also what dismisses it. */}
      {showTapHint && conn === "open" && (
        <div
          className="pointer-events-none absolute inset-x-0 bottom-3 z-10 flex justify-center"
          aria-hidden
        >
          <span className="bg-foreground/85 text-background rounded-full px-3 py-1.5 text-[11px] font-medium shadow-lg">
            Tap the prompt to type
          </span>
        </div>
      )}
      {conn !== "open" && (
        <div className="absolute inset-0 flex items-center justify-center gap-2 bg-background/60 text-xs">
          {conn === "failed" ? (
            <div className="text-muted-foreground flex flex-col items-center gap-3">
              <div className="flex items-center gap-2">
                <AlertCircle className="size-4" />
                couldn’t reach the terminal
              </div>
              <button
                type="button"
                onClick={() => forceReconnectRef.current()}
                className="bg-primary text-primary-foreground hover:brightness-110 inline-flex items-center gap-1.5 rounded-full px-3 py-1.5 text-xs font-medium transition-all active:scale-95"
              >
                <RefreshCw className="size-3.5" />
                Retry
              </button>
            </div>
          ) : (
            <div className="text-muted-foreground pointer-events-none flex flex-col items-center gap-1">
              <div className="flex items-center gap-2">
                <Loader2 className="size-4 animate-spin" />
                {conn === "reconnecting"
                  ? "reconnecting…"
                  : "connecting to terminal…"}
              </div>
              {conn === "reconnecting" && attempt > 0 && (
                <span className="text-muted-foreground/70 text-[10px]">
                  attempt {attempt}/{GIVE_UP_AFTER}
                </span>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
});
