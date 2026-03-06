import { Link, useSearchParams } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { listConflictGroups, patchConflict, listAgents, ApiError } from "@/lib/api";
import type { ConflictGroup, DecisionConflict } from "@/types/api";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
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

const severityConfig: Record<
  string,
  {
    variant:
      | "default"
      | "secondary"
      | "destructive"
      | "success"
      | "warning"
      | "outline";
    label: string;
  }
> = {
  critical: { variant: "destructive", label: "Critical" },
  high: { variant: "warning", label: "High" },
  medium: { variant: "secondary", label: "Medium" },
  low: { variant: "outline", label: "Low" },
};

function SeverityBadge({ severity }: { severity: string | null }) {
  if (!severity) return null;
  const cfg = severityConfig[severity] ?? {
    variant: "secondary" as const,
    label: severity,
  };
  return (
    <Badge variant={cfg.variant} className="text-xs font-semibold uppercase tracking-wide shrink-0">
      {cfg.label}
    </Badge>
  );
}

// ── Conflict pair helpers ────────────────────────────────────────────────────

/** Count distinct run IDs across all conflict pairs. */
function countDistinctTraces(conflicts: DecisionConflict[]): number {
  const runs = new Set<string>();
  for (const c of conflicts) {
    if (c.run_a) runs.add(c.run_a);
    if (c.run_b) runs.add(c.run_b);
  }
  return runs.size;
}

/** Count distinct decision IDs across all conflict pairs. */
function countDistinctDecisions(conflicts: DecisionConflict[]): number {
  const ids = new Set<string>();
  for (const c of conflicts) {
    if (c.decision_a_id) ids.add(c.decision_a_id);
    if (c.decision_b_id) ids.add(c.decision_b_id);
  }
  return ids.size;
}

// ── Conflict pair row ────────────────────────────────────────────────────────
//
// Shows one explicit conflict: which exact decision from agent A is paired
// against which exact decision from agent B. groupAgentA normalises the
// display so the same agent always appears on the same side regardless of
// how the conflict was stored internally. repeatedLeft/repeatedRight flag
// decisions that already appeared in an earlier pair in this group.

function ConflictPairRow({
  c,
  groupAgentA,
  repeatedLeft,
  repeatedRight,
  onAdjudicate,
}: {
  c: DecisionConflict;
  groupAgentA: string;
  repeatedLeft: boolean;
  repeatedRight: boolean;
  onAdjudicate: (c: DecisionConflict) => void;
}) {
  // Normalise: always show groupAgentA on the left.
  const flip = c.agent_a !== groupAgentA;
  const left = {
    agent: flip ? c.agent_b : c.agent_a,
    decidedAt: flip ? c.decided_at_b : c.decided_at_a,
    confidence: flip ? c.confidence_b : c.confidence_a,
    outcome: flip ? c.outcome_b : c.outcome_a,
    runId: flip ? c.run_b : c.run_a,
  };
  const right = {
    agent: flip ? c.agent_a : c.agent_b,
    decidedAt: flip ? c.decided_at_a : c.decided_at_b,
    confidence: flip ? c.confidence_a : c.confidence_b,
    outcome: flip ? c.outcome_a : c.outcome_b,
    runId: flip ? c.run_a : c.run_b,
  };

  const canAdjudicate = c.status === "open" || c.status === "acknowledged";

  function Side({
    side,
    repeated,
  }: {
    side: typeof left;
    repeated: boolean;
  }) {
    return (
      <Link
        to={`/decisions/${side.runId}`}
        className={`p-3 block transition-colors hover:bg-muted/50 min-w-0 ${repeated ? "opacity-50" : ""}`}
      >
        <div className="flex items-center gap-1.5 mb-1.5 flex-wrap">
          <Badge variant="outline" className="font-mono text-[10px] shrink-0">
            {side.agent}
          </Badge>
          <span className="text-[10px] text-muted-foreground tabular-nums">
            {formatDate(side.decidedAt)}
          </span>
          {repeated && (
            <span className="text-[10px] text-muted-foreground italic shrink-0">
              same as above
            </span>
          )}
          <Badge variant="secondary" className="text-[10px] shrink-0 ml-auto">
            {(side.confidence * 100).toFixed(0)}%
          </Badge>
        </div>
        <p className={`leading-snug ${repeated ? "text-muted-foreground" : "text-foreground/80"}`}>
          {truncate(side.outcome, 120)}
        </p>
      </Link>
    );
  }

  return (
    <div className="rounded border overflow-hidden text-xs">
      <div className="grid grid-cols-[1fr,auto,1fr]">
        <Side side={left} repeated={repeatedLeft} />
        <div className="flex items-center justify-center px-2 border-x bg-muted/30">
          <span className="text-[10px] font-medium text-muted-foreground">vs</span>
        </div>
        <Side side={right} repeated={repeatedRight} />
      </div>

      {canAdjudicate && (
        <div className="flex justify-end border-t px-3 py-1.5 bg-muted/20">
          <Button
            variant="ghost"
            size="sm"
            className="h-6 text-[10px]"
            onClick={() => onAdjudicate(c)}
          >
            <Eye className="h-3 w-3 mr-1" />
            Adjudicate this pair
          </Button>
        </div>
      )}
    </div>
  );
}

// ── Group card ──────────────────────────────────────────────────────────────

function ConflictGroupCard({
  group,
  onAdjudicate,
}: {
  group: ConflictGroup;
  onAdjudicate: (conflict: DecisionConflict) => void;
}) {
  const [expanded, setExpanded] = useState(false);

  const rep = group.representative;
  const openConflicts = group.open_conflicts ?? [];
  const isSelf = group.conflict_kind === "self_contradiction";

  const traceCount = countDistinctTraces(openConflicts);
  const decisionCount = countDistinctDecisions(openConflicts);

  const canAdjudicate =
    rep && (rep.status === "open" || rep.status === "acknowledged");

  return (
    <Card>
      <CardHeader className="pb-3">
        {/* ── Row 1: severity · agents · topic ── */}
        <div className="flex items-start justify-between gap-4">
          <div className="space-y-1.5 min-w-0">
            <div className="flex items-center gap-2 flex-wrap">
              {rep?.severity && <SeverityBadge severity={rep.severity} />}
              <span className="font-semibold text-sm">{group.agent_a}</span>
              {!isSelf ? (
                <>
                  <Swords className="h-3.5 w-3.5 text-muted-foreground/40 shrink-0" />
                  <span className="font-semibold text-sm">{group.agent_b}</span>
                </>
              ) : (
                <Badge variant="outline" className="text-[10px]">
                  self
                </Badge>
              )}
              <Badge variant={decisionTypeBadgeVariant(group.decision_type)} className="font-mono text-xs">
                {group.decision_type}
              </Badge>
            </div>

            {/* ── Row 2: counts ── */}
            <div className="flex items-center gap-3 text-xs text-muted-foreground flex-wrap">
              {group.open_count > 0 && (
                <Badge variant="warning" className="text-xs">
                  {group.open_count} open
                </Badge>
              )}
              {decisionCount > 0 && (
                <span>
                  {decisionCount} decision{decisionCount !== 1 ? "s" : ""} in
                  conflict
                </span>
              )}
              {traceCount > 0 && (
                <span>
                  {traceCount} trace{traceCount !== 1 ? "s" : ""}
                </span>
              )}
              <span className="text-muted-foreground/60">
                Last {formatDate(group.last_detected_at)}
              </span>
            </div>
          </div>

          {/* ── Actions ── */}
          <div className="flex items-center gap-1 shrink-0">
            {canAdjudicate && (
              <Button
                variant="ghost"
                size="sm"
                className="h-7 text-xs"
                onClick={() => onAdjudicate(rep)}
              >
                <Eye className="h-3 w-3 mr-1" />
                Adjudicate
              </Button>
            )}
            <Button
              variant="ghost"
              size="sm"
              className="h-7 w-7 p-0"
              onClick={() => setExpanded(!expanded)}
              aria-label={expanded ? "Collapse" : "Expand"}
            >
              {expanded ? (
                <ChevronUp className="h-3.5 w-3.5" />
              ) : (
                <ChevronDown className="h-3.5 w-3.5" />
              )}
            </Button>
          </div>
        </div>

        {/* ── LLM explanation — always shown, this is the narrative ── */}
        {rep?.explanation && (
          <p className="text-sm text-muted-foreground leading-relaxed">
            {rep.explanation}
          </p>
        )}
      </CardHeader>

      {/* ── Expanded: explicit conflict pairs ── */}
      {expanded && (
        <CardContent className="pt-0">
          <div className="border-t pt-4 space-y-3">
            {openConflicts.length === 0 ? (
              <p className="text-xs text-muted-foreground italic">
                All conflicts in this group have been resolved.
              </p>
            ) : (
              <>
                <p className="text-[10px] text-muted-foreground">
                  {isSelf
                    ? `${openConflicts.length} self-contradicting decision pair${openConflicts.length !== 1 ? "s" : ""}`
                    : `${openConflicts.length} conflicting pair${openConflicts.length !== 1 ? "s" : ""} — each row is one specific decision vs one specific decision`}
                </p>
                {(() => {
                  const sorted = [...openConflicts].sort((a, b) => {
                    const latestA = Math.max(
                      new Date(a.decided_at_a).getTime(),
                      new Date(a.decided_at_b).getTime(),
                    );
                    const latestB = Math.max(
                      new Date(b.decided_at_a).getTime(),
                      new Date(b.decided_at_b).getTime(),
                    );
                    return latestB - latestA;
                  });

                  // Track which decision IDs have already been shown on each
                  // side so we can mark repeats.
                  const seenLeft = new Set<string>();
                  const seenRight = new Set<string>();

                  return sorted.map((c) => {
                    const flip = c.agent_a !== group.agent_a;
                    const leftId = flip ? c.decision_b_id : c.decision_a_id;
                    const rightId = flip ? c.decision_a_id : c.decision_b_id;
                    const repeatedLeft = seenLeft.has(leftId);
                    const repeatedRight = seenRight.has(rightId);
                    seenLeft.add(leftId);
                    seenRight.add(rightId);
                    return (
                      <ConflictPairRow
                        key={c.id}
                        c={c}
                        groupAgentA={group.agent_a}
                        repeatedLeft={repeatedLeft}
                        repeatedRight={repeatedRight}
                        onAdjudicate={onAdjudicate}
                      />
                    );
                  });
                })()}
              </>
            )}
          </div>
        </CardContent>
      )}
    </Card>
  );
}

// ── Page ────────────────────────────────────────────────────────────────────

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

  const [adjudicateTarget, setAdjudicateTarget] =
    useState<DecisionConflict | null>(null);
  const [adjudicateStatus, setAdjudicateStatus] =
    useState<string>("acknowledged");
  const [adjudicateNote, setAdjudicateNote] = useState("");
  const [adjudicateWinner, setAdjudicateWinner] = useState<string | null>(null);
  const [adjudicateError, setAdjudicateError] = useState<string | null>(null);

  const { data, isPending } = useQuery({
    queryKey: ["conflict-groups", page, agentFilter, statusFilter],
    queryFn: () =>
      listConflictGroups({
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
        ...(agentFilter ? { agent_id: agentFilter } : {}),
        ...(statusFilter ? { status: statusFilter } : {}),
      }),
  });

  const groups = data?.conflict_groups;
  const totalPages = data ? Math.ceil(data.total / PAGE_SIZE) : 0;

  const adjudicateMutation = useMutation({
    mutationFn: (params: {
      id: string;
      status: string;
      resolution_note?: string;
      winning_decision_id?: string;
    }) =>
      patchConflict(params.id, {
        status: params.status,
        resolution_note: params.resolution_note,
        winning_decision_id: params.winning_decision_id,
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["conflict-groups"] });
      setAdjudicateTarget(null);
      setAdjudicateNote("");
      setAdjudicateWinner(null);
      setAdjudicateError(null);
    },
    onError: (err) => {
      setAdjudicateError(
        err instanceof ApiError ? err.message : "Failed to update conflict",
      );
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
      ...(adjudicateStatus === "resolved" && adjudicateWinner
        ? { winning_decision_id: adjudicateWinner }
        : {}),
    });
  }

  return (
    <div className="space-y-6 animate-page">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold tracking-tight">Conflicts</h1>
        {data?.total != null && data.total > 0 && (
          <Badge variant="outline">
            {data.total} group{data.total !== 1 ? "s" : ""}
          </Badge>
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
              <SelectItem value="all">All</SelectItem>
              <SelectItem value="open">Open</SelectItem>
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
            <Skeleton key={i} className="h-32 w-full" />
          ))}
        </div>
      ) : !groups?.length ? (
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
          <div className="space-y-3">
            {groups.map((group) => (
              <div key={group.id} className="animate-list-item">
                <ConflictGroupCard
                  group={group}
                  onAdjudicate={openAdjudicateDialog}
                />
              </div>
            ))}
          </div>

          {data && data.total > PAGE_SIZE && (
            <div className="flex items-center justify-between">
              <p className="text-sm text-muted-foreground">
                Showing {page * PAGE_SIZE + 1}&ndash;
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
      <Dialog
        open={adjudicateTarget !== null}
        onOpenChange={(open) => !open && setAdjudicateTarget(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Adjudicate Conflict</DialogTitle>
            <DialogDescription>
              Update the status of this conflict between{" "}
              <strong>{adjudicateTarget?.agent_a}</strong>
              {adjudicateTarget?.agent_a !== adjudicateTarget?.agent_b && (
                <>
                  {" "}
                  and <strong>{adjudicateTarget?.agent_b}</strong>
                </>
              )}{" "}
              on <strong>{adjudicateTarget?.decision_type}</strong>.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Action</label>
              <div className="flex gap-2">
                <Button
                  variant={
                    adjudicateStatus === "acknowledged" ? "default" : "outline"
                  }
                  size="sm"
                  onClick={() => setAdjudicateStatus("acknowledged")}
                >
                  <Eye className="h-3.5 w-3.5 mr-1.5" />
                  Acknowledge
                </Button>
                <Button
                  variant={
                    adjudicateStatus === "resolved" ? "default" : "outline"
                  }
                  size="sm"
                  onClick={() => setAdjudicateStatus("resolved")}
                >
                  <CheckCircle2 className="h-3.5 w-3.5 mr-1.5" />
                  Resolve
                </Button>
                <Button
                  variant={
                    adjudicateStatus === "wont_fix" ? "default" : "outline"
                  }
                  size="sm"
                  onClick={() => setAdjudicateStatus("wont_fix")}
                >
                  <XCircle className="h-3.5 w-3.5 mr-1.5" />
                  Won&apos;t Fix
                </Button>
              </div>
            </div>
            {adjudicateStatus === "resolved" && adjudicateTarget && (
              <div className="space-y-2">
                <label className="text-sm font-medium">
                  Winner (optional)
                </label>
                <div className="grid grid-cols-2 gap-2">
                  {(
                    [
                      {
                        id: adjudicateTarget.decision_a_id,
                        agent: adjudicateTarget.agent_a,
                        outcome: adjudicateTarget.outcome_a,
                      },
                      {
                        id: adjudicateTarget.decision_b_id,
                        agent: adjudicateTarget.agent_b,
                        outcome: adjudicateTarget.outcome_b,
                      },
                    ] as const
                  ).map((side) => (
                    <button
                      key={side.id}
                      type="button"
                      onClick={() =>
                        setAdjudicateWinner(
                          adjudicateWinner === side.id ? null : side.id,
                        )
                      }
                      className={`text-left rounded-md border p-3 text-xs transition-colors ${
                        adjudicateWinner === side.id
                          ? "border-primary bg-primary/5"
                          : "hover:border-primary/50 hover:bg-muted/50"
                      }`}
                    >
                      <div className="flex items-center justify-between mb-1.5">
                        <Badge
                          variant="outline"
                          className="font-mono text-[10px]"
                        >
                          {side.agent}
                        </Badge>
                        {adjudicateWinner === side.id && (
                          <Check className="h-3 w-3 text-primary" />
                        )}
                      </div>
                      <p className="leading-snug text-muted-foreground">
                        {truncate(side.outcome, 80)}
                      </p>
                    </button>
                  ))}
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
            <Button
              variant="outline"
              onClick={() => setAdjudicateTarget(null)}
            >
              Cancel
            </Button>
            <Button
              onClick={handleAdjudicate}
              disabled={adjudicateMutation.isPending}
            >
              <Check className="h-4 w-4 mr-1.5" />
              {adjudicateMutation.isPending ? "Saving\u2026" : "Save"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
