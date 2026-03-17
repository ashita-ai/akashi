import { useQuery } from "@tanstack/react-query";
import { getRecentDecisions, listAgents, getTraceHealth } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { formatRelativeTime } from "@/lib/utils";
import {
  AlertTriangle,
  FileText,
  HeartPulse,
  Info,
  Lightbulb,
  Users,
} from "lucide-react";
import { Link } from "react-router";

const healthStatusConfig: Record<string, { label: string; color: string; ring: string }> = {
  healthy: { label: "Healthy", color: "text-emerald-500", ring: "stroke-emerald-500" },
  needs_attention: { label: "Needs Attention", color: "text-amber-500", ring: "stroke-amber-500" },
  insufficient_data: { label: "No Data", color: "text-muted-foreground", ring: "stroke-muted-foreground" },
};

/** Tiny SVG ring that fills to a given percentage. */
function ProgressRing({ value, className }: { value: number; className?: string }) {
  const r = 16;
  const circumference = 2 * Math.PI * r;
  const offset = circumference * (1 - Math.min(Math.max(value, 0), 1));
  return (
    <svg viewBox="0 0 40 40" className={className} aria-hidden="true">
      <defs>
        <filter id="ring-glow">
          <feGaussianBlur stdDeviation="1.5" result="blur" />
          <feMerge>
            <feMergeNode in="blur" />
            <feMergeNode in="SourceGraphic" />
          </feMerge>
        </filter>
      </defs>
      <circle cx="20" cy="20" r={r} fill="none" strokeWidth="4" className="stroke-muted/40" />
      <circle
        cx="20"
        cy="20"
        r={r}
        fill="none"
        strokeWidth="4"
        strokeLinecap="round"
        strokeDasharray={circumference}
        strokeDashoffset={offset}
        className="transition-[stroke-dashoffset] duration-700 ease-out"
        transform="rotate(-90 20 20)"
        filter="url(#ring-glow)"
      />
    </svg>
  );
}

export default function Dashboard() {
  const recent = useQuery({
    queryKey: ["dashboard", "recent"],
    queryFn: () => getRecentDecisions({ limit: 10 }),
  });
  const agents = useQuery({
    queryKey: ["dashboard", "agents"],
    queryFn: listAgents,
  });
  const traceHealth = useQuery({
    queryKey: ["dashboard", "trace-health"],
    queryFn: getTraceHealth,
    staleTime: 30_000,
  });

  const healthConfig = healthStatusConfig[traceHealth.data?.status ?? ""] ?? { label: "Unknown", color: "text-muted-foreground", ring: "stroke-muted-foreground" };
  const completeness = traceHealth.data?.completeness.avg_completeness ?? 0;

  return (
    <div className="space-y-8 animate-page">
      <div className="page-header flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Dashboard</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Decision audit trail and agent coordination health
          </p>
        </div>
        {traceHealth.data && (
          <div className="shrink-0 flex items-center gap-2 rounded-full border px-3 py-1.5 text-[11px] font-medium text-muted-foreground bg-muted/30 uppercase tracking-wider">
            <span className={`h-1.5 w-1.5 rounded-full ${healthConfig.color.replace("text-", "bg-")}`} />
            {healthConfig.label}
          </div>
        )}
      </div>

      {/* Metric cards */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Card className="gradient-border">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">Decisions</CardTitle>
            <FileText className="h-4 w-4 text-primary/50" />
          </CardHeader>
          <CardContent>
            {recent.isPending ? (
              <Skeleton className="h-8 w-20" />
            ) : (
              <div className="text-3xl font-semibold tabular-nums tracking-tight">
                {(recent.data?.total ?? 0).toLocaleString()}
              </div>
            )}
            {traceHealth.data && (
              <p className="text-[11px] text-muted-foreground mt-1">
                {traceHealth.data.completeness.total_decisions} total traced
              </p>
            )}
          </CardContent>
        </Card>

        <Card className="gradient-border">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">Active Agents</CardTitle>
            <Users className="h-4 w-4 text-primary/50" />
          </CardHeader>
          <CardContent>
            {agents.isPending ? (
              <Skeleton className="h-8 w-12" />
            ) : (
              <div className="text-3xl font-semibold tabular-nums tracking-tight">
                {agents.data?.length ?? 0}
              </div>
            )}
            <p className="text-[11px] text-muted-foreground mt-1">registered</p>
          </CardContent>
        </Card>

        <Link to="/conflicts">
          <Card className="gradient-border">
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">Open Conflicts</CardTitle>
              <AlertTriangle className="h-4 w-4 text-amber-500/60" />
            </CardHeader>
            <CardContent>
              {traceHealth.isPending ? (
                <Skeleton className="h-8 w-12" />
              ) : (
                <div className="text-3xl font-semibold tabular-nums tracking-tight">
                  {traceHealth.data?.conflicts?.open ?? 0}
                </div>
              )}
              <p className="text-[11px] text-muted-foreground mt-1">need attention</p>
            </CardContent>
          </Card>
        </Link>

        <Card className="gradient-border">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">Trace Health</CardTitle>
            <HeartPulse className="h-4 w-4 text-emerald-500/60" />
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-8 w-20" />
            ) : traceHealth.error ? (
              <p className="text-sm text-muted-foreground">Unavailable</p>
            ) : (
              <div className="flex items-center gap-3">
                <ProgressRing
                  value={completeness}
                  className={`h-10 w-10 ${healthConfig.ring}`}
                />
                <div>
                  <div className={`text-lg font-semibold leading-tight ${healthConfig.color}`}>
                    {healthConfig.label}
                  </div>
                  <p className="text-[11px] text-muted-foreground">
                    {(completeness * 100).toFixed(0)}% complete
                  </p>
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* Coverage tips */}
      {traceHealth.data?.gaps && traceHealth.data.gaps.length > 0 && (
        <Card className="border-amber-500/20 bg-amber-500/[0.03]">
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-xs font-medium uppercase tracking-wider text-muted-foreground">
              <Lightbulb className="h-3.5 w-3.5 text-amber-500/80" />
              Coverage Tips
              <span className="ml-auto text-[11px] font-normal normal-case tracking-normal text-muted-foreground/60">
                {traceHealth.data.gaps.length} suggestion{traceHealth.data.gaps.length !== 1 ? "s" : ""}
              </span>
            </CardTitle>
          </CardHeader>
          <CardContent className="pt-0">
            <ul className="space-y-1.5">
              {traceHealth.data.gaps.map((gap, i) => (
                <li
                  key={i}
                  className="flex items-start gap-2.5 rounded-md px-3 py-2 text-sm bg-muted/40 border border-border/50"
                >
                  <Info className="h-3.5 w-3.5 shrink-0 text-amber-500/70 mt-0.5" />
                  <span className="text-muted-foreground leading-snug">{gap}</span>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      )}

      {/* Recent activity */}
      <Card>
        <CardHeader>
          <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">Recent Decisions</CardTitle>
        </CardHeader>
        <CardContent>
          {recent.isPending ? (
            <div className="space-y-3">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </div>
          ) : !recent.data?.decisions?.length ? (
            <div className="flex flex-col items-center py-14 text-center">
              <div className="relative mb-4">
                <div className="absolute inset-0 rounded-full bg-primary/10 blur-xl" />
                <FileText className="relative h-10 w-10 text-primary/30" />
              </div>
              <p className="text-sm font-medium text-muted-foreground">
                No decisions recorded yet.
              </p>
              <p className="text-xs text-muted-foreground/50 mt-1 max-w-[220px]">
                Call <code className="font-mono bg-muted px-1 rounded text-[11px]">akashi_trace</code> from any agent to start the audit trail.
              </p>
            </div>
          ) : (
            <div className="space-y-1.5">
              {recent.data.decisions.map((d) => (
                <Link
                  key={d.id}
                  to={`/decisions/${d.run_id}`}
                  className="animate-list-item flex items-center justify-between rounded-lg border border-border/60 p-3 text-sm transition-all duration-200 hover:bg-accent/50 hover:shadow-glow-sm hover:border-primary/20"
                >
                  <div className="flex items-center gap-3 min-w-0">
                    <Badge variant="outline" className="font-mono text-[11px] shrink-0 px-2 py-0.5">
                      {d.agent_id}
                    </Badge>
                    <span className="truncate text-foreground/80">
                      {d.outcome}
                    </span>
                  </div>
                  <div className="flex items-center gap-3 text-muted-foreground shrink-0 ml-4">
                    <Badge variant={decisionTypeBadgeVariant(d.decision_type)} className="text-[11px]">{d.decision_type}</Badge>
                    <span className="text-[11px] whitespace-nowrap tabular-nums">
                      {formatRelativeTime(d.created_at)}
                    </span>
                  </div>
                </Link>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
