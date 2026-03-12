import { useQuery } from "@tanstack/react-query";
import { useState, useMemo } from "react";
import { Link, useSearchParams } from "react-router";
import { getDecisionTimeline, listAgentsWithStats } from "@/lib/api";
import type { TimelineBucket, TimelineDecision } from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { cn, truncate } from "@/lib/utils";
import {
  Calendar,
  ChevronRight,
  AlertTriangle,
  FileText,
  Filter,
} from "lucide-react";

type Granularity = "day" | "week";

function GranularityToggle({
  value,
  onChange,
}: {
  value: Granularity;
  onChange: (g: Granularity) => void;
}) {
  const options: Granularity[] = ["day", "week"];
  return (
    <div className="flex gap-1 rounded-md border p-0.5">
      {options.map((g) => (
        <button
          key={g}
          onClick={() => onChange(g)}
          className={cn(
            "rounded px-3 py-1 text-xs font-medium transition-all duration-200 capitalize",
            g === value
              ? "bg-primary text-primary-foreground shadow-sm"
              : "text-muted-foreground hover:text-foreground hover:bg-accent",
          )}
        >
          {g === "day" ? "Daily" : "Weekly"}
        </button>
      ))}
    </div>
  );
}

function formatBucketDate(bucket: string, granularity: string): string {
  const date = new Date(bucket + "T00:00:00Z");
  if (granularity === "week") {
    const end = new Date(date);
    end.setDate(end.getDate() + 6);
    return `${date.toLocaleDateString("en-US", { month: "short", day: "numeric" })} - ${end.toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric" })}`;
  }
  return date.toLocaleDateString("en-US", {
    weekday: "short",
    month: "short",
    day: "numeric",
    year: "numeric",
  });
}

function TypeBreakdown({ types }: { types: Record<string, number> }) {
  const sorted = Object.entries(types).sort(([, a], [, b]) => b - a);
  if (sorted.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-1.5">
      {sorted.map(([type, count]) => (
        <Badge
          key={type}
          variant={decisionTypeBadgeVariant(type)}
          className="text-[10px] px-1.5 py-0"
        >
          {type.replace(/_/g, " ")} ({count})
        </Badge>
      ))}
    </div>
  );
}

function AgentBreakdown({ agents }: { agents: Record<string, number> }) {
  const sorted = Object.entries(agents).sort(([, a], [, b]) => b - a);
  if (sorted.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-1.5">
      {sorted.map(([agent, count]) => (
        <span
          key={agent}
          className="inline-flex items-center gap-1 text-xs text-muted-foreground"
        >
          <Badge variant="outline" className="text-[10px] font-mono px-1.5 py-0">
            {agent}
          </Badge>
          <span className="text-[10px]">{count}</span>
        </span>
      ))}
    </div>
  );
}

function TopDecisionRow({ d }: { d: TimelineDecision }) {
  return (
    <Link
      to={`/decisions/${d.id}`}
      className="group flex items-start gap-3 rounded-md border border-border/50 px-3 py-2 transition-all hover:border-border hover:bg-accent/50"
    >
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 mb-0.5">
          <Badge
            variant={decisionTypeBadgeVariant(d.decision_type)}
            className="text-[10px] px-1.5 py-0 shrink-0"
          >
            {d.decision_type.replace(/_/g, " ")}
          </Badge>
          <Badge variant="outline" className="text-[10px] font-mono px-1.5 py-0 shrink-0">
            {d.agent_id}
          </Badge>
          {d.project && (
            <span className="text-[10px] text-muted-foreground truncate">
              {d.project}
            </span>
          )}
          <span className="ml-auto text-[10px] text-muted-foreground shrink-0">
            {(d.confidence * 100).toFixed(0)}%
          </span>
        </div>
        <p className="text-sm text-foreground/80 leading-snug">
          {truncate(d.outcome, 180)}
        </p>
      </div>
      <ChevronRight className="h-4 w-4 text-muted-foreground shrink-0 mt-1 opacity-0 group-hover:opacity-100 transition-opacity" />
    </Link>
  );
}

function BucketCard({
  bucket,
  granularity,
}: {
  bucket: TimelineBucket;
  granularity: string;
}) {
  return (
    <Card className="gradient-border">
      <CardHeader className="pb-3">
        <div className="flex items-center justify-between">
          <CardTitle className="text-sm font-medium flex items-center gap-2">
            <Calendar className="h-4 w-4 text-primary/60" />
            {formatBucketDate(bucket.bucket, granularity)}
          </CardTitle>
          <div className="flex items-center gap-3 text-xs text-muted-foreground">
            <span className="flex items-center gap-1">
              <FileText className="h-3 w-3" />
              {bucket.decision_count}
            </span>
            <span>
              avg {(bucket.avg_confidence * 100).toFixed(0)}%
            </span>
            {bucket.conflict_count > 0 && (
              <span className="flex items-center gap-1 text-amber-500">
                <AlertTriangle className="h-3 w-3" />
                {bucket.conflict_count}
              </span>
            )}
          </div>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="space-y-2">
          <TypeBreakdown types={bucket.decision_types} />
          <AgentBreakdown agents={bucket.agents} />
        </div>
        {bucket.top_decisions.length > 0 && (
          <div className="space-y-1.5 pt-1 border-t border-border/50">
            <p className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider">
              Key decisions
            </p>
            {bucket.top_decisions.map((d) => (
              <TopDecisionRow key={d.id} d={d} />
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function SummaryStats({ buckets }: { buckets: TimelineBucket[] }) {
  const stats = useMemo(() => {
    const totalDecisions = buckets.reduce((s, b) => s + b.decision_count, 0);
    const totalConflicts = buckets.reduce((s, b) => s + b.conflict_count, 0);
    const weightedConf = buckets.reduce(
      (s, b) => s + b.avg_confidence * b.decision_count,
      0,
    );
    const avgConf = totalDecisions > 0 ? weightedConf / totalDecisions : 0;
    const uniqueAgents = new Set(
      buckets.flatMap((b) => Object.keys(b.agents)),
    );
    return { totalDecisions, totalConflicts, avgConf, agentCount: uniqueAgents.size };
  }, [buckets]);

  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <Card className="gradient-border">
        <CardContent className="pt-4 pb-3">
          <p className="text-xs text-muted-foreground">Total Decisions</p>
          <p className="text-2xl font-bold">{stats.totalDecisions}</p>
        </CardContent>
      </Card>
      <Card className="gradient-border">
        <CardContent className="pt-4 pb-3">
          <p className="text-xs text-muted-foreground">Avg Confidence</p>
          <p className="text-2xl font-bold">
            {(stats.avgConf * 100).toFixed(0)}%
          </p>
        </CardContent>
      </Card>
      <Card className="gradient-border">
        <CardContent className="pt-4 pb-3">
          <p className="text-xs text-muted-foreground">Active Agents</p>
          <p className="text-2xl font-bold">{stats.agentCount}</p>
        </CardContent>
      </Card>
      <Card className="gradient-border">
        <CardContent className="pt-4 pb-3">
          <p className="text-xs text-muted-foreground">Conflicts</p>
          <p className={cn("text-2xl font-bold", stats.totalConflicts > 0 ? "text-amber-500" : "")}>
            {stats.totalConflicts}
          </p>
        </CardContent>
      </Card>
    </div>
  );
}

export default function Timeline() {
  const [searchParams, setSearchParams] = useSearchParams();
  const granularity = (searchParams.get("granularity") ?? "day") as Granularity;
  const agentFilter = searchParams.get("agent") ?? "";
  const projectFilter = searchParams.get("project") ?? "";

  const [periodDays] = useState(30);

  const timeRange = useMemo(() => {
    const to = new Date();
    const from = new Date(to);
    from.setDate(from.getDate() - periodDays);
    return { from: from.toISOString(), to: to.toISOString() };
  }, [periodDays]);

  const timeline = useQuery({
    queryKey: ["timeline", granularity, agentFilter, projectFilter, timeRange.from],
    queryFn: () =>
      getDecisionTimeline({
        granularity,
        agent_id: agentFilter || undefined,
        project: projectFilter || undefined,
        from: timeRange.from,
        to: timeRange.to,
      }),
    staleTime: 30_000,
  });

  const agents = useQuery({
    queryKey: ["timeline", "agents"],
    queryFn: listAgentsWithStats,
    staleTime: 60_000,
  });

  const buckets = timeline.data?.buckets ?? [];
  const projects = timeline.data?.projects ?? [];
  const agentList = agents.data ?? [];

  function updateParams(updates: Record<string, string>) {
    const params: Record<string, string> = {};
    if (granularity !== "day") params.granularity = granularity;
    if (agentFilter) params.agent = agentFilter;
    if (projectFilter) params.project = projectFilter;
    for (const [k, v] of Object.entries(updates)) {
      if (v) {
        params[k] = v;
      } else {
        delete params[k];
      }
    }
    setSearchParams(params);
  }

  return (
    <div className="space-y-6 animate-page">
      <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Decision Timeline</h1>
          <p className="mt-0.5 text-sm text-muted-foreground">
            Executive summary of decisions across your organization
          </p>
        </div>
        <GranularityToggle
          value={granularity}
          onChange={(g) => updateParams({ granularity: g === "day" ? "" : g })}
        />
      </div>

      {/* Filters */}
      <div className="flex flex-wrap items-center gap-3">
        <Filter className="h-4 w-4 text-muted-foreground" />
        <select
          value={projectFilter}
          onChange={(e) => updateParams({ project: e.target.value })}
          className="rounded-md border bg-background px-3 py-1.5 text-sm"
        >
          <option value="">All projects</option>
          {projects.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
        <select
          value={agentFilter}
          onChange={(e) => updateParams({ agent: e.target.value })}
          className="rounded-md border bg-background px-3 py-1.5 text-sm"
        >
          <option value="">All agents</option>
          {agentList.map((a) => (
            <option key={a.agent_id} value={a.agent_id}>
              {a.agent_id}
            </option>
          ))}
        </select>
        {(agentFilter || projectFilter) && (
          <button
            onClick={() => updateParams({ agent: "", project: "" })}
            className="text-xs text-muted-foreground hover:text-foreground transition-colors"
          >
            Clear filters
          </button>
        )}
      </div>

      {timeline.isPending ? (
        <div className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            {Array.from({ length: 4 }).map((_, i) => (
              <Skeleton key={i} className="h-20" />
            ))}
          </div>
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-48" />
          ))}
        </div>
      ) : buckets.length === 0 ? (
        <div className="flex flex-col items-center justify-center py-16">
          <Calendar className="h-12 w-12 text-muted-foreground/40 mb-3" />
          <p className="text-sm text-muted-foreground">
            No decisions found for this period
          </p>
          <p className="text-xs text-muted-foreground mt-1">
            Try adjusting your filters or check back later
          </p>
        </div>
      ) : (
        <>
          <SummaryStats buckets={buckets} />
          <div className="space-y-4">
            {buckets.map((bucket) => (
              <BucketCard
                key={bucket.bucket}
                bucket={bucket}
                granularity={granularity}
              />
            ))}
          </div>
        </>
      )}
    </div>
  );
}
