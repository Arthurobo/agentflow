import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const buttonVariants = cva(
  "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-semibold transition-[background-color,border-color,color,box-shadow,transform] duration-150 hover:-translate-y-px active:translate-y-0 disabled:pointer-events-none disabled:translate-y-0 disabled:opacity-50 [&_svg]:pointer-events-none [&_svg:not([class*='size-'])]:size-4 shrink-0 [&_svg]:shrink-0 outline-none focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px]",
  {
    variants: {
      variant: {
        default:
          "border border-primary/35 bg-[linear-gradient(180deg,color-mix(in_oklch,var(--primary)_100%,white_8%),color-mix(in_oklch,var(--primary)_88%,black_12%))] text-primary-foreground shadow-[0_1px_0_color-mix(in_oklch,white_24%,transparent)_inset,0_10px_30px_color-mix(in_oklch,var(--primary)_24%,transparent)] hover:shadow-[0_1px_0_color-mix(in_oklch,white_28%,transparent)_inset,0_14px_36px_color-mix(in_oklch,var(--primary)_30%,transparent)]",
        destructive:
          "border border-destructive/40 bg-[linear-gradient(180deg,color-mix(in_oklch,var(--destructive)_100%,white_8%),color-mix(in_oklch,var(--destructive)_86%,black_14%))] text-destructive-foreground shadow-[0_1px_0_color-mix(in_oklch,white_18%,transparent)_inset,0_10px_26px_color-mix(in_oklch,var(--destructive)_22%,transparent)]",
        success:
          "border border-success/40 bg-[linear-gradient(180deg,color-mix(in_oklch,var(--success)_100%,white_8%),color-mix(in_oklch,var(--success)_84%,black_16%))] text-primary-foreground shadow-[0_1px_0_color-mix(in_oklch,white_18%,transparent)_inset,0_10px_26px_color-mix(in_oklch,var(--success)_20%,transparent)]",
        danger:
          "border border-destructive/45 bg-destructive/10 text-destructive shadow-sm hover:bg-destructive hover:text-destructive-foreground",
        outline:
          "border border-border/80 bg-surface-raised/70 shadow-[0_1px_0_color-mix(in_oklch,var(--foreground)_7%,transparent)_inset] hover:border-primary/35 hover:bg-accent/70 hover:text-accent-foreground",
        secondary:
          "border border-border/60 bg-secondary/80 text-secondary-foreground shadow-sm hover:bg-secondary",
        ghost: "hover:bg-accent/70 hover:text-accent-foreground",
        link: "text-primary underline-offset-4 hover:underline",
      },
      size: {
        // 44px minimum touch targets (Apple HIG) — every tap lands on phones.
        default: "h-11 px-4 py-2 has-[>svg]:px-3",
        sm: "h-10 rounded-md gap-1.5 px-3.5 has-[>svg]:px-3",
        lg: "h-12 rounded-md px-6 has-[>svg]:px-4",
        xl: "h-14 rounded-lg px-5 text-base has-[>svg]:px-4",
        icon: "size-11",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

function Button({
  className,
  variant,
  size,
  asChild = false,
  ...props
}: React.ComponentProps<"button"> &
  VariantProps<typeof buttonVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : "button";
  return (
    <Comp
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  );
}

export { Button, buttonVariants };
