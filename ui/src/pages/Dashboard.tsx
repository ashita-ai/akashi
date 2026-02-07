import { useQuery } from "@tanstack/react-query";
import { getUsage, getRecentDecisions, listAgents } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { formatRelativeTime, percentOf } from "@/lib/utils";
import { FileText, Users, AlertTriangle, Gauge } from "lucide-react";
import { Link } from "react-router";
import { listConflicts } from "@/lib/api";

function UsageGauge({ used, limit }: { used: number; limit: number }) {
  const unlimited = limit === 0;
  const pct = unlimited ? 0 : percentOf(used, limit);
  const color =
    pct >= 90
      ? "bg-destructive"
      : pct >= 70
        ? "bg-amber-500"
        : "bg-emerald-500";
  return (
    <div className="space-y-2">
      <div className="flex justify-between text-sm">
        <span>
          {unlimited
            ? `${used.toLocaleString()} / Unlimited`
            : `${used.toLocaleString()} / ${limit.toLocaleString()}`}
        </span>
        {!unlimited && (
          <span className="text-muted-foreground">{pct}%</span>
        )}
      </div>
      {!unlimited && (
        <div className="h-2 w-full rounded-full bg-secondary">
          <div
            className={`h-full rounded-full transition-all ${color}`}
            style={{ width: `${pct}%` }}
          />
        </div>
      )}
    </div>
  );
}

export default function Dashboard() {
  const usage = useQuery({
    queryKey: ["dashboard", "usage"],
    queryFn: getUsage,
  });
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
            {usage.isPending ? (
              <Skeleton className="h-8 w-20" />
            ) : (
              <div className="text-2xl font-bold">
                {(usage.data?.decision_count ?? 0).toLocaleString()}
              </div>
            )}
            <p className="text-xs text-muted-foreground">this period</p>
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
            <p className="text-xs text-muted-foreground">
              limit: {usage.data?.agent_limit === 0 ? "Unlimited" : (usage.data?.agent_limit ?? "\u2014")}
            </p>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Conflicts</CardTitle>
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

        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Plan</CardTitle>
            <Gauge className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {usage.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <Badge variant="secondary" className="text-base font-bold">
                {usage.data?.plan ?? "free"}
              </Badge>
            )}
            <p className="text-xs text-muted-foreground">current tier</p>
          </CardContent>
        </Card>
      </div>

      {/* Usage gauge */}
      {usage.data && (
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Decision Usage</CardTitle>
          </CardHeader>
          <CardContent>
            <UsageGauge
              used={usage.data.decision_count}
              limit={usage.data.decision_limit}
            />
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
