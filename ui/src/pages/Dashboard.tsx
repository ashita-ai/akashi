import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import {
  getRecentDecisions,
  listAgents,
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
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { cn, formatRelativeTime } from "@/lib/utils";
import {
  AlertTriangle,
  Clock,
  FileText,
  HeartPulse,
  Info,
  Lightbulb,
  Quote,
  TrendingDown,
  TrendingUp,
  Users,
  Zap,
} from "lucide-react";
import { Link } from "react-router";

// ── Types & Constants ────────────────────────────────────────────────

type Period = "7d" | "30d" | "90d";

const PERIOD_DAYS: Record<Period, number> = { "7d": 7, "30d": 30, "90d": 90 };

const healthStatusConfig: Record<string, { label: string; color: string; ring: string }> = {
  healthy: { label: "Healthy", color: "text-emerald-500", ring: "stroke-emerald-500" },
  needs_attention: { label: "Needs Attention", color: "text-amber-500", ring: "stroke-amber-500" },
  insufficient_data: { label: "No Data", color: "text-muted-foreground", ring: "stroke-muted-foreground" },
};

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

function periodToTimeRange(period: Period): { from: string; to: string } {
  const to = new Date();
  const from = new Date(to);
  from.setDate(from.getDate() - PERIOD_DAYS[period]);
  return { from: from.toISOString(), to: to.toISOString() };
}

/** Group decisions by date (YYYY-MM-DD) and compute daily averages. */
function buildDailyStats(
  decisions: Decision[],
): { date: string; count: number; avgCompleteness: number }[] {
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
        vals.completeness.reduce((s, v) => s + v, 0) / vals.completeness.length,
    }));
}

// ── Sub-components ───────────────────────────────────────────────────

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

  const max = data[0]!.count;
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

  const visible = data.slice(0, 5);
  const overflow = data.length - visible.length;

  return (
    <div className="space-y-2">
      {visible.map((dt, i) => (
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
      {overflow > 0 && (
        <p className="text-xs text-muted-foreground pt-1">
          +{overflow} more
        </p>
      )}
    </div>
  );
}

/** Agent scorecard table. */
function AgentScorecard({ agents }: { agents: AgentWithStats[] }) {
  const active = [...agents]
    .filter((a) => (a.decision_count ?? 0) > 0)
    .sort((a, b) => (b.decision_count ?? 0) - (a.decision_count ?? 0));
  const sorted = active.slice(0, 5);
  const overflow = active.length - sorted.length;

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
      {overflow > 0 && (
        <p className="text-xs text-muted-foreground pt-2">
          +{overflow} more
        </p>
      )}
    </div>
  );
}

// ── Main Page ────────────────────────────────────────────────────────

export default function Dashboard() {
  const [period, setPeriod] = useState<Period>("30d");

  const recent = useQuery({
    queryKey: ["dashboard", "recent"],
    queryFn: () => getRecentDecisions({ limit: 5 }),
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
  const conflictAnalytics = useQuery({
    queryKey: ["dashboard", "conflict-analytics", period],
    queryFn: () => getConflictAnalytics({ period }),
    staleTime: 30_000,
  });
  const decisionsTrend = useQuery({
    queryKey: ["dashboard", "decisions-trend", period],
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
  const agentsWithStats = useQuery({
    queryKey: ["dashboard", "agents-stats"],
    queryFn: listAgentsWithStats,
    staleTime: 60_000,
  });

  const healthConfig = healthStatusConfig[traceHealth.data?.status ?? ""] ?? {
    label: "Unknown",
    color: "text-muted-foreground",
    ring: "stroke-muted-foreground",
  };
  const completeness = traceHealth.data?.completeness.avg_completeness ?? 0;

  const health = traceHealth.data;
  const analytics = conflictAnalytics.data;
  const decisionList = decisionsTrend.data?.decisions ?? [];
  const dailyStats = useMemo(() => buildDailyStats(decisionList), [decisionList]);

  const os = health?.outcome_signals;
  const stabilityPct =
    os && os.decisions_total > 0
      ? ((os.decisions_total - os.revised_within_48h) / os.decisions_total) * 100
      : null;
  const citationPct =
    os && os.decisions_total > 0
      ? (os.cited_at_least_once / os.decisions_total) * 100
      : null;
  const mttrHours = analytics?.summary.mean_time_to_resolution_hours;

  return (
    <div className="space-y-8 animate-page">
      <div className="page-header flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Dashboard</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Decision audit trail and agent coordination health
          </p>
        </div>
        <div className="flex items-center gap-3 shrink-0">
          <PeriodSelector value={period} onChange={setPeriod} />
          {traceHealth.data && (
            <div className="flex items-center gap-2 rounded-full border px-3 py-1.5 text-[11px] font-medium text-muted-foreground bg-muted/30 uppercase tracking-wider">
              <span className={`h-1.5 w-1.5 rounded-full ${healthConfig.color.replace("text-", "bg-")}`} />
              {healthConfig.label}
            </div>
          )}
        </div>
      </div>

      {/* ── Metric cards ── */}
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

      {/* ── Coverage tips ── */}
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

      {/* ── Outcome Signals ── */}
      <div className="grid gap-4 sm:grid-cols-3">
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
                <p className="text-xs text-muted-foreground">not revised within 48h</p>
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
                <p className="text-xs text-muted-foreground">cited as precedent</p>
              </>
            )}
          </CardContent>
        </Card>

        <Card className="gradient-border hover:glow-amber transition-shadow duration-300">
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">MTTR</CardTitle>
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
                <p className="text-xs text-muted-foreground">mean time to resolve</p>
              </>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Decision Types + Trace Quality ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Decision Types</CardTitle>
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : (
              <DecisionTypeChart data={health?.decision_type_distribution ?? []} />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Trace Quality Breakdown</CardTitle>
          </CardHeader>
          <CardContent>
            {traceHealth.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : !health ? (
              <p className="text-sm text-muted-foreground py-4 text-center">Unavailable.</p>
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
                      <span className="text-muted-foreground font-medium">{item.label}</span>
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
                          width: item.total > 0 ? `${(item.value / item.total) * 100}%` : "0%",
                        }}
                      />
                    </div>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Completeness Trend + Decision Volume ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Completeness Trend</CardTitle>
          </CardHeader>
          <CardContent>
            {decisionsTrend.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : dailyStats.length === 0 ? (
              <p className="text-sm text-muted-foreground py-4 text-center">No data yet.</p>
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

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Decision Volume</CardTitle>
          </CardHeader>
          <CardContent>
            {decisionsTrend.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : dailyStats.length === 0 ? (
              <p className="text-sm text-muted-foreground py-4 text-center">No data yet.</p>
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
      </div>

      {/* ── Conflict Trend + Severity ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Conflict Trend</CardTitle>
          </CardHeader>
          <CardContent>
            {conflictAnalytics.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : !analytics?.trend?.length ? (
              <p className="text-sm text-muted-foreground py-4 text-center">No conflict data yet.</p>
            ) : (
              <ConflictTrend data={analytics.trend} />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Conflicts by Severity</CardTitle>
          </CardHeader>
          <CardContent>
            {conflictAnalytics.isPending ? (
              <Skeleton className="h-24 w-full" />
            ) : !analytics?.by_severity?.length ? (
              <p className="text-sm text-muted-foreground py-4 text-center">No conflicts detected.</p>
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
                      <span className={cn("inline-block h-2 w-2 rounded-full", severityColor(s.severity))} />
                      {s.severity}: {s.count}
                    </span>
                  ))}
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Agent Pairs + Scorecard ── */}
      <div className="grid gap-4 lg:grid-cols-2">
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
              <p className="text-sm text-muted-foreground py-4 text-center">No agent pair conflicts.</p>
            ) : (
              <div className="space-y-2">
                {analytics.by_agent_pair
                  .sort((a, b) => b.count - a.count)
                  .slice(0, 5)
                  .map((pair) => (
                    <div
                      key={`${pair.agent_a}-${pair.agent_b}`}
                      className="flex items-center justify-between rounded-lg border px-3 py-2.5 transition-all duration-200 hover:bg-accent/50 hover:shadow-sm"
                    >
                      <div className="flex items-center gap-2">
                        <Badge variant="outline" className="font-mono text-xs">
                          {pair.agent_a}
                        </Badge>
                        <span className="text-xs text-muted-foreground">vs</span>
                        <Badge variant="outline" className="font-mono text-xs">
                          {pair.agent_b}
                        </Badge>
                      </div>
                      <div className="flex items-center gap-3 text-xs">
                        <span className={cn(pair.open > 0 ? "text-amber-500" : "text-muted-foreground")}>
                          {pair.open} open
                        </span>
                        <span className="text-emerald-500">{pair.resolved} resolved</span>
                        <span className="text-muted-foreground font-medium">{pair.count} total</span>
                      </div>
                    </div>
                  ))}
                {analytics.by_agent_pair.length > 5 && (
                  <p className="text-xs text-muted-foreground pt-1">
                    +{analytics.by_agent_pair.length - 5} more
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
              Agent Scorecard
              <span className="text-xs font-normal text-muted-foreground">(top 5)</span>
            </CardTitle>
          </CardHeader>
          <CardContent>
            {agentsWithStats.isPending ? (
              <Skeleton className="h-32 w-full" />
            ) : (
              <AgentScorecard agents={agentsWithStats.data ?? []} />
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Recent Decisions ── */}
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
