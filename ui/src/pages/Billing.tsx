import { useQuery, useMutation } from "@tanstack/react-query";
import {
  getUsage,
  createCheckoutSession,
  createPortalSession,
  ApiError,
} from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { percentOf } from "@/lib/utils";
import { CreditCard, ExternalLink } from "lucide-react";
import { useState } from "react";

const planDetails: Record<string, { label: string; features: string[] }> = {
  free: {
    label: "Free",
    features: [
      "100 decisions/month",
      "3 agents",
      "Community support",
    ],
  },
  pro: {
    label: "Pro",
    features: [
      "10,000 decisions/month",
      "25 agents",
      "Priority support",
      "Semantic search",
      "Data export",
    ],
  },
  enterprise: {
    label: "Enterprise",
    features: [
      "Unlimited decisions",
      "Unlimited agents",
      "Dedicated support",
      "Custom integrations",
      "SLA guarantee",
    ],
  },
};

export default function Billing() {
  const [error, setError] = useState<string | null>(null);

  const { data: usage, isPending } = useQuery({
    queryKey: ["usage"],
    queryFn: getUsage,
  });

  const checkoutMutation = useMutation({
    mutationFn: () => {
      const baseUrl = window.location.origin;
      return createCheckoutSession(
        `${baseUrl}/billing?success=true`,
        `${baseUrl}/billing`,
      );
    },
    onSuccess: (data) => {
      window.location.href = data.checkout_url;
    },
    onError: (err) => {
      setError(
        err instanceof ApiError ? err.message : "Failed to start checkout",
      );
    },
  });

  const portalMutation = useMutation({
    mutationFn: () =>
      createPortalSession(`${window.location.origin}/billing`),
    onSuccess: (data) => {
      window.location.href = data.portal_url;
    },
    onError: (err) => {
      setError(
        err instanceof ApiError ? err.message : "Failed to open portal",
      );
    },
  });

  const plan = usage?.plan ?? "free";
  const details = planDetails[plan] ?? planDetails.free;

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold tracking-tight">Usage & Billing</h1>

      {isPending ? (
        <div className="space-y-4">
          <Skeleton className="h-48 w-full" />
          <Skeleton className="h-32 w-full" />
        </div>
      ) : (
        <>
          {/* Plan card */}
          <Card>
            <CardHeader>
              <div className="flex items-center justify-between">
                <div>
                  <CardTitle className="flex items-center gap-2">
                    <CreditCard className="h-5 w-5" />
                    Current Plan
                  </CardTitle>
                  <CardDescription>{details?.label}</CardDescription>
                </div>
                <Badge className="text-lg px-4 py-1">{plan}</Badge>
              </div>
            </CardHeader>
            <CardContent className="space-y-4">
              {details && (
                <ul className="space-y-1 text-sm text-muted-foreground">
                  {details.features.map((f) => (
                    <li key={f} className="flex items-center gap-2">
                      <span className="h-1.5 w-1.5 rounded-full bg-primary" />
                      {f}
                    </li>
                  ))}
                </ul>
              )}

              <div className="flex gap-3 pt-2">
                {plan === "free" && (
                  <Button
                    onClick={() => checkoutMutation.mutate()}
                    disabled={checkoutMutation.isPending}
                  >
                    <ExternalLink className="h-4 w-4" />
                    {checkoutMutation.isPending
                      ? "Redirecting\u2026"
                      : "Upgrade to Pro"}
                  </Button>
                )}
                {plan !== "free" && (
                  <Button
                    variant="outline"
                    onClick={() => portalMutation.mutate()}
                    disabled={portalMutation.isPending}
                  >
                    <ExternalLink className="h-4 w-4" />
                    {portalMutation.isPending
                      ? "Redirecting\u2026"
                      : "Manage Subscription"}
                  </Button>
                )}
              </div>
              {error && (
                <p className="text-sm text-destructive">{error}</p>
              )}
            </CardContent>
          </Card>

          {/* Usage meters */}
          {usage && (
            <Card>
              <CardHeader>
                <CardTitle className="text-sm font-medium">
                  Usage — {usage.period}
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-4">
                <UsageMeter
                  label="Decisions"
                  used={usage.decision_count}
                  limit={usage.decision_limit}
                />
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}

function UsageMeter({
  label,
  used,
  limit,
}: {
  label: string;
  used: number;
  limit: number;
}) {
  const pct = percentOf(used, limit);
  const color =
    pct >= 90
      ? "bg-destructive"
      : pct >= 70
        ? "bg-amber-500"
        : "bg-emerald-500";

  return (
    <div className="space-y-1">
      <div className="flex justify-between text-sm">
        <span>{label}</span>
        <span className="text-muted-foreground">
          {used.toLocaleString()} / {limit.toLocaleString()} ({pct}%)
        </span>
      </div>
      <div className="h-2 w-full rounded-full bg-secondary">
        <div
          className={`h-full rounded-full transition-all ${color}`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}
