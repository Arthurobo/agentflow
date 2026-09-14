"use client";

// CwdCombobox — the searchable working-directory picker for the run composer.
// Options come from GET /api/v1/agentd/cwds (directories this machine has run
// sessions in, most recent first), cached with a long staleTime because the
// list changes slowly.
//
// Behaviour contract (phone-first):
//   - type-to-filter on the path substring, case-insensitive;
//   - free text IS the value: any path can be typed, known or not, and a
//     failed or empty suggestion list never blocks typing;
//   - ArrowUp/Down + Enter keyboard nav on desktop, tap-to-select on phone;
//   - each option shows the path and when it was last used;
//   - a "Type a custom path…" row commits the raw typed text and dismisses the
//     suggestions (the input itself already accepts any path).
import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { FolderSearch, Pencil } from "lucide-react";
import { Input } from "@/components/ui/input";
import { listCwds, type CwdEntry } from "@/lib/agentd";
import { relTime } from "@/lib/format";
import { cn } from "@/lib/utils";

// filterCwds is the pure filter used by the dropdown and unit tests: a
// case-insensitive substring match on the path.
export function filterCwds(entries: CwdEntry[], query: string): CwdEntry[] {
  const q = query.trim().toLowerCase();
  if (!q) return entries;
  return entries.filter((e) => e.cwd.toLowerCase().includes(q));
}

export function CwdCombobox({
  value,
  onChange,
  entries,
  placeholder,
  id,
}: {
  value: string;
  onChange: (cwd: string) => void;
  /** Override the option source (tests / embedded use); defaults to the API. */
  entries?: CwdEntry[];
  placeholder?: string;
  id?: string;
}) {
  const query = useQuery({
    queryKey: ["run-cwds"],
    queryFn: () => listCwds(),
    staleTime: 5 * 60 * 1000, // the known-cwd list changes slowly
    enabled: entries === undefined, // embedded/tests supply their own options
  });
  const [open, setOpen] = useState(false);
  const [highlight, setHighlight] = useState(-1);
  const inputRef = useRef<HTMLInputElement>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  // mobile fires the input's focus AFTER an option tap lands — without this
  // guard the dropdown "closes" and instantly reopens, looking stuck open
  const suppressOpenRef = useRef(false);

  // outside click closes the dropdown (the modal overlay does not blur the
  // input, so a dismissal affordance is mandatory)
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
        setHighlight(-1);
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  const filtered = useMemo(
    () => filterCwds(entries ?? query.data ?? [], value),
    [entries, query.data, value],
  );

  const selectEntry = (e: CwdEntry) => {
    onChange(e.cwd);
    suppressOpenRef.current = true;
    setOpen(false);
    setHighlight(-1);
    setTimeout(() => {
      suppressOpenRef.current = false;
    }, 200);
  };

  const commitFreeText = () => {
    suppressOpenRef.current = true;
    setOpen(false);
    setHighlight(-1);
    inputRef.current?.focus();
    setTimeout(() => {
      suppressOpenRef.current = false;
    }, 200);
  };

  const move = (dir: 1 | -1) => {
    if (filtered.length === 0) return;
    setOpen(true);
    setHighlight((h) => {
      if (h === -1) return dir === 1 ? 0 : filtered.length - 1;
      const next = h + dir;
      if (next < 0) return filtered.length - 1;
      if (next >= filtered.length) return 0;
      return next;
    });
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      move(1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      move(-1);
    } else if (e.key === "Enter") {
      if (open && highlight >= 0 && filtered[highlight]) {
        e.preventDefault();
        selectEntry(filtered[highlight]);
      } else {
        commitFreeText();
      }
    } else if (e.key === "Escape") {
      setOpen(false);
    }
  };

  return (
    <div className="relative" ref={rootRef}>
      <Input
        id={id}
        ref={inputRef}
        value={value}
        onChange={(e) => {
          onChange(e.target.value);
          setOpen(true);
          setHighlight(-1);
        }}
        onFocus={() => {
          if (!suppressOpenRef.current) setOpen(true);
        }}
        onKeyDown={handleKeyDown}
        placeholder={placeholder ?? "/path/to/repo"}
        role="combobox"
        aria-expanded={open}
        aria-haspopup="listbox"
        aria-controls="run-cwd-options"
        aria-autocomplete="list"
        aria-label="Working directory"
        autoComplete="off"
        spellCheck={false}
      />
      {open && (
        <ul
          id="run-cwd-options"
          role="listbox"
          aria-label="Known working directories"
          className="bg-popover/96 text-popover-foreground absolute z-50 mt-1 max-h-64 w-full min-w-0 overflow-y-auto rounded-md border border-border/80 p-1 shadow-2xl backdrop-blur-xl"
        >
          {filtered.length === 0 && (
            <li className="text-muted-foreground px-2 py-2 text-xs leading-5">
              No known paths match — keep typing to use a custom path.
            </li>
          )}
          {filtered.map((e, i) => (
            <li
              key={e.cwd}
              role="option"
              aria-selected={i === highlight}
              onMouseDown={(ev) => ev.preventDefault()}
              onClick={() => selectEntry(e)}
              className={cn(
                "flex min-h-11 cursor-pointer items-center justify-between gap-2 rounded-sm px-2.5 py-2 text-left",
                i === highlight
                  ? "bg-accent text-accent-foreground"
                  : "hover:bg-accent/50",
              )}
            >
              <span className="min-w-0 truncate font-mono text-xs">
                {e.cwd}
              </span>
              {e.lastUsedAt > 0 && (
                <span className="text-muted-foreground shrink-0 text-[10px] tabular-nums">
                  {relTime(e.lastUsedAt)}
                </span>
              )}
            </li>
          ))}
          <li
            role="option"
            aria-selected={false}
            onMouseDown={(ev) => ev.preventDefault()}
            onClick={commitFreeText}
            className="flex min-h-11 cursor-pointer items-center gap-2 rounded-sm px-2.5 py-2 text-sm text-muted-foreground hover:bg-accent/50 hover:text-foreground"
          >
            <Pencil className="size-3.5 shrink-0" />
            Type a custom path…
          </li>
        </ul>
      )}
      {value === "" && !open && (
        <FolderSearch className="text-muted-foreground/70 pointer-events-none absolute top-1/2 right-3 size-4 -translate-y-1/2" />
      )}
    </div>
  );
}
