import { type HTMLAttributes } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex items-center rounded-md border px-2.5 py-0.5 text-xs font-semibold transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2",
  {
    variants: {
      variant: {
        default:
          "border-transparent bg-primary text-primary-foreground shadow dark:shadow-[0_0_12px_-3px_hsl(var(--glow-blue)/0.4)]",
        secondary:
          "border-transparent bg-secondary text-secondary-foreground",
        destructive:
          "border-transparent bg-destructive text-destructive-foreground shadow dark:shadow-[0_0_12px_-3px_hsl(var(--glow-red)/0.3)]",
        outline: "text-foreground",
        success:
          "border-transparent bg-emerald-100 text-emerald-800 dark:bg-emerald-500/10 dark:text-emerald-400 dark:border-emerald-500/20 dark:shadow-[0_0_10px_-3px_hsl(var(--glow-emerald)/0.3)]",
        warning:
          "border-transparent bg-amber-100 text-amber-800 dark:bg-amber-500/10 dark:text-amber-400 dark:border-amber-500/20 dark:shadow-[0_0_10px_-3px_hsl(var(--glow-amber)/0.3)]",
        architecture:
          "border-transparent bg-purple-100 text-purple-800 dark:bg-purple-500/10 dark:text-purple-400 dark:border-purple-500/20 dark:shadow-[0_0_10px_-3px_hsl(var(--glow-purple)/0.3)]",
        security:
          "border-transparent bg-red-100 text-red-800 dark:bg-red-500/10 dark:text-red-400 dark:border-red-500/20 dark:shadow-[0_0_10px_-3px_hsl(var(--glow-red)/0.25)]",
        code_review:
          "border-transparent bg-cyan-100 text-cyan-800 dark:bg-cyan-500/10 dark:text-cyan-400 dark:border-cyan-500/20 dark:shadow-[0_0_10px_-3px_hsl(var(--glow-cyan)/0.3)]",
        trade_off:
          "border-transparent bg-amber-100 text-amber-800 dark:bg-amber-500/10 dark:text-amber-400 dark:border-amber-500/20",
        planning:
          "border-transparent bg-blue-100 text-blue-800 dark:bg-blue-500/10 dark:text-blue-400 dark:border-blue-500/20",
        investigation:
          "border-transparent bg-emerald-100 text-emerald-800 dark:bg-emerald-500/10 dark:text-emerald-400 dark:border-emerald-500/20",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  },
);

export interface BadgeProps
  extends HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return (
    <div className={cn(badgeVariants({ variant }), className)} {...props} />
  );
}

/** Maps a decision_type string to the best badge variant. */
function decisionTypeBadgeVariant(
  decisionType: string,
): NonNullable<VariantProps<typeof badgeVariants>["variant"]> {
  const map: Record<string, NonNullable<VariantProps<typeof badgeVariants>["variant"]>> = {
    architecture: "architecture",
    security: "security",
    code_review: "code_review",
    trade_off: "trade_off",
    planning: "planning",
    investigation: "investigation",
    assessment: "trade_off",
    model_selection: "architecture",
    data_source: "planning",
    deployment: "security",
    error_handling: "warning",
    feature_scope: "code_review",
  };
  return map[decisionType] ?? "secondary";
}

export { Badge, badgeVariants, decisionTypeBadgeVariant };
