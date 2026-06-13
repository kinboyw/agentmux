import * as React from "react";
import { RefreshCw } from "lucide-react";
import { cn } from "../../lib/utils";

type ButtonProps = React.ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "default" | "secondary" | "ghost" | "destructive";
  size?: "xs" | "sm" | "default" | "icon" | "icon-sm";
  loading?: boolean;
};

export function Button({ className, variant = "default", size = "default", loading = false, disabled, children, ...props }: ButtonProps) {
  const iconOnly = size === "icon" || size === "icon-sm";
  return (
    <button
      className={cn(
        "inline-flex items-center justify-center gap-2 rounded-md border text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50",
        variant === "default" && "border-primary bg-primary text-primary-foreground hover:bg-primary/90",
        variant === "secondary" && "border-border bg-secondary text-secondary-foreground hover:bg-secondary/80",
        variant === "ghost" && "border-transparent bg-transparent text-foreground hover:bg-secondary",
        variant === "destructive" && "border-destructive bg-destructive text-destructive-foreground hover:bg-destructive/90",
        size === "default" && "h-9 px-3",
        size === "sm" && "h-8 px-2.5 text-xs",
        size === "xs" && "h-7 px-2 text-xs",
        size === "icon" && "h-9 w-9",
        size === "icon-sm" && "h-7 w-7",
        className,
      )}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      {...props}
    >
      {loading ? <RefreshCw className={cn("animate-spin", size === "xs" || size === "icon-sm" ? "h-3.5 w-3.5" : "h-4 w-4")} /> : null}
      {loading && iconOnly ? null : children}
    </button>
  );
}
