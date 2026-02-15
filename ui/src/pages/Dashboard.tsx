import { useQuery } from "@tanstack/react-query";
import { getRecentDecisions, listAgents, listConflicts, getTraceHealth } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { formatRelativeTime } from "@/lib/utils";
import {
  Activity,
  AlertTriangle,
  FileText,
  HeartPulse,
  Users,
} from "lucide-react";
import { Link } from "react-router";

const healthStatusConfig: Record<string, { label: string; color: string }> = {
  healthy: { label: "Healthy", color: "text-emerald-500" },
  degraded: { label: "Degraded", color: "text-amber-500" },
  unhealthy: { label: "Unhealthy", color: "text-red-500" },
};

export default function Dashboard() {
  const recent = useQuery({
    queryKey: ["dashboard", "recent"],
    queryFn: () => getRecentDecisions({ limit: 10 }),
  });
  const agents = useQuery({
    queryKey: ["dashboard", "agents"],
    queryFn: listAgents,
  });
  const conflicts = useQuery({
    queryKey: ["dashboard", "conflicts"],
    queryFn: () => listConflicts({ limit: 1 }),
  });
  const traceHealth = useQuery({
    queryKey: ["dashboard", "trace-health"],
    queryFn: getTraceHealth,
    staleTime: 30_000,
  });

  const healthConfig = healthStatusConfig[traceHealth.data?.status ?? ""] ?? { label: "Unknown", color: "text-muted-foreground" };

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
                {traceHealth.data.decisions_24h} in last 24h
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
            {traceHealth.data && traceHealth.data.active_agents > 0 && (
              <p className="text-xs text-muted-foreground">
                {traceHealth.data.active_agents} active recently
              </p>
            )}
          </CardContent>
        </Card>

        <Link to="/conflicts">
          <Card className="transition-colors hover:border-primary/50">
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <CardTitle className="text-sm font-medium">Open Conflicts</CardTitle>
              <AlertTriangle className="h-4 w-4 text-muted-foreground" />
            </CardHeader>
            <CardContent>
              {conflicts.isPending ? (
                <Skeleton className="h-8 w-12" />
              ) : (
                <div className="text-2xl font-bold">
                  {conflicts.data?.total ?? 0}
                </div>
              )}
              <p className="text-xs text-muted-foreground">detected</p>
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
              <>
                <div className={`text-2xl font-bold ${healthConfig.color}`}>
                  {healthConfig.label}
                </div>
                <p className="text-xs text-muted-foreground">
                  avg confidence: {((traceHealth.data?.avg_confidence ?? 0) * 100).toFixed(0)}%
                </p>
              </>
            )}
          </CardContent>
        </Card>
      </div>

      {/* Trace health gaps */}
      {traceHealth.data?.gaps && traceHealth.data.gaps.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm font-medium">
              <Activity className="h-4 w-4 text-amber-500" />
              Trace Gaps
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-2">
              {traceHealth.data.gaps.map((gap) => (
                <div
                  key={gap.agent_id}
                  className="flex items-center justify-between rounded-md border p-3 text-sm"
                >
                  <div className="flex items-center gap-2">
                    <Badge variant="outline" className="font-mono text-xs">
                      {gap.agent_id}
                    </Badge>
                    <span className="text-muted-foreground">
                      last seen {formatRelativeTime(gap.last_seen)}
                    </span>
                  </div>
                  <Badge variant="warning" className="text-xs">
                    {gap.gap_hours.toFixed(0)}h gap
                  </Badge>
                </div>
              ))}
            </div>
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
            <p className="text-sm text-muted-foreground">
              No decisions recorded yet.
            </p>
          ) : (
            <div className="space-y-2">
              {recent.data.decisions.map((d) => (
                <Link
                  key={d.id}
                  to={`/decisions/${d.run_id}`}
                  className="flex items-center justify-between rounded-md border p-3 text-sm transition-colors hover:bg-accent"
                >
                  <div className="flex items-center gap-3">
                    <Badge variant="outline" className="font-mono text-xs">
                      {d.agent_id}
                    </Badge>
                    <span className="truncate max-w-[200px]">
                      {d.outcome}
                    </span>
                  </div>
                  <div className="flex items-center gap-3 text-muted-foreground">
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
