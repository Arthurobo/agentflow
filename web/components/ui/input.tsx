import * as React from "react";
import { cn } from "@/lib/utils";

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "border-input bg-surface-sunken/65 file:text-foreground placeholder:text-muted-foreground selection:bg-primary selection:text-primary-foreground flex h-9 w-full min-w-0 rounded-md border px-3 py-1 text-base shadow-[inset_0_1px_0_color-mix(in_oklch,var(--foreground)_5%,transparent)] transition-[border-color,box-shadow,background-color] duration-150 outline-none file:inline-flex file:h-7 file:border-0 file:bg-transparent file:text-sm file:font-medium focus-visible:border-primary/65 focus-visible:bg-background/80 focus-visible:shadow-[0_0_0_3px_color-mix(in_oklch,var(--primary)_18%,transparent),inset_0_1px_0_color-mix(in_oklch,var(--foreground)_6%,transparent)] disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 md:text-sm",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
