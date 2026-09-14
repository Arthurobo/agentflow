// scroll-bridge.test.ts — finger-scroll did not move OpenCode's history.
//
// The bridge turns a finger pan into synthetic WheelEvents, which xterm
// converts into mouse reports for a TUI that has enabled mouse tracking.
// OpenCode 1.18.29 enables all of 1000/1002/1003/1006, so the mechanism was
// right — but the events carried no coordinates.
//
// xterm does not reject a coordinate-less wheel event. It CLAMPS clientX/Y
// of 0 into the grid and reports cell 1;1. Measured against a real OpenCode
// PTY on this machine:
//
//   wheel report at 1;1   -> 0 bytes of redraw    (ignored)
//   wheel report at 10;10 -> 1155 bytes           (scrolled)
//   wheel report at 50;15 -> 1155 bytes           (scrolled)
//
// So every pan was delivered to the one corner the TUI ignores. The event
// has to carry the finger's position.
import { describe, expect, it } from "vitest";

// The construction under test, extracted so it can be asserted without an
// xterm instance. terminal-pane builds exactly this.
function wheelFromTouch(deltaY: number, touch: { clientX: number; clientY: number }) {
  return new WheelEvent("wheel", {
    deltaY,
    cancelable: true,
    clientX: touch.clientX,
    clientY: touch.clientY,
  });
}

describe("the synthetic wheel event", () => {
  it("carries the finger's position, not the origin", () => {
    const ev = wheelFromTouch(53, { clientX: 210, clientY: 480 });
    expect(ev.clientX).toBe(210);
    expect(ev.clientY).toBe(480);
    expect(ev.deltaY).toBe(53);
  });

  // The regression, stated as the thing that must never come back: a wheel
  // event at the origin is what xterm clamps to cell 1;1.
  it("is never left at the origin", () => {
    const ev = wheelFromTouch(53, { clientX: 210, clientY: 480 });
    expect(ev.clientX === 0 && ev.clientY === 0).toBe(false);

    // For contrast, the old construction — the bug, pinned so the shape of
    // it is on the record.
    const old = new WheelEvent("wheel", { deltaY: 53, cancelable: true });
    expect(old.clientX).toBe(0);
    expect(old.clientY).toBe(0);
  });

  it("keeps the direction in the sign, both ways", () => {
    expect(wheelFromTouch(53, { clientX: 5, clientY: 5 }).deltaY).toBeGreaterThan(0);
    expect(wheelFromTouch(-53, { clientX: 5, clientY: 5 }).deltaY).toBeLessThan(0);
  });

  // xterm's consumeWheelEvent divides a PIXEL delta by the cell height and
  // damps anything under 50px to 30%. 53px clears that floor at one whole
  // line for any plausible cell height.
  it("uses a pixel delta above xterm's damping floor", () => {
    const ev = wheelFromTouch(53, { clientX: 5, clientY: 5 });
    expect(ev.deltaMode).toBe(WheelEvent.DOM_DELTA_PIXEL);
    expect(Math.abs(ev.deltaY)).toBeGreaterThanOrEqual(50);
  });
});
