"use client";

// Picker — the bottom-sheet list every section of the control sheet (and the
// loop wizard, and the composer's slash menu) renders from. Fuzzy search,
// grouped sticky headers, current-selection mark, keyboard + touch, empty
// state, and a visible "engine cannot do this" state.
//
// Layout contract, because the previous version did not scroll on a phone:
//   - Rendered through a PORTAL onto document.body. It used to mount inside
//     the control sheet's own fixed, blurred, scrolling box; WebKit treats a
//     backdrop-filter ancestor as the containing block for fixed children,
//     so on iPhone the picker was positioned and clipped INSIDE the sheet.
//   - One flex column with an explicit max height; header and search are
//     fixed rows, the list is the only thing that scrolls: min-h-0 so the
//     flex item may shrink, overflow-y-auto, overscroll-contain so the page
//     behind never takes the gesture, touch-action pan-y so the browser
//     knows the vertical drag is ours.
//   - Safe-area padding lives INSIDE the scroller, so the last row clears
//     the home indicator instead of the whole sheet floating above it.
import { useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Check, ChevronLeft, Loader2, Search, X } from "lucide-react";
import { cn } from "@/lib/utils";

export interface PickerItem {
  id: string;
  label: string;
  description?: string;
  // badge is the small "anthropic" / "build" tag rendered to the right of
  // the label; never used as a primary key.
  badge?: string;
  group?: string;
  current?: boolean;
  disabled?: boolean;
  disabledReason?: string;
}

export interface PickerProps {
  open: boolean;
  title: string;
  // recents are pinned at the top (most-recent-first); matches by id.
  recents?: string[];
  items: PickerItem[];
  loading?: boolean;
  // note is the "engine does not enumerate models" message — the picker
  // renders this when items is empty AND loading is false.
  note?: string;
  // engineUnsupported forces an empty / explanatory state regardless of
  // items (used when the active engine's Caps say "no listModels").
  engineUnsupported?: boolean;
  engineUnsupportedMessage?: string;
  // backLabel names what closing returns TO ("Control"). The picker covers
  // the sheet it was opened from, and on a phone there is no Escape key and
  // no window chrome — without a visible way back the only exit was
  // reloading the page.
  backLabel?: string;
  onClose: () => void;
  onPick: (item: PickerItem) => void | Promise<void>;
}

export function Picker({
  open,
  title,
  recents,
  items,
  loading,
  note,
  engineUnsupported,
  engineUnsupportedMessage,
  backLabel = "Back",
  onClose,
  onPick,
}: PickerProps) {
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    // pinned recents first (always when present, even if they don't match)
    const recentsOut: PickerItem[] = [];
    if (recents && recents.length > 0 && q === "") {
      for (const id of recents) {
        const it = items.find((i) => i.id === id);
        if (it) recentsOut.push({ ...it, group: "Recent" });
      }
    }
    const filtered = items.filter((i) => {
      if (i.disabled) return false;
      if (q === "") return true;
      return (
        i.label.toLowerCase().includes(q) ||
        i.id.toLowerCase().includes(q) ||
        (i.badge ?? "").toLowerCase().includes(q) ||
        (i.description ?? "").toLowerCase().includes(q)
      );
    });
    // sort by group header order, then by label
    const byGroup = new Map<string, PickerItem[]>();
    for (const it of filtered) {
      const g = it.group ?? "Other";
      if (!byGroup.has(g)) byGroup.set(g, []);
      byGroup.get(g)!.push(it);
    }
    for (const arr of byGroup.values()) {
      arr.sort((a, b) => a.label.localeCompare(b.label));
    }
    const out: PickerItem[] = [...recentsOut];
    for (const g of Array.from(byGroup.keys()).sort()) {
      out.push(...(byGroup.get(g) ?? []));
    }
    return out;
  }, [items, query, recents]);

  // Focus the search when the sheet opens — on touch too. This used to be
  // skipped on a coarse pointer because focusing popped the OS keyboard over
  // the list; the sheet now tracks the visual viewport (below) so the list
  // rides above the keyboard, and NOT focusing was the real bug: the sheet
  // opens from the composer, whose keyboard stays up, so a user typing to
  // filter was sending keystrokes to the composer behind the sheet and the
  // list never narrowed. Blur whatever had focus first (that composer) so the
  // already-open keyboard now drives the search field.
  useEffect(() => {
    if (!open) return undefined;
    const t = setTimeout(() => {
      const active = document.activeElement as HTMLElement | null;
      if (active && active !== inputRef.current) active.blur?.();
      inputRef.current?.focus();
    }, 50);
    return () => clearTimeout(t);
  }, [open]);

  // Keep the sheet above the on-screen keyboard. A `position: fixed; bottom: 0`
  // sheet sits at the LAYOUT viewport bottom, which on iOS is behind the
  // keyboard — so the search results were buried the moment the field focused.
  // Track the visual viewport and lift the sheet by the keyboard's height,
  // capping its height to the space that is actually visible.
  const [sheetStyle, setSheetStyle] = useState<{
    bottom: number | string;
    maxHeight: string;
  }>({ bottom: 0, maxHeight: "85dvh" });
  useEffect(() => {
    if (!open) return undefined;
    const vv =
      typeof window !== "undefined" ? window.visualViewport : undefined;
    if (!vv) return undefined;
    const apply = () => {
      const keyboard = Math.max(
        0,
        window.innerHeight - vv.height - vv.offsetTop,
      );
      setSheetStyle({
        bottom: keyboard,
        maxHeight: `${Math.round(vv.height * 0.92)}px`,
      });
    };
    apply();
    vv.addEventListener("resize", apply);
    vv.addEventListener("scroll", apply);
    return () => {
      vv.removeEventListener("resize", apply);
      vv.removeEventListener("scroll", apply);
    };
  }, [open]);

  // Lock the page behind the sheet while it is open. The run page locks
  // itself already; the loop wizard and loop actions do not, and a sheet
  // over a page that still scrolls is the classic two-scrollers bug.
  useEffect(() => {
    if (!open) return undefined;
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = prev;
    };
  }, [open]);

  // keyboard: escape closes, arrow up/down moves selection, enter picks.
  // The visible list is captured by closure; we re-bind the listener on
  // every render so arrow keys see the current filtered list.
  useEffect(() => {
    if (!open) return undefined;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
        return;
      }
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setActive((a) => Math.min(a + 1, visible.length - 1));
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setActive((a) => Math.max(0, a - 1));
        return;
      }
      if (e.key === "Enter") {
        e.preventDefault();
        const it = visible[active];
        if (it && !it.disabled) void onPick(it);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, visible, active, onClose, onPick]);

  // keep the keyboard-selected row in view while arrowing through a long list
  useEffect(() => {
    if (!open) return;
    const el = listRef.current?.querySelector<HTMLElement>(`[data-index="${active}"]`);
    // jsdom has no scrollIntoView; browsers do
    if (el && typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "nearest" });
  }, [open, active]);

  if (!open) return null;
  if (typeof document === "undefined") return null;

  return createPortal(
    <>
      {/* Backdrop — dims the sheet underneath and is itself the escape
          hatch: on a phone, tapping outside a sheet is the gesture people
          reach for first. Above the control sheet (z-40), below the picker
          (z-50). */}
      <div
        data-testid="picker-backdrop"
        aria-hidden
        onClick={onClose}
        className="fixed inset-0 z-[45] bg-black/40"
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="bg-background border-border/60 fixed inset-x-0 z-50 flex flex-col rounded-t-2xl border-t shadow-2xl"
        style={{ bottom: sheetStyle.bottom, maxHeight: sheetStyle.maxHeight }}
      >
        <div className="mx-auto flex min-h-0 w-full max-w-2xl flex-1 flex-col">
          {/* grab handle: the visual cue that this is a sheet, not a page */}
          <div className="flex justify-center pt-2 pb-1" aria-hidden>
            <span className="bg-border h-1 w-9 rounded-full" />
          </div>

          <div className="flex items-center justify-between gap-2 px-2 pb-1">
            {/* min-h-11 / min-w-11 is the 44px tap target; the label is part
                of the target because a bare chevron is a coin-sized hit box. */}
            <button
              type="button"
              onClick={onClose}
              aria-label={`${backLabel} to the control sheet`}
              className="text-muted-foreground hover:bg-accent/60 hover:text-foreground active:bg-accent inline-flex min-h-11 shrink-0 items-center gap-0.5 rounded-full pr-3 pl-1.5 text-[13px] font-medium transition-colors"
            >
              <ChevronLeft className="size-5" aria-hidden />
              {backLabel}
            </button>
            <h2 className="text-foreground min-w-0 truncate text-[15px] font-semibold">
              {title}
            </h2>
            <button
              type="button"
              onClick={onClose}
              aria-label="Close"
              className="text-muted-foreground hover:bg-accent/60 hover:text-foreground active:bg-accent grid min-h-11 min-w-11 shrink-0 place-items-center rounded-full transition-colors"
            >
              <X className="size-5" aria-hidden />
            </button>
          </div>

          <div className="px-3 pb-2">
            <label className="bg-muted/50 border-border/60 focus-within:border-ring/60 focus-within:bg-background flex min-h-11 items-center gap-2 rounded-xl border px-3 transition-colors">
              <Search className="text-muted-foreground size-4 shrink-0" aria-hidden />
              <input
                ref={inputRef}
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value);
                  setActive(0);
                }}
                placeholder="Search…"
                className="placeholder:text-muted-foreground/60 w-full bg-transparent text-[16px] outline-none"
                autoComplete="off"
                autoCorrect="off"
                autoCapitalize="off"
                spellCheck={false}
                enterKeyHint="done"
              />
              {loading && (
                <Loader2 className="text-muted-foreground size-4 shrink-0 animate-spin" />
              )}
            </label>
          </div>

          {/* THE scroller. min-h-0 lets this flex item shrink below its
              content; overscroll-contain keeps the gesture here; pan-y tells
              the browser the vertical drag belongs to this element. */}
          <div
            ref={listRef}
            className="border-border/40 min-h-0 flex-1 overflow-y-auto overscroll-contain border-t px-2 pt-1 [-webkit-overflow-scrolling:touch] [touch-action:pan-y]"
            style={{ paddingBottom: "max(env(safe-area-inset-bottom), 0.75rem)" }}
          >
            {engineUnsupported ? (
              <Empty
                title="Engine cannot list this"
                body={
                  engineUnsupportedMessage ??
                  "This engine does not expose this section."
                }
              />
            ) : loading ? (
              <Empty title="Loading…" />
            ) : visible.length === 0 ? (
              <Empty
                title={query ? "No matches" : "Nothing here yet"}
                body={note ?? (query ? "Try a different search." : "")}
              />
            ) : (
              <ul className="flex flex-col" role="listbox" aria-label={title}>
                {visible.map((it, idx) => {
                  const prev = visible[idx - 1];
                  const showHeader =
                    it.group && (!prev || prev.group !== it.group);
                  return (
                    <li key={`${it.group ?? "_"}:${it.id}`}>
                      {showHeader && (
                        <p className="bg-background text-muted-foreground sticky top-0 z-10 px-3 pt-3 pb-1 text-[11px] font-semibold tracking-wider uppercase">
                          {it.group}
                        </p>
                      )}
                      <button
                        type="button"
                        role="option"
                        aria-selected={it.current === true}
                        data-index={idx}
                        onClick={() => void onPick(it)}
                        disabled={it.disabled}
                        className={cn(
                          "hover:bg-accent/60 active:bg-accent flex min-h-12 w-full items-center gap-3 rounded-xl px-3 py-2 text-left transition-colors",
                          idx === active && "bg-accent/70",
                          it.current && "bg-primary/5",
                          it.disabled && "opacity-50",
                        )}
                      >
                        <span className="min-w-0 flex-1">
                          <span className="block truncate text-[16px] font-medium">
                            {it.label}
                          </span>
                          {it.description && (
                            <span className="text-muted-foreground block truncate text-[13px]">
                              {it.description}
                            </span>
                          )}
                        </span>
                        {it.badge && (
                          <span className="bg-muted text-muted-foreground shrink-0 rounded-full px-2 py-0.5 text-[11px]">
                            {it.badge}
                          </span>
                        )}
                        {it.current && (
                          <span className="bg-success/15 text-success inline-flex shrink-0 items-center gap-1 rounded-full py-0.5 pr-2 pl-1.5 text-[11px] font-medium">
                            <Check className="size-3.5" aria-hidden />
                            current
                          </span>
                        )}
                      </button>
                    </li>
                  );
                })}
              </ul>
            )}
          </div>
        </div>
      </div>
    </>,
    document.body,
  );
}

function Empty({ title, body }: { title: string; body?: string }) {
  return (
    <div className="text-muted-foreground flex flex-col items-center gap-1 px-6 py-12 text-center">
      <p className="text-foreground text-sm font-medium">{title}</p>
      {body && <p className="max-w-md text-[13px]">{body}</p>}
    </div>
  );
}
