import { type HTMLAttributes } from "react";
import { cn } from "@/lib/utils";

function Skeleton({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn("rounded-md bg-primary/10 shimmer", className)}
      {...props}
    />
  );
}

export { Skeleton };
