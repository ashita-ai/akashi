import { Link, useSearchParams } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { listConflicts, patchConflict, listAgents, ApiError } from "@/lib/api";
import type { DecisionConflict, ConflictStatus } from "@/types/api";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { formatDate } from "@/lib/utils";
import {
  AlertTriangle,
  ArrowRight,
  Check,
  CheckCircle2,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  ChevronUp,
  Eye,
  Swords,
  XCircle,
} from "lucide-react";
import { useState } from "react";

function truncate(text: string, maxLen: number): string {
  if (text.length <= maxLen) return text;
  return text.slice(0, maxLen).trimEnd() + "\u2026";
}

const statusConfig: Record<
  ConflictStatus,
  { label: string; variant: "default" | "secondary" | "destructive" | "success" | "warning" | "outline" }
> = {
  open: { label: "Open", variant: "warning" },
  acknowledged: { label: "Acknowledged", variant: "secondary" },
  resolved: { label: "Resolved", variant: "success" },
  wont_fix: { label: "Won't Fix", variant: "outline" },
};

const severityConfig: Record<string, { variant: "default" | "secondary" | "destructive" | "success" | "warning" | "outline" }> = {
  critical: { variant: "destructive" },
  high: { variant: "warning" },
  medium: { variant: "secondary" },
  low: { variant: "outline" },
};

function StatusBadge({ status }: { status: ConflictStatus }) {
  const config = statusConfig[status] ?? statusConfig.open;
  return <Badge variant={config.variant}>{config.label}</Badge>;
}

function SeverityBadge({ severity }: { severity: string | null }) {
  if (!severity) return null;
  const config = severityConfig[severity] ?? { variant: "secondary" as const };
  return (
    <Badge variant={config.variant} className="text-xs">
      {severity}
    </Badge>
  );
}

function CategoryBadge({ category }: { category: string | null }) {
  if (!category) return null;
  return (
    <Badge variant="outline" className="text-xs">
      {category}
    </Badge>
  );
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
      className="block h-full space-y-2 rounded-md border p-4 transition-colors hover:border-primary/50 hover:bg-muted/50"
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

function agentLabel(conflict: DecisionConflict): string {
  if (conflict.conflict_kind === "self_contradiction") {
    return conflict.agent_a;
  }
  return `${conflict.agent_a} vs ${conflict.agent_b}`;
}

function timeDelta(a: string, b: string): string {
  const ms = Math.abs(new Date(b).getTime() - new Date(a).getTime());
  const mins = Math.round(ms / 60_000);
  if (mins < 60) return `${mins}m apart`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h apart`;
  const days = Math.round(hours / 24);
  return `${days}d apart`;
}

function ConflictCard({
  conflict,
  onAdjudicate,
}: {
  conflict: DecisionConflict;
  onAdjudicate: (conflict: DecisionConflict) => void;
}) {
  const [expanded, setExpanded] = useState(false);

  return (
    <Card>
      <CardHeader className="pb-3">
        {/* Row 1: badges + date + resolve */}
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <StatusBadge status={conflict.status} />
            <SeverityBadge severity={conflict.severity} />
            <CategoryBadge category={conflict.category} />
            <Badge variant="outline" className="font-mono text-xs">
              {conflict.decision_type}
            </Badge>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-xs text-muted-foreground">
              {formatDate(conflict.detected_at)}
            </span>
            {(conflict.status === "open" || conflict.status === "acknowledged") && (
              <Button
                variant="ghost"
                size="sm"
                className="h-7 text-xs"
                onClick={() => onAdjudicate(conflict)}
              >
                <Eye className="h-3 w-3 mr-1" />
                Adjudicate
              </Button>
            )}
          </div>
        </div>

        {/* Row 2: explanation — the primary content */}
        {conflict.explanation ? (
          <p className="text-sm leading-relaxed mt-2">
            {conflict.explanation}
          </p>
        ) : (
          <p className="text-sm text-muted-foreground mt-2">
            {conflict.conflict_kind === "self_contradiction"
              ? `${conflict.agent_a} made contradictory ${conflict.decision_type} decisions.`
              : `${conflict.agent_a} and ${conflict.agent_b} disagree on ${conflict.decision_type}.`}
          </p>
        )}

        {/* Row 3: agents + time context */}
        <div className="flex items-center gap-3 mt-2 text-xs text-muted-foreground">
          <span className="flex items-center gap-1">
            <Swords className="h-3 w-3" />
            <span className="font-medium text-foreground">{agentLabel(conflict)}</span>
          </span>
          <span>{timeDelta(conflict.decided_at_a, conflict.decided_at_b)}</span>
          {conflict.conflict_kind === "self_contradiction" && (
            <Badge variant="outline" className="text-[10px] px-1.5 py-0">self</Badge>
          )}
        </div>

        {/* Resolution note */}
        {conflict.resolution_note && (
          <p className="text-xs mt-2 border-l-2 border-emerald-500 pl-2">
            <span className="text-muted-foreground">Resolution:</span>{" "}
            {conflict.resolution_note}
            {conflict.resolved_by && (
              <span className="text-muted-foreground"> by {conflict.resolved_by}</span>
            )}
          </p>
        )}
      </CardHeader>

      {/* Collapsible detail: the two decision sides */}
      <CardContent className="pt-0">
        <Button
          variant="ghost"
          size="sm"
          className="w-full justify-between text-xs text-muted-foreground h-8"
          onClick={() => setExpanded(!expanded)}
        >
          <span>
            {expanded ? "Hide" : "Show"} both decisions
          </span>
          {expanded
            ? <ChevronUp className="h-3.5 w-3.5" />
            : <ChevronDown className="h-3.5 w-3.5" />
          }
        </Button>
        {expanded && (
          <div className="grid gap-3 sm:grid-cols-[1fr,auto,1fr] mt-3">
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
        )}
      </CardContent>
    </Card>
  );
}

const PAGE_SIZE = 25;
const ALL_AGENTS = "__all__";

export default function Conflicts() {
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();

  const page = Math.max(0, parseInt(searchParams.get("page") ?? "0", 10));
  const agentFilter = searchParams.get("agent") ?? "";
  const statusFilter = searchParams.get("status") ?? "";

  const { data: agentsData } = useQuery({
    queryKey: ["agents"],
    queryFn: listAgents,
    staleTime: 60_000,
  });

  const [adjudicateTarget, setAdjudicateTarget] = useState<DecisionConflict | null>(null);
  const [adjudicateStatus, setAdjudicateStatus] = useState<string>("acknowledged");
  const [adjudicateNote, setAdjudicateNote] = useState("");
  const [adjudicateWinner, setAdjudicateWinner] = useState<string | null>(null);
  const [adjudicateError, setAdjudicateError] = useState<string | null>(null);

  const { data, isPending } = useQuery({
    queryKey: ["conflicts", page, agentFilter, statusFilter],
    queryFn: () =>
      listConflicts({
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
        ...(agentFilter ? { agent_id: agentFilter } : {}),
        ...(statusFilter ? { status: statusFilter } : {}),
      }),
  });

  const filteredConflicts = data?.conflicts;

  const totalPages = data ? Math.ceil(data.total / PAGE_SIZE) : 0;

  const adjudicateMutation = useMutation({
    mutationFn: (params: { id: string; status: string; resolution_note?: string; winning_decision_id?: string }) =>
      patchConflict(params.id, {
        status: params.status,
        resolution_note: params.resolution_note,
        winning_decision_id: params.winning_decision_id,
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["conflicts"] });
      setAdjudicateTarget(null);
      setAdjudicateNote("");
      setAdjudicateWinner(null);
      setAdjudicateError(null);
    },
    onError: (err) => {
      setAdjudicateError(err instanceof ApiError ? err.message : "Failed to update conflict");
    },
  });

  function selectAgent(value: string) {
    const agent = value === ALL_AGENTS ? "" : value;
    const params: Record<string, string> = {};
    if (agent) params.agent = agent;
    if (statusFilter) params.status = statusFilter;
    setSearchParams(params);
  }

  function setStatus(value: string) {
    const params: Record<string, string> = {};
    if (agentFilter) params.agent = agentFilter;
    if (value && value !== "all") params.status = value;
    setSearchParams(params);
  }

  function goToPage(p: number) {
    const params: Record<string, string> = {};
    if (agentFilter) params.agent = agentFilter;
    if (statusFilter) params.status = statusFilter;
    if (p > 0) params.page = String(p);
    setSearchParams(params);
  }

  function openAdjudicateDialog(conflict: DecisionConflict) {
    setAdjudicateTarget(conflict);
    setAdjudicateStatus("acknowledged");
    setAdjudicateNote("");
    setAdjudicateWinner(null);
    setAdjudicateError(null);
  }

  function handleAdjudicate() {
    if (!adjudicateTarget) return;
    adjudicateMutation.mutate({
      id: adjudicateTarget.id,
      status: adjudicateStatus,
      ...(adjudicateNote.trim() ? { resolution_note: adjudicateNote.trim() } : {}),
      ...(adjudicateStatus === "resolved" && adjudicateWinner ? { winning_decision_id: adjudicateWinner } : {}),
    });
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold tracking-tight">Conflicts</h1>
        {data?.total != null && data.total > 0 && (
          <Badge variant="outline">{data.total} detected</Badge>
        )}
      </div>

      {/* Filters */}
      <div className="flex flex-wrap items-end gap-3">
        <div className="space-y-1">
          <label className="text-xs text-muted-foreground">Agent</label>
          <Select value={agentFilter || ALL_AGENTS} onValueChange={selectAgent}>
            <SelectTrigger className="w-44">
              <SelectValue placeholder="All agents" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL_AGENTS}>All agents</SelectItem>
              {agentsData?.map((a) => (
                <SelectItem key={a.agent_id} value={a.agent_id}>
                  {a.agent_id}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1">
          <label className="text-xs text-muted-foreground">Status</label>
          <Select value={statusFilter || "all"} onValueChange={setStatus}>
            <SelectTrigger className="w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All statuses</SelectItem>
              <SelectItem value="open">Open</SelectItem>
              <SelectItem value="acknowledged">Acknowledged</SelectItem>
              <SelectItem value="resolved">Resolved</SelectItem>
              <SelectItem value="wont_fix">Won't Fix</SelectItem>
            </SelectContent>
          </Select>
        </div>
        {(agentFilter || statusFilter) && (
          <Button
            variant="ghost"
            size="sm"
            className="self-end"
            onClick={() => setSearchParams({})}
          >
            Clear
          </Button>
        )}
      </div>

      {isPending ? (
        <div className="space-y-4">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-48 w-full" />
          ))}
        </div>
      ) : !filteredConflicts?.length ? (
        <div className="flex flex-col items-center py-12 text-center">
          <AlertTriangle className="h-12 w-12 text-muted-foreground/30 mb-4" />
          <p className="text-sm text-muted-foreground">
            {agentFilter || statusFilter
              ? "No conflicts match the current filters."
              : "No conflicts detected. Agents are in agreement."}
          </p>
        </div>
      ) : (
        <>
          <div className="space-y-4">
            {filteredConflicts.map((conflict) => (
              <ConflictCard
                key={conflict.id ?? `${conflict.decision_a_id}-${conflict.decision_b_id}`}
                conflict={conflict}
                onAdjudicate={openAdjudicateDialog}
              />
            ))}
          </div>

          {/* Pagination */}
          {data && data.total > PAGE_SIZE && (
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
          )}
        </>
      )}

      {/* Adjudication dialog */}
      <Dialog open={adjudicateTarget !== null} onOpenChange={(open) => !open && setAdjudicateTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Adjudicate Conflict</DialogTitle>
            <DialogDescription>
              Update the status of this conflict between{" "}
              <strong>{adjudicateTarget?.agent_a}</strong>
              {adjudicateTarget?.agent_a !== adjudicateTarget?.agent_b && (
                <> and <strong>{adjudicateTarget?.agent_b}</strong></>
              )}
              {" on "}<strong>{adjudicateTarget?.decision_type}</strong>.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Action</label>
              <div className="flex gap-2">
                <Button
                  variant={adjudicateStatus === "acknowledged" ? "default" : "outline"}
                  size="sm"
                  onClick={() => setAdjudicateStatus("acknowledged")}
                >
                  <Eye className="h-3.5 w-3.5 mr-1.5" />
                  Acknowledge
                </Button>
                <Button
                  variant={adjudicateStatus === "resolved" ? "default" : "outline"}
                  size="sm"
                  onClick={() => setAdjudicateStatus("resolved")}
                >
                  <CheckCircle2 className="h-3.5 w-3.5 mr-1.5" />
                  Resolve
                </Button>
                <Button
                  variant={adjudicateStatus === "wont_fix" ? "default" : "outline"}
                  size="sm"
                  onClick={() => setAdjudicateStatus("wont_fix")}
                >
                  <XCircle className="h-3.5 w-3.5 mr-1.5" />
                  Won't Fix
                </Button>
              </div>
            </div>
            {adjudicateStatus === "resolved" && adjudicateTarget && (
              <div className="space-y-2">
                <label className="text-sm font-medium">Winner (optional)</label>
                <div className="grid grid-cols-2 gap-2">
                  <button
                    type="button"
                    onClick={() =>
                      setAdjudicateWinner(
                        adjudicateWinner === adjudicateTarget.decision_a_id
                          ? null
                          : adjudicateTarget.decision_a_id,
                      )
                    }
                    className={`text-left rounded-md border p-3 text-xs transition-colors ${
                      adjudicateWinner === adjudicateTarget.decision_a_id
                        ? "border-primary bg-primary/5"
                        : "hover:border-primary/50 hover:bg-muted/50"
                    }`}
                  >
                    <div className="flex items-center justify-between mb-1.5">
                      <Badge variant="outline" className="font-mono text-[10px]">
                        {adjudicateTarget.agent_a}
                      </Badge>
                      {adjudicateWinner === adjudicateTarget.decision_a_id && (
                        <Check className="h-3 w-3 text-primary" />
                      )}
                    </div>
                    <p className="leading-snug text-muted-foreground">
                      {truncate(adjudicateTarget.outcome_a, 80)}
                    </p>
                  </button>
                  <button
                    type="button"
                    onClick={() =>
                      setAdjudicateWinner(
                        adjudicateWinner === adjudicateTarget.decision_b_id
                          ? null
                          : adjudicateTarget.decision_b_id,
                      )
                    }
                    className={`text-left rounded-md border p-3 text-xs transition-colors ${
                      adjudicateWinner === adjudicateTarget.decision_b_id
                        ? "border-primary bg-primary/5"
                        : "hover:border-primary/50 hover:bg-muted/50"
                    }`}
                  >
                    <div className="flex items-center justify-between mb-1.5">
                      <Badge variant="outline" className="font-mono text-[10px]">
                        {adjudicateTarget.agent_b}
                      </Badge>
                      {adjudicateWinner === adjudicateTarget.decision_b_id && (
                        <Check className="h-3 w-3 text-primary" />
                      )}
                    </div>
                    <p className="leading-snug text-muted-foreground">
                      {truncate(adjudicateTarget.outcome_b, 80)}
                    </p>
                  </button>
                </div>
              </div>
            )}
            <div className="space-y-2">
              <label className="text-sm font-medium">Note (optional)</label>
              <textarea
                className="w-full rounded-md border bg-background px-3 py-2 text-sm min-h-[80px] resize-none focus:outline-none focus:ring-2 focus:ring-ring"
                placeholder="Describe why this conflict was adjudicated this way..."
                value={adjudicateNote}
                onChange={(e) => setAdjudicateNote(e.target.value)}
              />
            </div>
            {adjudicateError && (
              <p className="text-sm text-destructive">{adjudicateError}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAdjudicateTarget(null)}>
              Cancel
            </Button>
            <Button onClick={handleAdjudicate} disabled={adjudicateMutation.isPending}>
              <Check className="h-4 w-4 mr-1.5" />
              {adjudicateMutation.isPending ? "Saving\u2026" : "Save"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
