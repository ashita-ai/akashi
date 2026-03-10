import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import {
  getTraceHealth,
  getConflictAnalytics,
  queryDecisions,
  listAgentsWithStats,
} from "@/lib/api";
import type { AgentWithStats } from "@/lib/api";
import type { Decision, ConflictTrendPoint } from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import {
  Activity,
  AlertTriangle,
  BarChart3,
  HeartPulse,
  ShieldAlert,
  TrendingDown,
  TrendingUp,
  Users,
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

function confidenceBucket(c: number): string {
  if (c < 0.3) return "0.0-0.3";
  if (c < 0.5) return "0.3-0.5";
  if (c < 0.7) return "0.5-0.7";
  if (c < 0.9) return "0.7-0.9";
  return "0.9-1.0";
}

const BUCKET_ORDER = ["0.0-0.3", "0.3-0.5", "0.5-0.7", "0.7-0.9", "0.9-1.0"];

const BUCKET_COLORS: Record<string, string> = {
  "0.0-0.3": "bg-red-500",
  "0.3-0.5": "bg-amber-500",
  "0.5-0.7": "bg-yellow-400",
  "0.7-0.9": "bg-emerald-400",
  "0.9-1.0": "bg-blue-500",
};

/** Compute confidence histogram from a list of decisions. */
function buildConfidenceHistogram(
  decisions: Decision[],
): { bucket: string; count: number }[] {
  const counts: Record<string, number> = {};
  for (const b of BUCKET_ORDER) counts[b] = 0;
  for (const d of decisions) {
    const bucket = confidenceBucket(d.confidence);
    counts[bucket] = (counts[bucket] ?? 0) + 1;
  }
  return BUCKET_ORDER.map((b) => ({ bucket: b, count: counts[b] ?? 0 }));
}

/** Group decisions by date (YYYY-MM-DD) and compute daily averages. */
function buildDailyStats(
  decisions: Decision[],
): {
  date: string;
  count: number;
  avgConfidence: number;
  avgCompleteness: number;
}[] {
  const byDate: Record<
    string,
    { confidences: number[]; completeness: number[] }
  > = {};
  for (const d of decisions) {
    const date = d.created_at.slice(0, 10);
    if (!byDate[date])
      byDate[date] = { confidences: [], completeness: [] };
    byDate[date].confidences.push(d.confidence);
    byDate[date].completeness.push(d.completeness_score);
  }
  return Object.entries(byDate)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([date, vals]) => ({
      date,
      count: vals.confidences.length,
      avgConfidence:
        vals.confidences.reduce((s, v) => s + v, 0) / vals.confidences.length,
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
  if (total === 0) return <div className="h-6 rounded bg-muted" />;
  return (
    <div className="flex h-6 overflow-hidden rounded">
      {segments.map((seg) =>
        seg.value > 0 ? (
          <div
            key={seg.label}
            className={cn("transition-all duration-500", seg.color)}
            style={{ width: `${(seg.value / total) * 100}%` }}
            title={`${seg.label}: ${seg.value}`}
          />
        ) : null,
      )}
    </div>
  );
}

/** Simple CSS-only bar chart for daily trends. */
function TrendChart({
  data,
  label,
}: {
  data: { date: string; value: number }[];
  label: string;
}) {
  const max = Math.max(...data.map((d) => d.value), 1);
  return (
    <div className="space-y-1">
      <p className="text-xs font-medium text-muted-foreground">{label}</p>
      <div className="flex items-end gap-[2px] h-20">
        {data.map((d) => (
          <div
            key={d.date}
            className="flex-1 bg-primary/70 rounded-t transition-all duration-300 hover:bg-primary"
            style={{ height: `${Math.max((d.value / max) * 100, 2)}%` }}
            title={`${d.date}: ${d.value}`}
          />
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
          <span className="inline-block h-2 w-2 rounded-full bg-red-400" />
          Detected
        </span>
        <span className="flex items-center gap-1">
          <span className="inline-block h-2 w-2 rounded-full bg-emerald-400" />
          Resolved
        </span>
      </div>
      <div className="flex items-end gap-[2px] h-20">
        {data.map((d) => (
          <div key={d.date} className="flex-1 flex flex-col gap-[1px] justify-end h-full">
            <div
              className="bg-red-400/80 rounded-t"
              style={{
                height: `${Math.max((d.detected / max) * 50, d.detected > 0 ? 2 : 0)}%`,
              }}
              title={`${d.date} detected: ${d.detected}`}
            />
            <div
              className="bg-emerald-400/80 rounded-t"
              style={{
                height: `${Math.max((d.resolved / max) * 50, d.resolved > 0 ? 2 : 0)}%`,
              }}
              title={`${d.date} resolved: ${d.resolved}`}
            />
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

/** Confidence histogram with ideal-range overlay. */
function ConfidenceHistogram({
  data,
}: {
  data: { bucket: string; count: number }[];
}) {
  const total = data.reduce((s, d) => s + d.count, 0);
  const max = Math.max(...data.map((d) => d.count), 1);
  const overconfident =
    total > 0
      ? data.find((d) => d.bucket === "0.9-1.0")?.count ?? 0
      : 0;
  const overconfidentPct = total > 0 ? (overconfident / total) * 100 : 0;

  return (
    <div className="space-y-3">
      <div className="flex items-end gap-2 h-24">
        {data.map((d) => (
          <div key={d.bucket} className="flex-1 flex flex-col items-center gap-1">
            <span className="text-[10px] text-muted-foreground">{d.count}</span>
            <div
              className={cn(
                "w-full rounded-t transition-all duration-300",
                BUCKET_COLORS[d.bucket],
              )}
              style={{ height: `${Math.max((d.count / max) * 100, d.count > 0 ? 4 : 0)}%` }}
            />
            <span className="text-[10px] text-muted-foreground whitespace-nowrap">
              {d.bucket}
            </span>
          </div>
        ))}
      </div>
      {/* Ideal range indicator */}
      <div className="flex items-center gap-2 text-xs">
        <div className="h-2 flex-1 rounded bg-muted relative">
          {/* Ideal range: 0.4-0.8 → spans buckets 2-3 (0.3-0.5 and 0.5-0.7) */}
          <div
            className="absolute h-full rounded bg-emerald-500/30 border border-emerald-500/50"
            style={{ left: "20%", width: "40%" }}
            title="Ideal range (0.3-0.7)"
          />
        </div>
        <span className="text-muted-foreground shrink-0">ideal range</span>
      </div>
      {overconfidentPct > 50 && (
        <p className="text-xs text-amber-500 flex items-center gap-1">
          <AlertTriangle className="h-3 w-3" />
          {overconfidentPct.toFixed(0)}% of decisions are 0.9+ — likely
          overconfident
        </p>
      )}
    </div>
  );
}

/** Agent scorecard table. */
function AgentScorecard({ agents }: { agents: AgentWithStats[] }) {
  const sorted = [...agents]
    .filter((a) => (a.decision_count ?? 0) > 0)
    .sort((a, b) => (b.decision_count ?? 0) - (a.decision_count ?? 0));

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
            <th className="pb-2 font-medium text-right">Avg Confidence</th>
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
              <td className="py-2 text-right">
                {/* We don't have avg_confidence from listAgentsWithStats directly,
                    so show decision count as the primary metric */}
                -
              </td>
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

  // Fetch recent decisions to compute confidence histogram and daily stats.
  // Scoped to the selected period so charts stay consistent with conflict data.
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

  const histogram = useMemo(() => buildConfidenceHistogram(decisionList), [decisionList]);
  const dailyStats = useMemo(() => buildDailyStats(decisionList), [decisionList]);

  // Compute health score composite (0-100)
  const healthScore = useMemo(
    () =>
      health
        ? Math.round(
            (health.completeness.avg_completeness * 40 +
              (health.completeness.reasoning_pct / 100) * 20 +
              (health.completeness.alternatives_pct / 100) * 15 +
              (health.evidence.coverage_pct / 100) * 15 +
              (health.conflicts?.resolved_pct ?? 0) / 100 * 10),
          )
        : null,
    [health],
  );

  const healthLabel =
    healthScore === null
      ? "Unknown"
      : healthScore >= 70
        ? "Healthy"
        : healthScore >= 40
          ? "Needs Attention"
          : "Critical";

  const healthColor =
    healthScore === null
      ? "text-muted-foreground"
      : healthScore >= 70
        ? "text-emerald-500"
        : healthScore >= 40
          ? "text-amber-500"
          : "text-red-500";

  return (
    <div className="space-y-6 animate-page">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Analytics</h1>
          <p className="mt-0.5 text-sm text-muted-foreground">Conflict trends and decision quality over time</p>
        </div>
        <PeriodSelector value={period} onChange={setPeriod} />
      </div>

      {/* ── Panel 1: Health Score + Summary Cards ── */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Card className="gradient-border">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Health Score</CardTitle>
            <HeartPulse className="h-4 w-4 text-emerald-500/70" />
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <>
                <div className={cn("text-2xl font-bold", healthColor)}>
                  {healthScore ?? "?"}
                  <span className="text-sm font-normal text-muted-foreground">
                    /100
                  </span>
                </div>
                <p className={cn("text-xs", healthColor)}>{healthLabel}</p>
              </>
            )}
          </CardContent>
        </Card>

        <Card className="gradient-border">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Completeness</CardTitle>
            <BarChart3 className="h-4 w-4 text-primary/60" />
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <>
                <div className="text-2xl font-bold">
                  {((health?.completeness.avg_completeness ?? 0) * 100).toFixed(
                    0,
                  )}
                  %
                </div>
                <p className="text-xs text-muted-foreground">
                  {health?.completeness.below_half ?? 0} decisions below 50%
                </p>
              </>
            )}
          </CardContent>
        </Card>

        <Card className="gradient-border">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">
              Conflict Resolution
            </CardTitle>
            <ShieldAlert className="h-4 w-4 text-amber-500/70" />
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <>
                <div className="text-2xl font-bold">
                  {(health?.conflicts?.resolved_pct ?? 0).toFixed(0)}%
                </div>
                <p className="text-xs text-muted-foreground">
                  {health?.conflicts?.open ?? 0} open /{" "}
                  {health?.conflicts?.total ?? 0} total
                </p>
              </>
            )}
          </CardContent>
        </Card>

        <Card className="gradient-border">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">
              Evidence Coverage
            </CardTitle>
            <Activity className="h-4 w-4 text-primary/60" />
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-8 w-16" />
            ) : (
              <>
                <div className="text-2xl font-bold">
                  {(health?.evidence.coverage_pct ?? 0).toFixed(0)}%
                </div>
                <p className="text-xs text-muted-foreground">
                  {health?.evidence.with_evidence ?? 0} of{" "}
                  {health?.evidence.total_decisions ?? 0} with evidence
                </p>
              </>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 2: Confidence Calibration + Completeness Trend ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              Confidence Calibration
              {decisionList.length > 0 && (
                <span className="text-xs font-normal text-muted-foreground">
                  ({decisionList.length} decisions)
                </span>
              )}
            </CardTitle>
          </CardHeader>
          <CardContent>
            {decisions.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : (
              <ConfidenceHistogram data={histogram} />
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
                {analytics.summary.mean_time_to_resolution_hours != null && (
                  <p className="text-xs text-muted-foreground">
                    Mean time to resolution:{" "}
                    {analytics.summary.mean_time_to_resolution_hours.toFixed(1)}h
                  </p>
                )}
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
                      className="flex items-center justify-between rounded-md border px-3 py-2"
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
                    color: "bg-emerald-500",
                  },
                  {
                    label: "With alternatives",
                    value: health.completeness.with_alternatives,
                    total: health.completeness.total_decisions,
                    color: "bg-blue-500",
                  },
                  {
                    label: "With evidence",
                    value: health.evidence.with_evidence,
                    total: health.evidence.total_decisions,
                    color: "bg-purple-500",
                  },
                ].map((item) => (
                  <div key={item.label} className="space-y-1">
                    <div className="flex justify-between text-xs">
                      <span className="text-muted-foreground">
                        {item.label}
                      </span>
                      <span>
                        {item.value}/{item.total} ({pct(item.value, item.total)})
                      </span>
                    </div>
                    <div className="h-2 rounded bg-muted">
                      <div
                        className={cn(
                          "h-full rounded transition-all duration-500",
                          item.color,
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

      {/* ── Row 6: Gaps / Recommendations ── */}
      {health?.gaps && health.gaps.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <AlertTriangle className="h-4 w-4 text-amber-500" />
              Improvement Recommendations
            </CardTitle>
          </CardHeader>
          <CardContent>
            <ul className="space-y-2">
              {health.gaps.map((gap, i) => (
                <li
                  key={i}
                  className="rounded-md bg-amber-500/5 border border-amber-500/20 px-3 py-2 text-sm text-muted-foreground"
                >
                  {gap}
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
