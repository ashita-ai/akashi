import { useQuery } from "@tanstack/react-query";
import { getRecentDecisions, listAgents, getTraceHealth } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
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
    <div className="space-y-6">
      <h1 className="text-2xl font-bold tracking-tight">Dashboard</h1>

      {/* Metric cards */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Decisions</CardTitle>
            <FileText className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {recent.isPending ? (
              <Skeleton className="h-8 w-20" />
            ) : (
              <div className="text-2xl font-bold">
                {(recent.data?.total ?? 0).toLocaleString()}
              </div>
            )}
            {traceHealth.data && (
              <p className="text-xs text-muted-foreground">
                {traceHealth.data.completeness.total_decisions} total traced
              </p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Active Agents</CardTitle>
            <Users className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {agents.isPending ? (
              <Skeleton className="h-8 w-12" />
            ) : (
              <div className="text-2xl font-bold">
                {agents.data?.length ?? 0}
              </div>
            )}
            <p className="text-xs text-muted-foreground">registered</p>
          </CardContent>
        </Card>

        <Link to="/conflicts">
          <Card className="transition-colors hover:border-primary/50">
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <CardTitle className="text-sm font-medium">Open Conflicts</CardTitle>
              <AlertTriangle className="h-4 w-4 text-muted-foreground" />
            </CardHeader>
            <CardContent>
              {traceHealth.isPending ? (
                <Skeleton className="h-8 w-12" />
              ) : (
                <div className="text-2xl font-bold">
                  {traceHealth.data?.conflicts?.open ?? 0}
                </div>
              )}
              <p className="text-xs text-muted-foreground">need attention</p>
            </CardContent>
          </Card>
        </Link>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Trace Health</CardTitle>
            <HeartPulse className="h-4 w-4 text-muted-foreground" />
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
                  <div className={`text-lg font-bold leading-tight ${healthConfig.color}`}>
                    {healthConfig.label}
                  </div>
                  <p className="text-xs text-muted-foreground">
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
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm font-medium">
              <Lightbulb className="h-4 w-4 text-muted-foreground" />
              Coverage Tips
            </CardTitle>
          </CardHeader>
          <CardContent>
            <ul className="space-y-2">
              {traceHealth.data.gaps.map((gap, i) => (
                <li
                  key={i}
                  className="flex items-center gap-2 rounded-md bg-muted/50 px-3 py-2 text-sm"
                >
                  <Info className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                  <span className="text-muted-foreground">{gap}</span>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      )}

      {/* Recent activity */}
      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">Recent Decisions</CardTitle>
        </CardHeader>
        <CardContent>
          {recent.isPending ? (
            <div className="space-y-3">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </div>
          ) : !recent.data?.decisions?.length ? (
            <div className="flex flex-col items-center py-12 text-center">
              <FileText className="h-12 w-12 text-muted-foreground/20 mb-3" />
              <p className="text-sm text-muted-foreground">
                No decisions recorded yet.
              </p>
              <p className="text-xs text-muted-foreground/60 mt-1">
                Start tracing with the SDK to see decisions here.
              </p>
            </div>
          ) : (
            <div className="space-y-2">
              {recent.data.decisions.map((d) => (
                <Link
                  key={d.id}
                  to={`/decisions/${d.run_id}`}
                  className="flex items-center justify-between rounded-md border p-3 text-sm transition-colors hover:bg-accent"
                >
                  <div className="flex items-center gap-3 min-w-0">
                    <Badge variant="outline" className="font-mono text-xs shrink-0">
                      {d.agent_id}
                    </Badge>
                    <span className="truncate max-w-[200px]">
                      {d.outcome}
                    </span>
                  </div>
                  <div className="flex items-center gap-3 text-muted-foreground shrink-0">
                    <Badge variant="secondary">{d.decision_type}</Badge>
                    <span className="text-xs whitespace-nowrap">
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
