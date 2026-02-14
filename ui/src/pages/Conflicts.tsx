import { Link, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { listConflicts } from "@/lib/api";
import type { DecisionConflict } from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate } from "@/lib/utils";
import { AlertTriangle, ArrowRight, ChevronLeft, ChevronRight, Swords } from "lucide-react";
import { useState, type FormEvent } from "react";

function truncate(text: string, maxLen: number): string {
  if (text.length <= maxLen) return text;
  return text.slice(0, maxLen).trimEnd() + "\u2026";
}

function ConflictSide({
  agent,
  outcome,
  confidence,
  reasoning,
  decidedAt,
  runId,
}: {
  agent: string;
  outcome: string;
  confidence: number;
  reasoning: string | null;
  decidedAt: string;
  runId: string;
}) {
  return (
    <Link
      to={`/decisions/${runId}`}
      className="block space-y-2 rounded-md border p-4 transition-colors hover:border-primary/50 hover:bg-muted/50"
    >
      <div className="flex items-center justify-between">
        <Badge variant="outline" className="font-mono text-xs">
          {agent}
        </Badge>
        <Badge variant="secondary">
          {(confidence * 100).toFixed(0)}%
        </Badge>
      </div>
      <p className="text-sm font-medium leading-snug">{outcome}</p>
      {reasoning && (
        <p className="text-xs text-muted-foreground leading-relaxed">
          {truncate(reasoning, 200)}
        </p>
      )}
      <div className="flex items-center justify-between pt-1">
        <span className="text-xs text-muted-foreground">
          {formatDate(decidedAt)}
        </span>
        <span className="flex items-center gap-1 text-xs text-primary">
          View decision <ArrowRight className="h-3 w-3" />
        </span>
      </div>
    </Link>
  );
}

function ConflictCard({ conflict }: { conflict: DecisionConflict }) {
  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center justify-between">
          <CardTitle className="flex items-center gap-2 text-sm">
            <AlertTriangle className="h-4 w-4 text-amber-500" />
            <span className="font-mono">{conflict.decision_type}</span>
          </CardTitle>
          <span className="text-xs text-muted-foreground">
            Detected {formatDate(conflict.detected_at)}
          </span>
        </div>
        <p className="text-xs text-muted-foreground mt-1">
          {conflict.conflict_kind === "self_contradiction" ? (
            <>
              <span className="font-medium text-foreground">{conflict.agent_a}</span>
              {" contradicted themselves on the same decision type within 7 days"}
            </>
          ) : (
            <>
              <span className="font-medium text-foreground">{conflict.agent_a}</span>
              {" and "}
              <span className="font-medium text-foreground">{conflict.agent_b}</span>
              {" reached different conclusions on the same decision type within an hour"}
            </>
          )}
        </p>
      </CardHeader>
      <CardContent>
        <div className="grid gap-3 sm:grid-cols-[1fr,auto,1fr]">
          <ConflictSide
            agent={conflict.agent_a}
            outcome={conflict.outcome_a}
            confidence={conflict.confidence_a}
            reasoning={conflict.reasoning_a}
            decidedAt={conflict.decided_at_a}
            runId={conflict.run_a}
          />
          <div className="hidden sm:flex items-center justify-center">
            <Swords className="h-5 w-5 text-muted-foreground/40" />
          </div>
          <div className="sm:hidden flex items-center justify-center py-1">
            <span className="text-xs font-medium text-muted-foreground">vs</span>
          </div>
          <ConflictSide
            agent={conflict.agent_b}
            outcome={conflict.outcome_b}
            confidence={conflict.confidence_b}
            reasoning={conflict.reasoning_b}
            decidedAt={conflict.decided_at_b}
            runId={conflict.run_b}
          />
        </div>
      </CardContent>
    </Card>
  );
}

const PAGE_SIZE = 25;

export default function Conflicts() {
  const [searchParams, setSearchParams] = useSearchParams();

  const page = Math.max(0, parseInt(searchParams.get("page") ?? "0", 10));
  const agentFilter = searchParams.get("agent") ?? "";

  const [agentInput, setAgentInput] = useState(agentFilter);

  const { data, isPending } = useQuery({
    queryKey: ["conflicts", page, agentFilter],
    queryFn: () =>
      listConflicts({
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
        ...(agentFilter ? { agent_id: agentFilter } : {}),
      }),
  });

  const totalPages = data ? Math.ceil(data.total / PAGE_SIZE) : 0;

  function applyFilter(e: FormEvent) {
    e.preventDefault();
    const params: Record<string, string> = {};
    if (agentInput.trim()) params.agent = agentInput.trim();
    setSearchParams(params);
  }

  function goToPage(p: number) {
    const params: Record<string, string> = {};
    if (agentFilter) params.agent = agentFilter;
    if (p > 0) params.page = String(p);
    setSearchParams(params);
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold tracking-tight">Conflicts</h1>
        {data?.total != null && data.total > 0 && (
          <Badge variant="outline">{data.total} detected</Badge>
        )}
      </div>

      {/* Agent filter */}
      <form onSubmit={applyFilter} className="flex flex-wrap items-end gap-3">
        <div className="space-y-1">
          <label className="text-xs text-muted-foreground">Agent</label>
          <Input
            placeholder="agent-id"
            value={agentInput}
            onChange={(e) => setAgentInput(e.target.value)}
            className="w-48"
          />
        </div>
        <Button type="submit" size="sm">
          Filter
        </Button>
        {agentFilter && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => {
              setAgentInput("");
              setSearchParams({});
            }}
          >
            Clear
          </Button>
        )}
      </form>

      {isPending ? (
        <div className="space-y-4">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-48 w-full" />
          ))}
        </div>
      ) : !data?.conflicts?.length ? (
        <div className="flex flex-col items-center py-12 text-center">
          <AlertTriangle className="h-12 w-12 text-muted-foreground/30 mb-4" />
          <p className="text-sm text-muted-foreground">
            {agentFilter
              ? `No conflicts found for agent "${agentFilter}".`
              : "No conflicts detected. Agents are in agreement."}
          </p>
        </div>
      ) : (
        <>
          <div className="space-y-4">
            {data.conflicts.map((conflict) => (
              <ConflictCard
                key={`${conflict.decision_a_id}-${conflict.decision_b_id}`}
                conflict={conflict}
              />
            ))}
          </div>

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
