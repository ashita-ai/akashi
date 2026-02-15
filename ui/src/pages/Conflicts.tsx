import { Link, useSearchParams } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { listConflicts, patchConflict, ApiError } from "@/lib/api";
import type { DecisionConflict, ConflictStatus } from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
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
  ChevronLeft,
  ChevronRight,
  Eye,
  Swords,
  XCircle,
} from "lucide-react";
import { useState, type FormEvent } from "react";

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

function ConflictCard({
  conflict,
  onResolve,
}: {
  conflict: DecisionConflict;
  onResolve: (conflict: DecisionConflict) => void;
}) {
  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <AlertTriangle className="h-4 w-4 text-amber-500" />
              <span className="font-mono">{conflict.decision_type}</span>
            </CardTitle>
            <StatusBadge status={conflict.status} />
            <SeverityBadge severity={conflict.severity} />
            <CategoryBadge category={conflict.category} />
          </div>
          <span className="text-xs text-muted-foreground">
            Detected {formatDate(conflict.detected_at)}
          </span>
        </div>
        <div className="flex items-center justify-between mt-1">
          <p className="text-xs text-muted-foreground">
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
          {conflict.status === "open" && (
            <div className="flex gap-1">
              <Button
                variant="ghost"
                size="sm"
                className="h-7 text-xs"
                onClick={() => onResolve(conflict)}
              >
                <Eye className="h-3 w-3 mr-1" />
                Resolve
              </Button>
            </div>
          )}
        </div>
        {conflict.explanation && (
          <p className="text-xs text-muted-foreground mt-2 italic border-l-2 border-muted pl-2">
            {conflict.explanation}
          </p>
        )}
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
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();

  const page = Math.max(0, parseInt(searchParams.get("page") ?? "0", 10));
  const agentFilter = searchParams.get("agent") ?? "";
  const statusFilter = searchParams.get("status") ?? "";

  const [agentInput, setAgentInput] = useState(agentFilter);
  const [resolveTarget, setResolveTarget] = useState<DecisionConflict | null>(null);
  const [resolveStatus, setResolveStatus] = useState<string>("acknowledged");
  const [resolveNote, setResolveNote] = useState("");
  const [resolveError, setResolveError] = useState<string | null>(null);

  const { data, isPending } = useQuery({
    queryKey: ["conflicts", page, agentFilter, statusFilter],
    queryFn: () =>
      listConflicts({
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
        ...(agentFilter ? { agent_id: agentFilter } : {}),
      }),
  });

  // Client-side status filter (backend may not support status filter on list)
  const filteredConflicts = statusFilter
    ? data?.conflicts?.filter((c) => c.status === statusFilter)
    : data?.conflicts;

  const totalPages = data ? Math.ceil(data.total / PAGE_SIZE) : 0;

  const resolveMutation = useMutation({
    mutationFn: (params: { id: string; status: string; resolution_note?: string }) =>
      patchConflict(params.id, { status: params.status, resolution_note: params.resolution_note }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["conflicts"] });
      setResolveTarget(null);
      setResolveNote("");
      setResolveError(null);
    },
    onError: (err) => {
      setResolveError(err instanceof ApiError ? err.message : "Failed to update conflict");
    },
  });

  function applyFilter(e: FormEvent) {
    e.preventDefault();
    const params: Record<string, string> = {};
    if (agentInput.trim()) params.agent = agentInput.trim();
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

  function openResolveDialog(conflict: DecisionConflict) {
    setResolveTarget(conflict);
    setResolveStatus("acknowledged");
    setResolveNote("");
    setResolveError(null);
  }

  function handleResolve() {
    if (!resolveTarget) return;
    resolveMutation.mutate({
      id: resolveTarget.id,
      status: resolveStatus,
      ...(resolveNote.trim() ? { resolution_note: resolveNote.trim() } : {}),
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
        <Button type="submit" size="sm">
          Filter
        </Button>
        {(agentFilter || statusFilter) && (
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
                onResolve={openResolveDialog}
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

      {/* Resolution dialog */}
      <Dialog open={resolveTarget !== null} onOpenChange={(open) => !open && setResolveTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Resolve Conflict</DialogTitle>
            <DialogDescription>
              Update the status of this conflict between{" "}
              <strong>{resolveTarget?.agent_a}</strong>
              {resolveTarget?.agent_a !== resolveTarget?.agent_b && (
                <> and <strong>{resolveTarget?.agent_b}</strong></>
              )}
              {" on "}<strong>{resolveTarget?.decision_type}</strong>.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Action</label>
              <div className="flex gap-2">
                <Button
                  variant={resolveStatus === "acknowledged" ? "default" : "outline"}
                  size="sm"
                  onClick={() => setResolveStatus("acknowledged")}
                >
                  <Eye className="h-3.5 w-3.5 mr-1.5" />
                  Acknowledge
                </Button>
                <Button
                  variant={resolveStatus === "resolved" ? "default" : "outline"}
                  size="sm"
                  onClick={() => setResolveStatus("resolved")}
                >
                  <CheckCircle2 className="h-3.5 w-3.5 mr-1.5" />
                  Resolve
                </Button>
                <Button
                  variant={resolveStatus === "wont_fix" ? "default" : "outline"}
                  size="sm"
                  onClick={() => setResolveStatus("wont_fix")}
                >
                  <XCircle className="h-3.5 w-3.5 mr-1.5" />
                  Won't Fix
                </Button>
              </div>
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium">Note (optional)</label>
              <textarea
                className="w-full rounded-md border bg-background px-3 py-2 text-sm min-h-[80px] resize-none focus:outline-none focus:ring-2 focus:ring-ring"
                placeholder="Describe why this conflict was resolved this way..."
                value={resolveNote}
                onChange={(e) => setResolveNote(e.target.value)}
              />
            </div>
            {resolveError && (
              <p className="text-sm text-destructive">{resolveError}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setResolveTarget(null)}>
              Cancel
            </Button>
            <Button onClick={handleResolve} disabled={resolveMutation.isPending}>
              <Check className="h-4 w-4 mr-1.5" />
              {resolveMutation.isPending ? "Saving\u2026" : "Save"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
