// globals.test.ts — the token ramp, asserted against the stylesheet itself.
//
// This round changes how everything looks and a visual regression is easy to
// miss, so the values that carry the hierarchy are pinned here. These are
// LIGHTNESS assertions, not contrast ratios: WCAG contrast is a text metric
// and is close to meaningless between two large adjacent fills, where a
// healthy card-on-page pair measures about 1.14:1. Chasing a ratio on a panel
// fill would have wasted the round.
import { readFileSync } from "fs";
import { join } from "path";
import { describe, expect, it } from "vitest";

const css = readFileSync(join(__dirname, "globals.css"), "utf8");

function block(selector: string): string {
  const start = css.indexOf(selector + " {");
  if (start < 0) throw new Error(`no ${selector} block`);
  return css.slice(start, css.indexOf("\n}", start));
}
const light = block(":root");
const dark = block(".dark");

/** lightness reads the L of an oklch token, which is the only number that
 *  matters for surface hierarchy. */
function lightness(scope: string, token: string): number {
  const m = scope.match(new RegExp(`--${token}:\\s*oklch\\(([0-9.]+)`));
  if (!m) throw new Error(`no --${token} in scope`);
  return parseFloat(m[1]);
}

describe("dark is a page, not a black hole", () => {
  // af-panel builds depth with a shadow and a one-pixel inset highlight. Cast
  // onto near-black, the shadow contributes nothing and the highlight is the
  // only cue that survives. Moving the page up is what makes them work.
  it("keeps the background off black", () => {
    expect(lightness(dark, "background")).toBeGreaterThanOrEqual(0.15);
  });

  it("separates page, card and raised by a visible step", () => {
    const bg = lightness(dark, "background");
    const card = lightness(dark, "card");
    const raised = lightness(dark, "surface-raised");
    expect(card - bg).toBeGreaterThanOrEqual(0.06);
    expect(raised - card).toBeGreaterThanOrEqual(0.05);
  });

  it("puts the sunken surface below the page and the rail below it too", () => {
    expect(lightness(dark, "surface-sunken")).toBeLessThan(lightness(dark, "background"));
    // The rail used to sit 0.020 from the page and was effectively invisible.
    expect(lightness(dark, "background") - lightness(dark, "sidebar")).toBeGreaterThanOrEqual(0.025);
  });

  // In dark the border does the structural work, so it has to be present.
  it("gives the border enough presence to be structural", () => {
    const m = dark.match(/--border:\s*oklch\(1 0 0 \/ (\d+)%\)/);
    expect(m).not.toBeNull();
    expect(Number(m![1])).toBeGreaterThanOrEqual(20);
  });
});

describe("light was the worse theme", () => {
  // Its surfaces were three times flatter than dark: dL 0.022 against 0.065.
  it("separates page from card by a real step", () => {
    expect(lightness(light, "card") - lightness(light, "background")).toBeGreaterThanOrEqual(0.04);
  });

  it("drops the page off pure white so a white card can sit on it", () => {
    expect(lightness(light, "background")).toBeLessThanOrEqual(0.96);
    expect(lightness(light, "card")).toBeGreaterThanOrEqual(0.99);
  });

  // A border on white cannot reach 3:1 without looking like a table grid, so
  // light carries hierarchy with the fill and a shadow. The border is still
  // darkened from the hairline it was.
  it("darkens the hairline border", () => {
    expect(lightness(light, "border")).toBeLessThanOrEqual(0.78);
  });

  // These are TEXT and line colours, where WCAG does apply, and every one was
  // under 3.8:1 on a light card.
  it("darkens the colours used as text", () => {
    expect(lightness(light, "primary")).toBeLessThanOrEqual(0.51);
    expect(lightness(light, "warning")).toBeLessThanOrEqual(0.56);
    expect(lightness(light, "success")).toBeLessThanOrEqual(0.53);
    expect(lightness(light, "ring")).toBeLessThanOrEqual(0.56);
  });

  it("leaves the dark equivalents alone, which were already 8 to 10:1", () => {
    expect(lightness(dark, "primary")).toBeGreaterThan(0.7);
    expect(lightness(dark, "warning")).toBeGreaterThan(0.7);
    expect(lightness(dark, "success")).toBeGreaterThan(0.7);
  });
});

describe("the two themes get different depth mechanisms", () => {
  it("defines a panel shadow per theme rather than one rule for both", () => {
    expect(light).toContain("--panel-shadow:");
    expect(dark).toContain("--panel-shadow:");
    // The dark shadow is black on a dark ground; the light one is drawn from
    // the foreground so it reads on white.
    expect(dark).toMatch(/--panel-shadow:[^;]*black/);
    expect(light).toMatch(/--panel-shadow:[^;]*foreground/);
  });

  it("uses those tokens in af-panel rather than a hardcoded shadow", () => {
    const panel = css.slice(css.indexOf(".af-panel"), css.indexOf(".af-subtle-panel"));
    expect(panel).toContain("var(--panel-shadow)");
    expect(panel).toContain("var(--panel-inset)");
    expect(panel).not.toMatch(/box-shadow:[^;]*black 30%/);
  });
});

describe("identity colours are tokens", () => {
  // Three of these went out with the chat: the card-kind colours and the
  // transcript's terminal surface had no other reader. What is left is what
  // something on screen still uses.
  it("names a colour per identity, in both themes", () => {
    for (const token of ["kind-engineer"]) {
      expect(light).toContain(`--${token}:`);
      expect(dark).toContain(`--${token}:`);
    }
  });

  // A token nothing reads is a token that drifts. These were removed with
  // their only reader and must not come back without one.
  it("keeps no colour that nothing on screen reads", () => {
    // engine-claude / engine-opencode joined them when the engine badge was
    // deleted: the engine is not an identity the UI colours any more.
    for (const token of [
      "kind-report",
      "kind-note",
      "terminal-surface",
      "engine-claude",
      "engine-opencode",
    ]) {
      expect(light).not.toContain(`--${token}:`);
      expect(dark).not.toContain(`--${token}:`);
    }
  });
});

describe("one selection idiom", () => {
  it("defines it once, as a full-opacity ring rather than a tint", () => {
    const sel = css.slice(css.indexOf(".af-selected"), css.indexOf(".af-label"));
    expect(sel).toContain("box-shadow: 0 0 0 2px var(--primary)");
    expect(sel).toContain("border-color: var(--primary)");
  });
});
