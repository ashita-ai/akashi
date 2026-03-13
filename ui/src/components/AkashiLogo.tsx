import { cn } from "@/lib/utils";

/**
 * Akashi logo — a square frame with a stylized "K" breaking through
 * the bottom-right corner. Rendered as inline SVG so it scales cleanly,
 * works on any background, and inherits color via currentColor.
 */
export function AkashiLogo({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 100 100"
      fill="none"
      stroke="currentColor"
      strokeLinecap="square"
      strokeLinejoin="miter"
      className={cn("shrink-0", className)}
      aria-label="Akashi"
      role="img"
    >
      {/* Square frame — top + right (partial) */}
      <path d="M7 7 H93 V48" strokeWidth="13" />
      {/* Square frame — left + bottom (partial) */}
      <path d="M7 7 V93 H48" strokeWidth="13" />
      {/* K upper arm: right-end → bottom-end */}
      <line x1="87" y1="52" x2="52" y2="87" strokeWidth="13" />
      {/* K lower arm: extends down-right */}
      <line x1="66" y1="52" x2="94" y2="90" strokeWidth="13" />
    </svg>
  );
}

/**
 * Full brand mark: logo + "Akashi" wordmark side by side.
 * Logo renders in primary blue (matching the landing page); wordmark in foreground.
 */
export function AkashiBrand({ className, logoSize = "h-7 w-7" }: { className?: string; logoSize?: string }) {
  return (
    <div className={cn("flex items-center gap-2.5", className)}>
      <AkashiLogo className={cn(logoSize, "text-primary drop-shadow-[0_0_8px_hsl(var(--glow-blue)/0.45)]")} />
      <span className="text-[17px] font-semibold tracking-tight text-foreground">Akashi</span>
    </div>
  );
}
