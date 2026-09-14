import * as React from "react";
import { cn } from "@/lib/utils";

/**
 * autoResize grows the box to its content instead of scrolling inside a fixed
 * rows count. A brief is a paragraph, and on a 390px screen a 400 character
 * one is roughly a dozen lines: in a three-row box that is a peephole, and
 * editing prose you cannot see is how a sentence gets duplicated.
 *
 * maxRows caps the growth so a very long rule still leaves the Save button on
 * screen; past that it scrolls, which is the right trade at that length.
 */
function Textarea({
  className,
  autoResize,
  maxRows = 18,
  onChange,
  value,
  ...props
}: React.ComponentProps<"textarea"> & { autoResize?: boolean; maxRows?: number }) {
  const ref = React.useRef<HTMLTextAreaElement>(null);

  const fit = React.useCallback(() => {
    const el = ref.current;
    if (!el || !autoResize) return;
    // Collapse first: scrollHeight only shrinks if the box is smaller than
    // its content, so without this the box can grow and never come back.
    el.style.height = "auto";
    const line = parseFloat(getComputedStyle(el).lineHeight || "20") || 20;
    const max = line * maxRows;
    el.style.height = `${Math.min(el.scrollHeight, max)}px`;
    el.style.overflowY = el.scrollHeight > max ? "auto" : "hidden";
  }, [autoResize, maxRows]);

  // Fit on mount and whenever the value changes from outside, which is what
  // happens on revert and on take-the-new-one.
  React.useLayoutEffect(fit, [fit, value]);

  return (
    <textarea
      ref={ref}
      data-slot="textarea"
      value={value}
      onChange={(e) => {
        onChange?.(e);
        fit();
      }}
      className={cn(
        "border-input bg-surface-sunken/65 placeholder:text-muted-foreground selection:bg-primary selection:text-primary-foreground flex min-h-16 w-full min-w-0 rounded-md border px-3 py-2 text-base shadow-[inset_0_1px_0_color-mix(in_oklch,var(--foreground)_5%,transparent)] transition-[border-color,box-shadow,background-color] duration-150 outline-none focus-visible:border-primary/65 focus-visible:bg-background/80 focus-visible:shadow-[0_0_0_3px_color-mix(in_oklch,var(--primary)_18%,transparent),inset_0_1px_0_color-mix(in_oklch,var(--foreground)_6%,transparent)] disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 md:text-sm",
        autoResize && "resize-none",
        className,
      )}
      {...props}
    />
  );
}

export { Textarea };
