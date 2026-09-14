// selection.test.ts — one idiom, and the guarantee that every screen uses it.
import { readFileSync } from "fs";
import { join } from "path";
import { describe, expect, it } from "vitest";
import { selectionClass, SELECTED_RING } from "./selection";

describe("selectionClass", () => {
  it("is the shared idiom in both states", () => {
    expect(selectionClass(true)).toBe("af-selected");
    expect(selectionClass(false)).toBe("af-unselected");
    expect(SELECTED_RING).toBe("af-selected");
  });
});

// There were SEVEN idioms: a tinted fill, a tinted fill plus border, a ring, a
// background swap plus shadow, an opacity bar, a text-size change, and a pill.
// Nothing transferred between screens, so there was no signal to learn.
describe("every selectable surface uses it", () => {
  const read = (p: string) => readFileSync(join(__dirname, "..", p), "utf8");

  // Either the class or the helper that returns it: what matters is that no
  // screen rolls its own.
  it.each([
    ["the play card", "features/loop/play-card.tsx"],
    ["the engine picker", "features/run/run-form.tsx"],
    ["the chip toggle", "components/ui/surface.tsx"],
    ["the sidebar nav", "components/layout/sidebar.tsx"],
  ])("%s", (_name, path) => {
    const src = read(path);
    expect(src.includes("af-selected") || src.includes("selectionClass")).toBe(true);
  });

  // The old idioms specifically: a 10% fill never reads at any lightness, and
  // a background swap is invisible in light where the swap is white on white.
  it.each([
    ["the engine picker", "features/run/run-form.tsx"],
    ["the sidebar nav", "components/layout/sidebar.tsx"],
  ])("%s no longer swaps a background instead", (_name, path) => {
    const src = read(path);
    expect(src).not.toContain('? "bg-accent text-foreground');
    expect(src).not.toContain('? "bg-sidebar-accent text-sidebar-accent-foreground shadow-');
  });

  // A focus ring at half opacity cannot reach the contrast a focus indicator
  // exists to have.
  it("stops halving the ring", () => {
    for (const path of ["components/ui/button.tsx"]) {
      expect(read(path)).not.toMatch(/ring-ring\/(50|40|15)\b/);
    }
  });
});
