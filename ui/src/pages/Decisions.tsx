import { useQuery } from "@tanstack/react-query";
import { useSearchParams, Link } from "react-router";
import { queryDecisions, listAgents, listDecisionFacets } from "@/lib/api";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate, truncate } from "@/lib/utils";
import { ChevronLeft, ChevronRight, FileText } from "lucide-react";

function typeRowClass(decisionType: string): string {
  const map: Record<string, string> = {
    architecture: "type-border-architecture",
    security: "type-border-security",
    code_review: "type-border-code_review",
    trade_off: "type-border-trade_off",
    planning: "type-border-planning",
    investigation: "type-border-investigation",
    assessment: "type-border-assessment",
  };
  return map[decisionType] ?? "";
}

const PAGE_SIZE = 25;
const ALL = "__all__";

export default function Decisions() {
  const [searchParams, setSearchParams] = useSearchParams();

  const page = Math.max(0, parseInt(searchParams.get("page") ?? "0", 10));
  const agentFilter = searchParams.get("agent") ?? "";
  const typeFilter = searchParams.get("type") ?? "";
  const projectFilter = searchParams.get("project") ?? "";

  const { data, isPending } = useQuery({
    queryKey: ["decisions", page, agentFilter, typeFilter, projectFilter],
    queryFn: () =>
      queryDecisions({
        filters: {
          ...(agentFilter ? { agent_id: [agentFilter] } : {}),
          ...(typeFilter ? { decision_type: typeFilter } : {}),
          ...(projectFilter ? { project: projectFilter } : {}),
        },
        order_by: "valid_from",
        order_dir: "desc",
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
      }),
  });

  const { data: agents } = useQuery({
    queryKey: ["agents"],
    queryFn: listAgents,
    staleTime: 60_000,
  });

  const { data: facets } = useQuery({
    queryKey: ["decision-facets"],
    queryFn: listDecisionFacets,
    staleTime: 60_000,
  });

  function updateFilter(key: string, value: string) {
    const params: Record<string, string> = {};
    const current = { agent: agentFilter, type: typeFilter, project: projectFilter };
    for (const [k, v] of Object.entries(current)) {
      if (k === key) {
        if (value) params[k] = value;
      } else if (v) {
        params[k] = v;
      }
    }
    // Reset to page 0 when filters change
    setSearchParams(params);
  }

  function clearFilters() {
    setSearchParams({});
  }

  function goToPage(p: number) {
    const params: Record<string, string> = {};
    if (agentFilter) params.agent = agentFilter;
    if (typeFilter) params.type = typeFilter;
    if (projectFilter) params.project = projectFilter;
    if (p > 0) params.page = String(p);
    setSearchParams(params);
  }

  const hasFilters = agentFilter || typeFilter || projectFilter;
  const totalPages = data ? Math.ceil(data.total / PAGE_SIZE) : 0;

  return (
    <div className="space-y-8 animate-page">
      <div className="page-header">
        <h1 className="text-2xl font-semibold">Decisions</h1>
        <p className="mt-1 text-sm text-muted-foreground">Full audit trail of every traced AI decision</p>
      </div>

      {/* Filters */}
      <div className="flex flex-wrap items-end gap-3">
        <div className="space-y-1">
          <label className="text-xs text-muted-foreground">Agent</label>
          <Select
            value={agentFilter || ALL}
            onValueChange={(v) => updateFilter("agent", v === ALL ? "" : v)}
          >
            <SelectTrigger className="w-44">
              <SelectValue placeholder="All agents" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>All agents</SelectItem>
              {agents?.map((a) => (
                <SelectItem key={a.agent_id} value={a.agent_id}>
                  {a.agent_id}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1">
          <label className="text-xs text-muted-foreground">Type</label>
          <Select
            value={typeFilter || ALL}
            onValueChange={(v) => updateFilter("type", v === ALL ? "" : v)}
          >
            <SelectTrigger className="w-44">
              <SelectValue placeholder="All types" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>All types</SelectItem>
              {facets?.types.map((t) => (
                <SelectItem key={t} value={t}>
                  {t}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1">
          <label className="text-xs text-muted-foreground">Project</label>
          <Select
            value={projectFilter || ALL}
            onValueChange={(v) => updateFilter("project", v === ALL ? "" : v)}
          >
            <SelectTrigger className="w-44">
              <SelectValue placeholder="All projects" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>All projects</SelectItem>
              {facets?.projects.map((p) => (
                <SelectItem key={p} value={p}>
                  {p}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {hasFilters && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={clearFilters}
          >
            Clear
          </Button>
        )}
      </div>

      {/* Table */}
      {isPending ? (
        <div className="space-y-2">
          {Array.from({ length: 10 }).map((_, i) => (
            <Skeleton key={i} className="h-10 w-full" />
          ))}
        </div>
      ) : !data?.decisions?.length ? (
        <div className="flex flex-col items-center py-14 text-center">
          <div className="relative mb-4">
            <div className="absolute inset-0 rounded-full bg-primary/8 blur-xl" />
            <FileText className="relative h-10 w-10 text-primary/25" />
          </div>
          <p className="text-sm font-medium text-muted-foreground">No decisions found</p>
          {hasFilters ? (
            <p className="text-xs text-muted-foreground/50 mt-1">Try adjusting your filters.</p>
          ) : (
            <p className="text-xs text-muted-foreground/50 mt-1">No decisions have been traced yet.</p>
          )}
        </div>
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Timestamp</TableHead>
                <TableHead>Agent</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Outcome</TableHead>
                <TableHead className="text-right">Confidence</TableHead>
                <TableHead className="text-right">Completeness</TableHead>
                <TableHead>Project</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.decisions.map((d) => (
                  <TableRow key={d.id} className={typeRowClass(d.decision_type)}>
                    <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                      <Link
                        to={`/decisions/${d.run_id}`}
                        className="hover:underline"
                      >
                        {formatDate(d.created_at)}
                      </Link>
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline" className="font-mono text-xs">
                        {d.agent_id}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <Badge variant={decisionTypeBadgeVariant(d.decision_type)}>{d.decision_type}</Badge>
                    </TableCell>
                    <TableCell className="max-w-[200px]">
                      <Link
                        to={`/decisions/${d.run_id}`}
                        className="hover:underline"
                      >
                        {truncate(d.outcome, 60)}
                      </Link>
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex items-center justify-end gap-2">
                        <div className="h-1.5 w-12 rounded-full bg-muted overflow-hidden">
                          <div
                            className="h-full rounded-full bg-gradient-to-r from-primary to-blue-400 progress-fill-animated shadow-[0_0_6px_-1px_hsl(var(--glow-blue)/0.4)]"
                            style={{ width: `${(d.confidence * 100).toFixed(0)}%` }}
                          />
                        </div>
                        <span className="font-mono text-xs tabular-nums w-8 text-right">
                          {(d.confidence * 100).toFixed(0)}%
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex items-center justify-end gap-2">
                        <div className="h-1.5 w-12 rounded-full bg-muted overflow-hidden">
                          <div
                            className={`h-full rounded-full progress-fill-animated ${
                              d.completeness_score >= 0.7
                                ? "bg-gradient-to-r from-emerald-600 to-emerald-400 shadow-[0_0_6px_-1px_hsl(var(--glow-emerald)/0.4)]"
                                : d.completeness_score >= 0.5
                                ? "bg-gradient-to-r from-amber-600 to-amber-400 shadow-[0_0_6px_-1px_hsl(var(--glow-amber)/0.4)]"
                                : "bg-gradient-to-r from-red-600 to-red-400 shadow-[0_0_6px_-1px_hsl(var(--glow-red)/0.4)]"
                            }`}
                            style={{ width: `${(d.completeness_score * 100).toFixed(0)}%` }}
                          />
                        </div>
                        <span className={`font-mono text-xs tabular-nums w-8 text-right ${
                          d.completeness_score >= 0.7
                            ? "text-emerald-600 dark:text-emerald-400"
                            : d.completeness_score >= 0.5
                            ? "text-amber-600 dark:text-amber-400"
                            : "text-red-600 dark:text-red-400"
                        }`}>
                          {(d.completeness_score * 100).toFixed(0)}%
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {d.project ? (
                        <Badge variant="outline" className="font-mono text-xs">
                          {d.project}
                        </Badge>
                      ) : (
                        <span className="opacity-40">{"\u2014"}</span>
                      )}
                    </TableCell>
                  </TableRow>
              ))}
            </TableBody>
          </Table>

          {/* Pagination */}
          <div className="flex items-center justify-between">
            <p className="text-sm text-muted-foreground">
              Showing {page * PAGE_SIZE + 1}{"\u2013"}
              {Math.min((page + 1) * PAGE_SIZE, data.total)} of{" "}
              {data.total.toLocaleString()}
            </p>
            <div className="flex gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={page === 0}
                onClick={() => goToPage(page - 1)}
              >
                <ChevronLeft className="h-4 w-4" />
                Prev
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={page >= totalPages - 1}
                onClick={() => goToPage(page + 1)}
              >
                Next
                <ChevronRight className="h-4 w-4" />
              </Button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
