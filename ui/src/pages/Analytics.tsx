import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import {
  getTraceHealth,
  getConflictAnalytics,
  queryDecisions,
  listAgentsWithStats,
} from "@/lib/api";
import type { AgentWithStats } from "@/lib/api";
import type {
  Decision,
  ConflictTrendPoint,
  DecisionTypeCount,
} from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import {
  Clock,
  GitBranch,
  Quote,
  TrendingDown,
  TrendingUp,
  Users,
  Zap,
} from "lucide-react";

type Period = "7d" | "30d" | "90d";

// ── Helpers ──────────────────────────────────────────────────────────

function pct(n: number, total: number): string {
  if (total === 0) return "0%";
  return `${((n / total) * 100).toFixed(0)}%`;
}

function severityColor(severity: string): string {
  switch (severity) {
    case "critical":
      return "bg-red-500";
    case "high":
      return "bg-amber-500";
    case "medium":
      return "bg-yellow-400";
    case "low":
      return "bg-emerald-500";
    default:
      return "bg-muted-foreground";
  }
}

/** Group decisions by date (YYYY-MM-DD) and compute daily averages. */
function buildDailyStats(
  decisions: Decision[],
): {
  date: string;
  count: number;
  avgCompleteness: number;
}[] {
  const byDate: Record<string, { completeness: number[] }> = {};
  for (const d of decisions) {
    const date = d.created_at.slice(0, 10);
    if (!byDate[date]) byDate[date] = { completeness: [] };
    byDate[date].completeness.push(d.completeness_score);
  }
  return Object.entries(byDate)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([date, vals]) => ({
      date,
      count: vals.completeness.length,
      avgCompleteness:
        vals.completeness.reduce((s, v) => s + v, 0) /
        vals.completeness.length,
    }));
}

// ── Components ───────────────────────────────────────────────────────

function PeriodSelector({
  value,
  onChange,
}: {
  value: Period;
  onChange: (p: Period) => void;
}) {
  const periods: Period[] = ["7d", "30d", "90d"];
  return (
    <div className="flex gap-1 rounded-md border p-0.5">
      {periods.map((p) => (
        <button
          key={p}
          onClick={() => onChange(p)}
          className={cn(
            "rounded px-3 py-1 text-xs font-medium transition-all duration-200",
            p === value
              ? "bg-primary text-primary-foreground shadow-sm"
              : "text-muted-foreground hover:text-foreground hover:bg-accent",
          )}
        >
          {p}
        </button>
      ))}
    </div>
  );
}

/** Horizontal stacked bar, no external chart library. */
function StackedBar({
  segments,
}: {
  segments: { label: string; value: number; color: string }[];
}) {
  const total = segments.reduce((s, seg) => s + seg.value, 0);
  if (total === 0) return <div className="h-8 rounded bg-muted" />;
  return (
    <div className="flex h-8 overflow-hidden rounded-lg shadow-inner">
      {segments.map((seg) =>
        seg.value > 0 ? (
          <div
            key={seg.label}
            className={cn(
              "group/seg relative transition-all duration-500 hover:brightness-110 hover:saturate-150",
              seg.color,
            )}
            style={{ width: `${(seg.value / total) * 100}%` }}
          >
            <span className="absolute inset-0 flex items-center justify-center text-[10px] font-bold text-white opacity-0 group-hover/seg:opacity-100 transition-opacity drop-shadow-sm">
              {seg.value}
            </span>
          </div>
        ) : null,
      )}
    </div>
  );
}

/** Simple CSS-only bar chart for daily trends. */
function TrendChart({
  data,
  label,
  suffix = "",
}: {
  data: { date: string; value: number }[];
  label: string;
  suffix?: string;
}) {
  const max = Math.max(...data.map((d) => d.value), 1);
  return (
    <div className="space-y-1">
      <p className="text-xs font-medium text-muted-foreground">{label}</p>
      <div className="flex items-end gap-[2px] h-24">
        {data.map((d) => (
          <div
            key={d.date}
            className="group/bar relative flex-1 rounded-t transition-all duration-300 bg-gradient-to-t from-primary/60 to-primary hover:from-primary hover:to-primary hover:shadow-[0_-4px_12px_-2px_hsl(var(--glow-blue)/0.4)]"
            style={{ height: `${Math.max((d.value / max) * 100, 2)}%` }}
          >
            <span className="absolute -top-6 left-1/2 -translate-x-1/2 rounded bg-foreground/90 px-1.5 py-0.5 text-[10px] font-semibold text-background opacity-0 group-hover/bar:opacity-100 transition-opacity pointer-events-none whitespace-nowrap shadow-md">
              {d.value}{suffix}
            </span>
          </div>
        ))}
      </div>
      {data.length > 1 && (
        <div className="flex justify-between text-[10px] text-muted-foreground">
          <span>{data[0]!.date.slice(5)}</span>
          <span>{data[data.length - 1]!.date.slice(5)}</span>
        </div>
      )}
    </div>
  );
}

/** Dual-line conflict trend: detected vs resolved. */
function ConflictTrend({ data }: { data: ConflictTrendPoint[] }) {
  const max = Math.max(...data.flatMap((d) => [d.detected, d.resolved]), 1);
  return (
    <div className="space-y-1">
      <div className="flex items-center gap-4 text-xs text-muted-foreground">
        <span className="flex items-center gap-1">
          <span className="inline-block h-2.5 w-2.5 rounded-full bg-gradient-to-br from-red-400 to-red-600 shadow-sm shadow-red-500/30" />
          Detected
        </span>
        <span className="flex items-center gap-1">
          <span className="inline-block h-2.5 w-2.5 rounded-full bg-gradient-to-br from-emerald-400 to-emerald-600 shadow-sm shadow-emerald-500/30" />
          Resolved
        </span>
      </div>
      <div className="flex items-end gap-[2px] h-24">
        {data.map((d) => (
          <div key={d.date} className="group/ct relative flex-1 flex flex-col gap-[1px] justify-end h-full">
            <div
              className="bg-gradient-to-t from-red-500/70 to-red-400 rounded-t transition-all duration-300 group-hover/ct:from-red-500 group-hover/ct:to-red-400 group-hover/ct:shadow-[0_-2px_8px_-1px_rgba(239,68,68,0.4)]"
              style={{
                height: `${Math.max((d.detected / max) * 50, d.detected > 0 ? 2 : 0)}%`,
              }}
            />
            <div
              className="bg-gradient-to-t from-emerald-500/70 to-emerald-400 rounded-t transition-all duration-300 group-hover/ct:from-emerald-500 group-hover/ct:to-emerald-400 group-hover/ct:shadow-[0_-2px_8px_-1px_rgba(16,185,129,0.4)]"
              style={{
                height: `${Math.max((d.resolved / max) * 50, d.resolved > 0 ? 2 : 0)}%`,
              }}
            />
            <span className="absolute -top-6 left-1/2 -translate-x-1/2 rounded bg-foreground/90 px-1.5 py-0.5 text-[10px] font-semibold text-background opacity-0 group-hover/ct:opacity-100 transition-opacity pointer-events-none whitespace-nowrap shadow-md">
              {d.detected}d / {d.resolved}r
            </span>
          </div>
        ))}
      </div>
      {data.length > 1 && (
        <div className="flex justify-between text-[10px] text-muted-foreground">
          <span>{data[0]!.date.slice(5)}</span>
          <span>{data[data.length - 1]!.date.slice(5)}</span>
        </div>
      )}
    </div>
  );
}

/** Horizontal bar chart for decision type distribution. */
function DecisionTypeChart({ data }: { data: DecisionTypeCount[] }) {
  if (data.length === 0) {
    return (
      <p className="text-sm text-muted-foreground py-4 text-center">
        No data yet.
      </p>
    );
  }

  const max = data[0]!.count; // already sorted desc from backend
  const colors = [
    "from-blue-600 to-blue-400",
    "from-emerald-600 to-emerald-400",
    "from-purple-600 to-purple-400",
    "from-amber-600 to-amber-400",
    "from-rose-600 to-rose-400",
    "from-cyan-600 to-cyan-400",
    "from-indigo-600 to-indigo-400",
    "from-teal-600 to-teal-400",
  ];

  return (
    <div className="space-y-2">
      {data.map((dt, i) => (
        <div key={dt.decision_type} className="space-y-1">
          <div className="flex justify-between text-xs">
            <span className="text-muted-foreground font-medium truncate mr-2">
              {dt.decision_type}
            </span>
            <span className="font-semibold tabular-nums shrink-0">
              {dt.count}
            </span>
          </div>
          <div className="h-2 rounded-full bg-muted overflow-hidden">
            <div
              className={cn(
                "h-full rounded-full bg-gradient-to-r transition-all duration-500",
                colors[i % colors.length],
              )}
              style={{
                width: max > 0 ? `${(dt.count / max) * 100}%` : "0%",
              }}
            />
          </div>
        </div>
      ))}
    </div>
  );
}

/** Agent scorecard table, capped at top 10. */
function AgentScorecard({ agents }: { agents: AgentWithStats[] }) {
  const sorted = [...agents]
    .filter((a) => (a.decision_count ?? 0) > 0)
    .sort((a, b) => (b.decision_count ?? 0) - (a.decision_count ?? 0))
    .slice(0, 10);

  if (sorted.length === 0) {
    return (
      <p className="text-sm text-muted-foreground py-4 text-center">
        No agent activity yet.
      </p>
    );
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-xs text-muted-foreground">
            <th className="pb-2 font-medium">Agent</th>
            <th className="pb-2 font-medium text-right">Decisions</th>
            <th className="pb-2 font-medium text-right">Last Active</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((a) => (
            <tr key={a.agent_id} className="border-b border-border/50">
              <td className="py-2">
                <Badge variant="outline" className="font-mono text-xs">
                  {a.agent_id}
                </Badge>
              </td>
              <td className="py-2 text-right">{a.decision_count ?? 0}</td>
              <td className="py-2 text-right text-xs text-muted-foreground">
                {a.last_decision_at
                  ? new Date(a.last_decision_at).toLocaleDateString()
                  : "-"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ── Main Page ────────────────────────────────────────────────────────

const PERIOD_DAYS: Record<Period, number> = { "7d": 7, "30d": 30, "90d": 90 };

function periodToTimeRange(period: Period): { from: string; to: string } {
  const to = new Date();
  const from = new Date(to);
  from.setDate(from.getDate() - PERIOD_DAYS[period]);
  return { from: from.toISOString(), to: to.toISOString() };
}

export default function Analytics() {
  const [period, setPeriod] = useState<Period>("30d");

  const traceHealth = useQuery({
    queryKey: ["analytics", "trace-health"],
    queryFn: getTraceHealth,
    staleTime: 30_000,
  });

  const conflictAnalytics = useQuery({
    queryKey: ["analytics", "conflict-analytics", period],
    queryFn: () => getConflictAnalytics({ period }),
    staleTime: 30_000,
  });

  const decisions = useQuery({
    queryKey: ["analytics", "decisions", period],
    queryFn: () =>
      queryDecisions({
        filters: { time_range: periodToTimeRange(period) },
        order_by: "created_at",
        order_dir: "desc",
        limit: 200,
        offset: 0,
      }),
    staleTime: 60_000,
  });

  const agents = useQuery({
    queryKey: ["analytics", "agents"],
    queryFn: listAgentsWithStats,
    staleTime: 60_000,
  });

  const health = traceHealth.data;
  const analytics = conflictAnalytics.data;
  const decisionList = decisions.data?.decisions ?? [];

  const dailyStats = useMemo(() => buildDailyStats(decisionList), [decisionList]);

  // Outcome signal derived metrics.
  const os = health?.outcome_signals;
  const stabilityPct = os && os.decisions_total > 0
    ? ((os.decisions_total - os.revised_within_48h) / os.decisions_total) * 100
    : null;
  const citationPct = os && os.decisions_total > 0
    ? (os.cited_at_least_once / os.decisions_total) * 100
    : null;
  const mttrHours = analytics?.summary.mean_time_to_resolution_hours;

  return (
    <div className="space-y-8 animate-page">
      <div className="page-header flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Analytics</h1>
          <p className="mt-1 text-sm text-muted-foreground">Conflict trends and decision quality over time</p>
        </div>
        <PeriodSelector value={period} onChange={setPeriod} />
      </div>

      {/* ── Row 1: Outcome Signal Cards ── */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Card className="gradient-border hover:glow-emerald transition-shadow duration-300">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">Stability</CardTitle>
            <Zap className="h-4 w-4 text-emerald-500" />
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <>
                <div className={cn(
                  "text-3xl font-black tabular-nums tracking-tight",
                  stabilityPct !== null && stabilityPct >= 90
                    ? "text-emerald-500"
                    : stabilityPct !== null && stabilityPct >= 70
                      ? "text-amber-500"
                      : "text-foreground",
                )}>
                  {stabilityPct !== null ? `${stabilityPct.toFixed(0)}` : "-"}
                  <span className="text-lg">%</span>
                </div>
                <p className="text-xs text-muted-foreground">
                  not revised within 48h
                </p>
              </>
            )}
          </CardContent>
        </Card>

        <Card className="gradient-border hover:glow-primary transition-shadow duration-300">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">Citation Rate</CardTitle>
            <Quote className="h-4 w-4 text-blue-500" />
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <>
                <div className="text-3xl font-black tabular-nums tracking-tight">
                  {citationPct !== null ? `${citationPct.toFixed(0)}` : "-"}
                  <span className="text-lg">%</span>
                </div>
                <p className="text-xs text-muted-foreground">
                  cited as precedent
                </p>
              </>
            )}
          </CardContent>
        </Card>

        <Card className="gradient-border hover:glow-amber transition-shadow duration-300">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
              MTTR
            </CardTitle>
            <Clock className="h-4 w-4 text-amber-500" />
          </CardHeader>
          <CardContent>
            {conflictAnalytics.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <>
                <div className="text-3xl font-black tabular-nums tracking-tight">
                  {mttrHours != null ? mttrHours.toFixed(1) : "-"}
                  <span className="text-lg text-muted-foreground">h</span>
                </div>
                <p className="text-xs text-muted-foreground">
                  mean time to resolve
                </p>
              </>
            )}
          </CardContent>
        </Card>

        <Card className="gradient-border hover:glow-purple transition-shadow duration-300">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
              Total Decisions
            </CardTitle>
            <GitBranch className="h-4 w-4 text-purple-500" />
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <>
                <div className="text-3xl font-black tabular-nums tracking-tight">
                  {health?.completeness.total_decisions ?? 0}
                </div>
                <p className="text-xs text-muted-foreground">
                  active decisions
                </p>
              </>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 2: Decision Type Distribution + Completeness Trend ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">
              Decision Types
            </CardTitle>
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : (
              <DecisionTypeChart
                data={health?.decision_type_distribution ?? []}
              />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">
              Completeness Trend
            </CardTitle>
          </CardHeader>
          <CardContent>
            {decisions.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : dailyStats.length === 0 ? (
              <p className="text-sm text-muted-foreground py-4 text-center">
                No data yet.
              </p>
            ) : (
              <div className="space-y-4">
                <TrendChart
                  data={dailyStats.map((d) => ({
                    date: d.date,
                    value: Math.round(d.avgCompleteness * 100),
                  }))}
                  label="Avg completeness % by day"
                  suffix="%"
                />
                {dailyStats.length >= 2 && (
                  <div className="flex items-center gap-2 text-xs">
                    {dailyStats[dailyStats.length - 1]!.avgCompleteness >=
                    dailyStats[0]!.avgCompleteness ? (
                      <>
                        <TrendingUp className="h-3 w-3 text-emerald-500" />
                        <span className="text-emerald-500">Improving</span>
                      </>
                    ) : (
                      <>
                        <TrendingDown className="h-3 w-3 text-amber-500" />
                        <span className="text-amber-500">Declining</span>
                      </>
                    )}
                    <span className="text-muted-foreground">
                      {dailyStats[0]!.date.slice(5)} to{" "}
                      {dailyStats[dailyStats.length - 1]!.date.slice(5)}
                    </span>
                  </div>
                )}
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 3: Decision Volume + Conflict Trend ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">
              Decision Volume
            </CardTitle>
          </CardHeader>
          <CardContent>
            {decisions.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : dailyStats.length === 0 ? (
              <p className="text-sm text-muted-foreground py-4 text-center">
                No data yet.
              </p>
            ) : (
              <TrendChart
                data={dailyStats.map((d) => ({
                  date: d.date,
                  value: d.count,
                }))}
                label="Decisions per day"
              />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">
              Conflict Trend
            </CardTitle>
          </CardHeader>
          <CardContent>
            {conflictAnalytics.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : !analytics?.trend?.length ? (
              <p className="text-sm text-muted-foreground py-4 text-center">
                No conflict data yet.
              </p>
            ) : (
              <ConflictTrend data={analytics.trend} />
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 4: Conflict Breakdown (severity + agent pairs) ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">
              Conflicts by Severity
            </CardTitle>
          </CardHeader>
          <CardContent>
            {conflictAnalytics.isPending ? (
              <Skeleton className="h-24 w-full" />
            ) : !analytics?.by_severity?.length ? (
              <p className="text-sm text-muted-foreground py-4 text-center">
                No conflicts detected.
              </p>
            ) : (
              <div className="space-y-3">
                <StackedBar
                  segments={analytics.by_severity.map((s) => ({
                    label: s.severity,
                    value: s.count,
                    color: severityColor(s.severity),
                  }))}
                />
                <div className="flex flex-wrap gap-3 text-xs">
                  {analytics.by_severity.map((s) => (
                    <span key={s.severity} className="flex items-center gap-1">
                      <span
                        className={cn(
                          "inline-block h-2 w-2 rounded-full",
                          severityColor(s.severity),
                        )}
                      />
                      {s.severity}: {s.count}
                    </span>
                  ))}
                </div>
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Users className="h-4 w-4 text-muted-foreground" />
              Conflicting Agent Pairs
            </CardTitle>
          </CardHeader>
          <CardContent>
            {conflictAnalytics.isPending ? (
              <Skeleton className="h-24 w-full" />
            ) : !analytics?.by_agent_pair?.length ? (
              <p className="text-sm text-muted-foreground py-4 text-center">
                No agent pair conflicts.
              </p>
            ) : (
              <div className="space-y-2">
                {analytics.by_agent_pair
                  .sort((a, b) => b.count - a.count)
                  .slice(0, 8)
                  .map((pair) => (
                    <div
                      key={`${pair.agent_a}-${pair.agent_b}`}
                      className="flex items-center justify-between rounded-lg border px-3 py-2.5 transition-all duration-200 hover:bg-accent/50 hover:shadow-sm"
                    >
                      <div className="flex items-center gap-2">
                        <Badge
                          variant="outline"
                          className="font-mono text-xs"
                        >
                          {pair.agent_a}
                        </Badge>
                        <span className="text-xs text-muted-foreground">
                          vs
                        </span>
                        <Badge
                          variant="outline"
                          className="font-mono text-xs"
                        >
                          {pair.agent_b}
                        </Badge>
                      </div>
                      <div className="flex items-center gap-3 text-xs">
                        <span
                          className={cn(
                            pair.open > 0
                              ? "text-amber-500"
                              : "text-muted-foreground",
                          )}
                        >
                          {pair.open} open
                        </span>
                        <span className="text-emerald-500">
                          {pair.resolved} resolved
                        </span>
                        <span className="text-muted-foreground font-medium">
                          {pair.count} total
                        </span>
                      </div>
                    </div>
                  ))}
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 5: Trace Quality Breakdown + Agent Scorecard ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">
              Trace Quality Breakdown
            </CardTitle>
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : !health ? (
              <p className="text-sm text-muted-foreground py-4 text-center">
                Unavailable.
              </p>
            ) : (
              <div className="space-y-3">
                {[
                  {
                    label: "With reasoning",
                    value: health.completeness.with_reasoning,
                    total: health.completeness.total_decisions,
                    color: "bg-gradient-to-r from-emerald-600 to-emerald-400",
                    glow: "shadow-emerald-500/30",
                  },
                  {
                    label: "With alternatives",
                    value: health.completeness.with_alternatives,
                    total: health.completeness.total_decisions,
                    color: "bg-gradient-to-r from-blue-600 to-blue-400",
                    glow: "shadow-blue-500/30",
                  },
                  {
                    label: "With evidence",
                    value: health.evidence.with_evidence,
                    total: health.evidence.total_decisions,
                    color: "bg-gradient-to-r from-purple-600 to-purple-400",
                    glow: "shadow-purple-500/30",
                  },
                ].map((item) => (
                  <div key={item.label} className="space-y-1.5">
                    <div className="flex justify-between text-xs">
                      <span className="text-muted-foreground font-medium">
                        {item.label}
                      </span>
                      <span className="font-semibold tabular-nums">
                        {item.value}/{item.total} ({pct(item.value, item.total)})
                      </span>
                    </div>
                    <div className="h-2.5 rounded-full bg-muted overflow-hidden">
                      <div
                        className={cn(
                          "h-full rounded-full progress-fill-animated shadow-sm",
                          item.color,
                          item.glow,
                        )}
                        style={{
                          width:
                            item.total > 0
                              ? `${(item.value / item.total) * 100}%`
                              : "0%",
                        }}
                      />
                    </div>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Users className="h-4 w-4 text-muted-foreground" />
              Agent Scorecard
              <span className="text-xs font-normal text-muted-foreground">(top 10)</span>
            </CardTitle>
          </CardHeader>
          <CardContent>
            {agents.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : (
              <AgentScorecard agents={agents.data ?? []} />
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
