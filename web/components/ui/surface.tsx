import { Loader2 } from "lucide-react";
import * as React from "react";
import { cn } from "@/lib/utils";

export function PageShell({
  className,
  ...props
}: React.ComponentProps<"main">) {
  return <main className={cn("af-page", className)} {...props} />;
}

export function PageHeader({
  eyebrow,
  title,
  description,
  actions,
  className,
}: {
  eyebrow?: React.ReactNode;
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
}) {
  return (
    <header
      className={cn(
        "mb-5 flex flex-col gap-3 border-b border-border/60 pb-5 md:flex-row md:items-end md:justify-between",
        className,
      )}
    >
      <div className="min-w-0">
        {eyebrow && <div className="af-label mb-1">{eyebrow}</div>}
        <h1 className="text-balance text-3xl font-semibold leading-[1.05] tracking-normal text-foreground drop-shadow-[0_10px_28px_color-mix(in_oklch,var(--primary)_10%,transparent)] md:text-4xl">
          {title}
        </h1>
        {description && (
          <p className="text-muted-foreground mt-2 max-w-3xl text-sm leading-6">
            {description}
          </p>
        )}
      </div>
      {actions && (
        <div className="flex shrink-0 flex-wrap gap-2">{actions}</div>
      )}
    </header>
  );
}

export function MetricStrip({
  children,
  className,
}: React.ComponentProps<"dl">) {
  return (
    <dl className={cn("grid grid-cols-2 gap-2 md:grid-cols-4", className)}>
      {children}
    </dl>
  );
}

export function Metric({
  label,
  value,
  hint,
}: {
  label: string;
  value: React.ReactNode;
  hint?: React.ReactNode;
}) {
  return (
    <div className="af-subtle-panel group px-3 py-2 transition-[border-color,background-color,transform] duration-150 hover:border-primary/35">
      <dt className="af-label">{label}</dt>
      <dd className="mt-1 truncate text-base font-semibold tabular-nums">
        {value}
      </dd>
      {hint && (
        <div className="text-muted-foreground mt-0.5 truncate text-[11px]">
          {hint}
        </div>
      )}
    </div>
  );
}

export function EmptyState({
  icon,
  title,
  children,
  loading,
}: {
  icon?: React.ReactNode;
  title: React.ReactNode;
  children?: React.ReactNode;
  /** loading turns this into the app's ONE way of saying "not yet". There
   *  were twelve: nine Skeletons, four literal strings and three bare
   *  animate-pulses, so nothing about waiting looked the same twice. */
  loading?: boolean;
}) {
  return (
    <div
      role={loading ? "status" : undefined}
      aria-busy={loading || undefined}
      className="text-muted-foreground flex flex-col items-center gap-3 py-14 text-center text-sm"
    >
      {loading ? (
        <div className="rounded-lg border border-border/70 bg-surface-raised/55 p-2 text-primary shadow-sm">
          <Loader2 className="size-4 animate-spin" aria-hidden />
        </div>
      ) : (
        icon && (
          <div className="rounded-lg border border-border/70 bg-surface-raised/55 p-2 text-primary shadow-sm">
            {icon}
          </div>
        )
      )}
      <p className="text-foreground text-base font-semibold">{title}</p>
      {children && <div className="max-w-sm text-xs leading-5">{children}</div>}
    </div>
  );
}

export function SectionHeader({
  eyebrow,
  title,
  description,
  actions,
  className,
}: {
  eyebrow?: React.ReactNode;
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "mb-4 flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between",
        className,
      )}
    >
      <div className="min-w-0">
        {eyebrow && <div className="af-label mb-1">{eyebrow}</div>}
        <h2 className="text-lg font-semibold leading-tight">{title}</h2>
        {description && (
          <p className="text-muted-foreground mt-1 text-sm leading-5">
            {description}
          </p>
        )}
      </div>
      {actions && (
        <div className="flex shrink-0 flex-wrap gap-2">{actions}</div>
      )}
    </div>
  );
}

export function Field({
  label,
  hint,
  children,
  className,
}: {
  label: React.ReactNode;
  hint?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <label className={cn("flex min-w-0 flex-col gap-1.5", className)}>
      <span className="af-label">{label}</span>
      {children}
      {hint && (
        <span className="text-muted-foreground text-xs leading-5">{hint}</span>
      )}
    </label>
  );
}

// FieldGroup is Field for a section holding MORE THAN ONE control. Field is a
// <label>, and a label wrapping several controls hands every one of them the
// label's whole text as its accessible name: a play button in a labelled grid
// announces as "Play, the sequence the engine enforces, Audit Deep" instead of
// "Audit". Same caption, same hint, same spacing, correct semantics.
export function FieldGroup({
  label,
  hint,
  children,
  className,
}: {
  label: React.ReactNode;
  hint?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  const id = React.useId();
  return (
    <div
      role="group"
      aria-labelledby={id}
      className={cn("flex min-w-0 flex-col gap-1.5", className)}
    >
      <span id={id} className="af-label">
        {label}
      </span>
      {children}
      {hint && (
        <span className="text-muted-foreground text-xs leading-5">{hint}</span>
      )}
    </div>
  );
}

// SelectedPill is the non-colour half of the selection idiom: a word, filled,
// so the chosen thing says so even where a hue does not read.
export function SelectedPill({
  children = "Selected",
  className,
}: {
  children?: React.ReactNode;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "bg-primary text-primary-foreground inline-flex shrink-0 items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-semibold",
        className,
      )}
    >
      {children}
    </span>
  );
}

export function ChipToggle({
  selected,
  children,
  onClick,
  className,
}: {
  selected: boolean;
  children: React.ReactNode;
  onClick: () => void;
  className?: string;
}) {
  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={onClick}
      className={cn(
        "inline-flex h-8 items-center rounded-md border px-2.5 font-mono text-xs shadow-sm transition-[background-color,border-color,color,box-shadow,transform] duration-150 hover:-translate-y-px active:translate-y-0",
        selected
          ? "af-selected text-primary font-semibold"
          : "border-border/75 bg-surface-raised/60 text-muted-foreground hover:border-primary/35 hover:bg-surface-raised hover:text-foreground",
        className,
      )}
    >
      {children}
    </button>
  );
}

export function CodePreview({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <pre
      className={cn(
        "max-h-44 overflow-auto rounded-lg border border-border/70 bg-surface-sunken/70 p-3 font-mono text-[11px] leading-relaxed shadow-[inset_0_1px_0_color-mix(in_oklch,var(--foreground)_5%,transparent)]",
        className,
      )}
    >
      {children}
    </pre>
  );
}

export function StatusDot({
  state = "idle",
  pulse = false,
}: {
  state?: "live" | "idle" | "warn" | "danger";
  pulse?: boolean;
}) {
  const tone =
    state === "live"
      ? "bg-success"
      : state === "warn"
        ? "bg-warning"
        : state === "danger"
          ? "bg-danger"
          : "bg-muted-foreground";
  return (
    <span className="relative inline-flex size-2.5 shrink-0">
      {pulse && (
        <span
          className={cn(
            "absolute inline-flex size-2.5 animate-ping rounded-full opacity-55",
            tone,
          )}
        />
      )}
      <span
        className={cn("relative inline-flex size-2.5 rounded-full", tone)}
      />
    </span>
  );
}
