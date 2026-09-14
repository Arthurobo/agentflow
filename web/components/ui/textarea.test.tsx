// textarea.test.tsx — a brief is a paragraph, and on a 390px screen a 400
// character one is roughly a dozen lines. In a three-row box that is a
// peephole, and editing prose you cannot see is how a sentence gets
// duplicated.
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Textarea } from "./textarea";

// jsdom reports 0 for every layout measurement, so scrollHeight is stubbed to
// a real-looking value. Without saying that out loud a passing assertion here
// would mean nothing.
function withScrollHeight(px: number) {
  const spy = vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockReturnValue(px);
  return () => spy.mockRestore();
}

describe("Textarea", () => {
  it("grows to its content when asked to", () => {
    const restore = withScrollHeight(260);
    render(<Textarea autoResize aria-label="brief" defaultValue={"x".repeat(400)} />);
    const el = screen.getByLabelText("brief");
    expect(el.style.height).toBe("260px");
    expect(el.style.overflowY).toBe("hidden");
    restore();
  });

  it("stops growing at the cap and scrolls instead, so Save stays on screen", () => {
    const restore = withScrollHeight(5000);
    render(<Textarea autoResize maxRows={4} aria-label="brief" defaultValue="long" />);
    const el = screen.getByLabelText("brief");
    expect(parseFloat(el.style.height)).toBeLessThan(5000);
    expect(el.style.overflowY).toBe("auto");
    restore();
  });

  // Everywhere it is used today passes a fixed rows prop, and those must not
  // start resizing themselves.
  it("leaves a plain textarea alone", () => {
    const restore = withScrollHeight(260);
    render(<Textarea rows={3} aria-label="plain" defaultValue="hello" />);
    expect(screen.getByLabelText("plain").style.height).toBe("");
    restore();
  });

  it("still reports what the engineer types", async () => {
    const onChange = vi.fn();
    const restore = withScrollHeight(80);
    render(<Textarea autoResize aria-label="brief" value="a" onChange={onChange} />);
    const el = screen.getByLabelText("brief") as HTMLTextAreaElement;
    el.focus();
    // fire a native input event the way a keystroke does
    const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
    setter.call(el, "ab");
    el.dispatchEvent(new Event("input", { bubbles: true }));
    expect(onChange).toHaveBeenCalled();
    restore();
  });
});
