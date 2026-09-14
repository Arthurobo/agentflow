import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex w-fit shrink-0 items-center justify-center gap-1 overflow-hidden whitespace-nowrap rounded-md border px-2 py-0.5 text-xs font-semibold shadow-[0_1px_0_color-mix(in_oklch,var(--foreground)_7%,transparent)_inset] transition-[background-color,border-color,color,box-shadow] duration-150 [&>svg]:pointer-events-none [&>svg]:size-3",
  {
    variants: {
      variant: {
        default:
          "border-primary/30 bg-primary/18 text-primary shadow-[0_1px_0_color-mix(in_oklch,white_12%,transparent)_inset]",
        secondary: "border-border/65 bg-secondary/70 text-secondary-foreground",
        ghost: "border-transparent text-muted-foreground",
        destructive: "border-destructive/30 bg-destructive/18 text-destructive",
        outline: "border-border/80 bg-surface-raised/45 text-foreground",
        info: "border-primary/30 bg-primary/15 text-primary",
        success: "border-success/30 bg-success/15 text-success",
        live: "border-success/30 bg-success/15 text-success shadow-[0_0_24px_color-mix(in_oklch,var(--success)_14%,transparent)] motion-safe:animate-pulse",
        warning: "border-warning/30 bg-warning/15 text-warning",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

function Badge({
  className,
  variant,
  ...props
}: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return (
    <span
      data-slot="badge"
      className={cn(badgeVariants({ variant }), className)}
      {...props}
    />
  );
}

export { Badge, badgeVariants };
