import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { Link, useSearchParams } from "react-router";
import { queryDecisions, listConflicts, listAgentsWithStats } from "@/lib/api";
import type { Decision, DecisionConflict } from "@/types/api";
import { Card, CardContent } from "@/components/ui/card";
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { cn, formatRelativeTime, truncate } from "@/lib/utils";
import {
  Activity,
  AlertTriangle,
  ChevronRight,
  Clock,
  Filter,
  GitBranch,
  Zap,
} from "lucide-react";

// --- Feed item types ---

interface SessionFeedItem {
  kind: "session";
  timestamp: string;
  sessionId: string;
  agentId: string;
  project: string | null;
  decisions: Decision[];
  durationSecs: number;
  avgConfidence: number;
  typeBreakdown: Record<string, number>;
}

interface ConflictFeedItem {
  kind: "conflict";
  timestamp: string;
  conflict: DecisionConflict;
}

interface DecisionFeedItem {
  kind: "decision";
  timestamp: string;
  decision: Decision;
}

type FeedItem = SessionFeedItem | ConflictFeedItem | DecisionFeedItem;

// --- Grouping logic ---

function buildFeed(
  decisions: Decision[],
  conflicts: DecisionConflict[],
): FeedItem[] {
  const items: FeedItem[] = [];

  // Group decisions by session_id
  const bySession = new Map<string, Decision[]>();
  const ungrouped: Decision[] = [];

  for (const d of decisions) {
    const sid = d.session_id;
    if (sid) {
      const group = bySession.get(sid);
      if (group) {
        group.push(d);
      } else {
        bySession.set(sid, [d]);
      }
    } else {
      ungrouped.push(d);
    }
  }

  // Convert session groups into feed items
  for (const [sessionId, sessionDecisions] of bySession) {
    if (sessionDecisions.length === 0) continue;
    const sorted = sessionDecisions.sort(
      (a, b) =>
        new Date(a.valid_from).getTime() - new Date(b.valid_from).getTime(),
    );
    const first = sorted[0]!;
    const last = sorted[sorted.length - 1]!;
    const durationSecs = Math.max(
      0,
      (new Date(last.valid_from).getTime() -
        new Date(first.valid_from).getTime()) /
        1000,
    );

    const typeBreakdown: Record<string, number> = {};
    let totalConf = 0;
    for (const d of sorted) {
      typeBreakdown[d.decision_type] =
        (typeBreakdown[d.decision_type] ?? 0) + 1;
      totalConf += d.confidence;
    }

    items.push({
      kind: "session",
      timestamp: last.valid_from,
      sessionId,
      agentId: first.agent_id,
      project: first.project ?? null,
      decisions: sorted,
      durationSecs,
      avgConfidence: totalConf / sorted.length,
      typeBreakdown,
    });
  }

  // For ungrouped decisions, cluster by agent+project within 30-min windows
  const clusters: Decision[][] = [];
  const sortedUngrouped = [...ungrouped].sort(
    (a, b) =>
      new Date(a.valid_from).getTime() - new Date(b.valid_from).getTime(),
  );

  for (const d of sortedUngrouped) {
    const lastCluster = clusters[clusters.length - 1];
    if (lastCluster && lastCluster.length > 0) {
      const prev = lastCluster[lastCluster.length - 1]!;
      const gap =
        Math.abs(
          new Date(d.valid_from).getTime() -
            new Date(prev.valid_from).getTime(),
        ) / 1000;
      const sameAgent = d.agent_id === prev.agent_id;
      const sameProject = (d.project ?? null) === (prev.project ?? null);
      if (sameAgent && sameProject && gap < 1800) {
        lastCluster.push(d);
        continue;
      }
    }
    clusters.push([d]);
  }

  // Convert clusters: single-decision clusters → DecisionFeedItem, multi → SessionFeedItem
  for (const cluster of clusters) {
    if (cluster.length === 0) continue;
    if (cluster.length === 1) {
      const single = cluster[0]!;
      items.push({
        kind: "decision",
        timestamp: single.valid_from,
        decision: single,
      });
    } else {
      const sorted = cluster;
      const first = sorted[0]!;
      const last = sorted[sorted.length - 1]!;
      const durationSecs = Math.max(
        0,
        (new Date(last.valid_from).getTime() -
          new Date(first.valid_from).getTime()) /
          1000,
      );
      const typeBreakdown: Record<string, number> = {};
      let totalConf = 0;
      for (const d of sorted) {
        typeBreakdown[d.decision_type] =
          (typeBreakdown[d.decision_type] ?? 0) + 1;
        totalConf += d.confidence;
      }
      items.push({
        kind: "session",
        timestamp: last.valid_from,
        sessionId: `cluster-${first.id}`,
        agentId: first.agent_id,
        project: first.project ?? null,
        decisions: sorted,
        durationSecs,
        avgConfidence: totalConf / sorted.length,
        typeBreakdown,
      });
    }
  }

  // Add conflicts
  for (const c of conflicts) {
    items.push({
      kind: "conflict",
      timestamp: c.detected_at,
      conflict: c,
    });
  }

  // Sort newest first
  items.sort(
    (a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime(),
  );

  return items;
}

// --- Duration formatting ---

function formatDuration(secs: number): string {
  if (secs < 60) return `${Math.round(secs)}s`;
  if (secs < 3600) return `${Math.round(secs / 60)}m`;
  const h = Math.floor(secs / 3600);
  const m = Math.round((secs % 3600) / 60);
  return m > 0 ? `${h}h ${m}m` : `${h}h`;
}

// --- Severity colors ---

const severityStyles: Record<string, string> = {
  critical: "bg-red-500/10 text-red-500 border-red-500/30",
  high: "bg-orange-500/10 text-orange-500 border-orange-500/30",
  medium: "bg-amber-500/10 text-amber-500 border-amber-500/30",
  low: "bg-blue-500/10 text-blue-500 border-blue-500/30",
};

// --- Feed item components ---

function SessionCard({ item }: { item: SessionFeedItem }) {
  const isRealSession = !item.sessionId.startsWith("cluster-");
  const topDecisions = [...item.decisions]
    .sort((a, b) => b.confidence - a.confidence)
    .slice(0, 3);

  return (
    <Card className="gradient-border">
      <CardContent className="pt-5 pb-4 space-y-3">
        {/* Header */}
        <div className="flex items-start justify-between gap-2">
          <div className="flex items-center gap-2 flex-wrap">
            <div className="flex items-center justify-center h-6 w-6 rounded-full bg-primary/10">
              <Zap className="h-3.5 w-3.5 text-primary" />
            </div>
            <Badge variant="outline" className="text-xs font-mono px-1.5 py-0">
              {item.agentId}
            </Badge>
            {item.project && (
              <span className="text-xs text-muted-foreground">
                on <span className="font-medium text-foreground/70">{item.project}</span>
              </span>
            )}
          </div>
          <span className="text-xs text-muted-foreground shrink-0">
            {formatRelativeTime(item.timestamp)}
          </span>
        </div>

        {/* Summary line */}
        <p className="text-sm text-foreground/80">
          Made{" "}
          <span className="font-semibold text-foreground">
            {item.decisions.length} decision{item.decisions.length !== 1 ? "s" : ""}
          </span>
          {item.durationSecs > 0 && (
            <>
              {" "}over{" "}
              <span className="inline-flex items-center gap-0.5">
                <Clock className="h-3 w-3 inline" />
                {formatDuration(item.durationSecs)}
              </span>
            </>
          )}
          {" "}at{" "}
          <span className="font-medium">
            {(item.avgConfidence * 100).toFixed(0)}% avg confidence
          </span>
        </p>

        {/* Type breakdown */}
        <div className="flex flex-wrap gap-1.5">
          {Object.entries(item.typeBreakdown)
            .sort(([, a], [, b]) => b - a)
            .map(([type, count]) => (
              <Badge
                key={type}
                variant={decisionTypeBadgeVariant(type)}
                className="text-[10px] px-1.5 py-0"
              >
                {type.replace(/_/g, " ")} ({count})
              </Badge>
            ))}
        </div>

        {/* Top decisions */}
        {topDecisions.length > 0 && (
          <div className="space-y-1.5 pt-1 border-t border-border/50">
            {topDecisions.map((d) => (
              <Link
                key={d.id}
                to={`/decisions/${d.run_id}`}
                className="group flex items-start gap-2 rounded-md px-2 py-1.5 transition-all hover:bg-accent/50"
              >
                <div className="flex-1 min-w-0">
                  <p className="text-sm text-foreground/80 leading-snug">
                    {truncate(d.outcome, 150)}
                  </p>
                </div>
                <span className="text-[10px] text-muted-foreground shrink-0 mt-0.5">
                  {(d.confidence * 100).toFixed(0)}%
                </span>
                <ChevronRight className="h-3.5 w-3.5 text-muted-foreground shrink-0 mt-0.5 opacity-0 group-hover:opacity-100 transition-opacity" />
              </Link>
            ))}
          </div>
        )}

        {/* Link to session if real */}
        {isRealSession && (
          <div className="pt-1">
            <Link
              to={`/sessions/${item.sessionId}`}
              className="text-xs text-primary hover:text-primary/80 transition-colors"
            >
              View full session →
            </Link>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function ConflictCard({ item }: { item: ConflictFeedItem }) {
  const c = item.conflict;
  const severity = c.severity ?? "medium";
  const isResolved = c.status === "resolved" || c.status === "wont_fix";

  return (
    <Card className={cn("border-l-2", isResolved ? "border-l-emerald-500/50 opacity-70" : "border-l-amber-500")}>
      <CardContent className="pt-5 pb-4 space-y-2">
        <div className="flex items-start justify-between gap-2">
          <div className="flex items-center gap-2 flex-wrap">
            <div className={cn("flex items-center justify-center h-6 w-6 rounded-full", isResolved ? "bg-emerald-500/10" : "bg-amber-500/10")}>
              <AlertTriangle className={cn("h-3.5 w-3.5", isResolved ? "text-emerald-500" : "text-amber-500")} />
            </div>
            <span className="text-sm font-medium">
              {isResolved ? "Conflict resolved" : "Conflict detected"}
            </span>
            <Badge className={cn("text-[10px] px-1.5 py-0 border", severityStyles[severity])}>
              {severity}
            </Badge>
            {c.category && (
              <Badge variant="outline" className="text-[10px] px-1.5 py-0">
                {c.category}
              </Badge>
            )}
          </div>
          <span className="text-xs text-muted-foreground shrink-0">
            {formatRelativeTime(item.timestamp)}
          </span>
        </div>

        {/* Agents involved */}
        <p className="text-sm text-foreground/80">
          <Badge variant="outline" className="text-[10px] font-mono px-1.5 py-0">
            {c.agent_a}
          </Badge>
          {c.agent_a !== c.agent_b ? (
            <>
              {" "}vs{" "}
              <Badge variant="outline" className="text-[10px] font-mono px-1.5 py-0">
                {c.agent_b}
              </Badge>
            </>
          ) : (
            <span className="text-xs text-muted-foreground ml-1">(self-contradiction)</span>
          )}
          {" "}on{" "}
          <Badge
            variant={decisionTypeBadgeVariant(c.decision_type)}
            className="text-[10px] px-1.5 py-0"
          >
            {c.decision_type.replace(/_/g, " ")}
          </Badge>
        </p>

        {/* Explanation */}
        {c.explanation && (
          <p className="text-xs text-muted-foreground leading-relaxed">
            {truncate(c.explanation, 200)}
          </p>
        )}

        <Link
          to="/conflicts"
          className="inline-block text-xs text-primary hover:text-primary/80 transition-colors"
        >
          View conflict →
        </Link>
      </CardContent>
    </Card>
  );
}

function StandaloneDecisionCard({ item }: { item: DecisionFeedItem }) {
  const d = item.decision;
  return (
    <Link
      to={`/decisions/${d.run_id}`}
      className="group block"
    >
      <Card className="gradient-border transition-all hover:shadow-glow-sm">
        <CardContent className="pt-5 pb-4 space-y-2">
          <div className="flex items-start justify-between gap-2">
            <div className="flex items-center gap-2 flex-wrap">
              <div className="flex items-center justify-center h-6 w-6 rounded-full bg-muted">
                <GitBranch className="h-3.5 w-3.5 text-muted-foreground" />
              </div>
              <Badge variant="outline" className="text-xs font-mono px-1.5 py-0">
                {d.agent_id}
              </Badge>
              <Badge
                variant={decisionTypeBadgeVariant(d.decision_type)}
                className="text-[10px] px-1.5 py-0"
              >
                {d.decision_type.replace(/_/g, " ")}
              </Badge>
              {d.project && (
                <span className="text-[10px] text-muted-foreground">
                  {d.project}
                </span>
              )}
            </div>
            <div className="flex items-center gap-2 shrink-0">
              <span className="text-xs text-muted-foreground">
                {(d.confidence * 100).toFixed(0)}%
              </span>
              <span className="text-xs text-muted-foreground">
                {formatRelativeTime(item.timestamp)}
              </span>
            </div>
          </div>
          <p className="text-sm text-foreground/80 leading-snug">
            {truncate(d.outcome, 200)}
          </p>
          {d.reasoning && (
            <p className="text-xs text-muted-foreground line-clamp-1">
              {d.reasoning}
            </p>
          )}
        </CardContent>
      </Card>
    </Link>
  );
}

function FeedItemComponent({ item }: { item: FeedItem }) {
  switch (item.kind) {
    case "session":
      return <SessionCard item={item} />;
    case "conflict":
      return <ConflictCard item={item} />;
    case "decision":
      return <StandaloneDecisionCard item={item} />;
  }
}

// --- Summary stats ---

function FeedSummary({ items }: { items: FeedItem[] }) {
  const stats = useMemo(() => {
    let totalDecisions = 0;
    let sessions = 0;
    let conflicts = 0;
    const agents = new Set<string>();

    for (const item of items) {
      switch (item.kind) {
        case "session":
          if (!item.sessionId.startsWith("cluster-")) {
            sessions++;
          }
          totalDecisions += item.decisions.length;
          agents.add(item.agentId);
          break;
        case "decision":
          totalDecisions++;
          agents.add(item.decision.agent_id);
          break;
        case "conflict":
          conflicts++;
          break;
      }
    }

    return { totalDecisions, sessions, conflicts, agents: agents.size };
  }, [items]);

  const cards = [
    { label: "Decisions", value: stats.totalDecisions },
    { label: "Sessions", value: stats.sessions },
    { label: "Agents", value: stats.agents },
    {
      label: "Conflicts",
      value: stats.conflicts,
      highlight: stats.conflicts > 0,
    },
  ];

  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      {cards.map((c) => (
        <Card key={c.label} className="gradient-border">
          <CardContent className="pt-4 pb-3">
            <p className="text-xs text-muted-foreground">{c.label}</p>
            <p
              className={cn(
                "text-2xl font-bold",
                c.highlight && "text-amber-500",
              )}
            >
              {c.value}
            </p>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}

// --- Main page ---

export default function Timeline() {
  const [searchParams, setSearchParams] = useSearchParams();
  const agentFilter = searchParams.get("agent") ?? "";
  const projectFilter = searchParams.get("project") ?? "";

  // Fetch last 200 decisions for the feed
  const decisions = useQuery({
    queryKey: ["activity-feed", "decisions", agentFilter, projectFilter],
    queryFn: () =>
      queryDecisions({
        filters: {
          ...(agentFilter ? { agent_id: [agentFilter] } : {}),
          time_range: {
            from: new Date(Date.now() - 30 * 86_400_000).toISOString(),
            to: new Date().toISOString(),
          },
        },
        order_by: "created_at",
        order_dir: "desc",
        limit: 200,
        offset: 0,
      }),
    staleTime: 30_000,
  });

  const conflicts = useQuery({
    queryKey: ["activity-feed", "conflicts", agentFilter],
    queryFn: () =>
      listConflicts({
        limit: 50,
        ...(agentFilter ? { agent_id: agentFilter } : {}),
      }),
    staleTime: 30_000,
  });

  const agents = useQuery({
    queryKey: ["activity-feed", "agents"],
    queryFn: listAgentsWithStats,
    staleTime: 60_000,
  });

  // Extract distinct projects from decisions
  const projects = useMemo(() => {
    const set = new Set<string>();
    for (const d of decisions.data?.decisions ?? []) {
      if (d.project) set.add(d.project);
    }
    return [...set].sort();
  }, [decisions.data]);

  // Build the feed, applying project filter client-side
  const feedItems = useMemo(() => {
    let decs = decisions.data?.decisions ?? [];
    if (projectFilter) {
      decs = decs.filter((d) => d.project === projectFilter);
    }
    let cons = conflicts.data?.conflicts ?? [];
    if (projectFilter) {
      // Filter conflicts to those involving agents active in the filtered project
      const projectAgents = new Set(decs.map((d) => d.agent_id));
      cons = cons.filter(
        (c) => projectAgents.has(c.agent_a) || projectAgents.has(c.agent_b),
      );
    }
    return buildFeed(decs, cons);
  }, [decisions.data, conflicts.data, projectFilter]);

  const agentList = agents.data ?? [];
  const isPending = decisions.isPending || conflicts.isPending;

  function updateParams(updates: Record<string, string>) {
    const params: Record<string, string> = {};
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
      {/* Header */}
      <div>
        <h1 className="text-2xl font-bold tracking-tight">Activity Feed</h1>
        <p className="mt-0.5 text-sm text-muted-foreground">
          What happened across your agents, sessions, and projects
        </p>
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

      {/* Content */}
      {isPending ? (
        <div className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            {Array.from({ length: 4 }).map((_, i) => (
              <Skeleton key={i} className="h-20" />
            ))}
          </div>
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} className="h-32" />
          ))}
        </div>
      ) : feedItems.length === 0 ? (
        <div className="flex flex-col items-center justify-center py-16">
          <Activity className="h-12 w-12 text-muted-foreground/40 mb-3" />
          <p className="text-sm text-muted-foreground">
            No activity found for the last 30 days
          </p>
          <p className="text-xs text-muted-foreground mt-1">
            Try adjusting your filters or trace some decisions
          </p>
        </div>
      ) : (
        <>
          <FeedSummary items={feedItems} />

          {/* Feed timeline */}
          <div className="relative space-y-3 pl-4">
            {/* Vertical line */}
            <div className="absolute left-[7px] top-2 bottom-2 w-px bg-gradient-to-b from-primary/40 via-border to-transparent" />

            {feedItems.map((item, i) => (
              <div key={`${item.kind}-${i}`} className="relative animate-list-item">
                {/* Dot on the timeline */}
                <div
                  className={cn(
                    "absolute -left-4 top-6 h-2.5 w-2.5 rounded-full border-2 border-background",
                    item.kind === "conflict"
                      ? "bg-amber-500"
                      : item.kind === "session"
                        ? "bg-primary"
                        : "bg-muted-foreground/50",
                  )}
                />
                <div className="ml-4">
                  <FeedItemComponent item={item} />
                </div>
              </div>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
