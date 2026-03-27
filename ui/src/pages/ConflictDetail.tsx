import { useParams, Link } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { getConflictDetail, patchConflict, ApiError } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { formatDate, formatRelativeTime } from "@/lib/utils";
import {
  ArrowLeft,
  CheckCircle2,
  Lightbulb,
  Swords,
  XCircle,
} from "lucide-react";
import { useState } from "react";

function truncate(text: string, maxLen: number): string {
  if (text.length <= maxLen) return text;
  return text.slice(0, maxLen).trimEnd() + "\u2026";
}

const severityColors: Record<string, string> = {
  critical: "text-red-400",
  high: "text-orange-400",
  medium: "text-yellow-400",
  low: "text-muted-foreground",
};

const statusConfig: Record<string, { label: string; variant: "default" | "secondary" | "destructive" | "success" | "warning" | "outline" }> = {
  open: { label: "Open", variant: "warning" },
  resolved: { label: "Resolved", variant: "success" },
  false_positive: { label: "False Positive", variant: "secondary" },
};

function DecisionSide({
  label,
  agent,
  outcome,
  reasoning,
  confidence,
  decidedAt,
  runId,
  isWinner,
}: {
  label: string;
  agent: string;
  outcome: string;
  reasoning: string | null;
  confidence: number;
  decidedAt: string;
  runId: string | null;
  isWinner: boolean;
}) {
  return (
    <Card className={isWinner ? "ring-1 ring-emerald-500/50" : ""}>
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">{label}</span>
            <Badge variant="outline" className="font-mono text-[10px]">
              {agent}
            </Badge>
            {isWinner && (
              <Badge variant="success" className="text-[10px]">
                Winner
              </Badge>
            )}
          </div>
          <Badge variant="secondary" className="text-xs">
            {(confidence * 100).toFixed(0)}%
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-sm leading-relaxed">{outcome}</p>
        {reasoning && (
          <div className="border-l-2 border-muted pl-3">
            <p className="text-xs text-muted-foreground leading-snug">{reasoning}</p>
          </div>
        )}
        <div className="flex items-center gap-3 text-[10px] text-muted-foreground">
          <span>{formatDate(decidedAt)}</span>
          {runId && (
            <Link to={`/decisions/${runId}`} className="text-primary hover:underline">
              View decision
            </Link>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

export default function ConflictDetail() {
  const { conflictId } = useParams<{ conflictId: string }>();
  const queryClient = useQueryClient();
  const [resolveNote, setResolveNote] = useState("");
  const [resolveWinner, setResolveWinner] = useState<string | null>(null);
  const [resolveError, setResolveError] = useState<string | null>(null);

  const { data: conflict, isPending, error } = useQuery({
    queryKey: ["conflict-detail", conflictId],
    queryFn: () => getConflictDetail(conflictId!),
    enabled: !!conflictId,
  });

  const resolveMutation = useMutation({
    mutationFn: (params: { status: string; resolution_note?: string; winning_decision_id?: string }) =>
      patchConflict(conflictId!, params),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["conflict-detail", conflictId] });
      queryClient.invalidateQueries({ queryKey: ["conflict-groups"] });
      setResolveNote("");
      setResolveWinner(null);
      setResolveError(null);
    },
    onError: (err) => {
      setResolveError(err instanceof ApiError ? err.message : "Failed to update conflict");
    },
  });

  if (isPending) {
    return (
      <div className="space-y-6 animate-page">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-48 w-full" />
        <div className="grid grid-cols-2 gap-4">
          <Skeleton className="h-64" />
          <Skeleton className="h-64" />
        </div>
      </div>
    );
  }

  if (error || !conflict) {
    return (
      <div className="space-y-4 animate-page">
        <Link to="/conflicts" className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeft className="h-4 w-4" /> Back to conflicts
        </Link>
        <div className="text-center py-14">
          <XCircle className="h-10 w-10 text-destructive/30 mx-auto mb-3" />
          <p className="text-sm text-muted-foreground">Conflict not found</p>
        </div>
      </div>
    );
  }

  const rec = conflict.recommendation;
  const statusCfg = statusConfig[conflict.status] ?? { label: conflict.status, variant: "outline" as const };
  const isOpen = conflict.status === "open";
  const winnerA = conflict.winning_decision_id === conflict.decision_a_id;
  const winnerB = conflict.winning_decision_id === conflict.decision_b_id;

  return (
    <div className="space-y-6 animate-page">
      {/* Header */}
      <div>
        <Link to="/conflicts" className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground mb-3">
          <ArrowLeft className="h-4 w-4" /> Back to conflicts
        </Link>
        <div className="flex items-start justify-between gap-4">
          <div className="space-y-1">
            <div className="flex items-center gap-2 flex-wrap">
              <Badge variant={statusCfg.variant}>{statusCfg.label}</Badge>
              {conflict.severity && (
                <span className={`text-xs font-semibold uppercase ${severityColors[conflict.severity] ?? ""}`}>
                  {conflict.severity}
                </span>
              )}
              <span className="font-semibold">{conflict.agent_a}</span>
              <Swords className="h-3.5 w-3.5 text-muted-foreground/40" />
              <span className="font-semibold">{conflict.agent_b}</span>
              <Badge variant={decisionTypeBadgeVariant(conflict.decision_type)} className="font-mono text-xs">
                {conflict.decision_type}
              </Badge>
            </div>
            {conflict.category && (
              <span className="text-xs text-muted-foreground">Category: {conflict.category}</span>
            )}
          </div>
          <span className="text-xs text-muted-foreground shrink-0">
            Detected {formatRelativeTime(conflict.detected_at)}
          </span>
        </div>
      </div>

      {/* Explanation */}
      {conflict.explanation && (
        <Card>
          <CardContent className="pt-5">
            <p className="text-sm leading-relaxed">{conflict.explanation}</p>
          </CardContent>
        </Card>
      )}

      {/* Side-by-side decisions */}
      <div className="grid grid-cols-1 md:grid-cols-[1fr,auto,1fr] gap-4 items-start">
        <DecisionSide
          label="Side A"
          agent={conflict.agent_a}
          outcome={conflict.outcome_a}
          reasoning={conflict.reasoning_a}
          confidence={conflict.confidence_a}
          decidedAt={conflict.decided_at_a}
          runId={conflict.run_a}
          isWinner={winnerA}
        />
        <div className="hidden md:flex items-center justify-center self-center">
          <span className="text-xs font-medium text-muted-foreground px-2">vs</span>
        </div>
        <DecisionSide
          label="Side B"
          agent={conflict.agent_b}
          outcome={conflict.outcome_b}
          reasoning={conflict.reasoning_b}
          confidence={conflict.confidence_b}
          decidedAt={conflict.decided_at_b}
          runId={conflict.run_b}
          isWinner={winnerB}
        />
      </div>

      {/* Recommendation */}
      {rec && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm flex items-center gap-1.5">
              <Lightbulb className="h-4 w-4 text-yellow-500" />
              Recommendation
              <Badge variant="secondary" className="text-[10px] ml-1">
                {(rec.confidence * 100).toFixed(0)}% confidence
              </Badge>
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            <p className="text-sm">
              Suggested winner: <strong>{rec.suggested_winner === conflict.decision_a_id ? conflict.agent_a : conflict.agent_b}</strong>
            </p>
            {rec.reasons.length > 0 && (
              <ul className="text-xs text-muted-foreground space-y-1">
                {rec.reasons.map((reason, i) => (
                  <li key={i} className="flex gap-1.5 items-start">
                    <span className="text-muted-foreground/50 shrink-0">•</span>
                    {reason}
                  </li>
                ))}
              </ul>
            )}
            {isOpen && (
              <Button
                variant="outline"
                size="sm"
                className="mt-2"
                onClick={() => {
                  setResolveWinner(rec.suggested_winner);
                  resolveMutation.mutate({
                    status: "resolved",
                    winning_decision_id: rec.suggested_winner,
                    resolution_note: "Accepted system recommendation",
                  });
                }}
              >
                <CheckCircle2 className="h-3 w-3 mr-1" />
                Accept recommendation
              </Button>
            )}
          </CardContent>
        </Card>
      )}

      {/* Resolution info (for resolved/false_positive conflicts) */}
      {!isOpen && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm flex items-center gap-1.5">
              <CheckCircle2 className="h-4 w-4 text-emerald-500" />
              Resolution
            </CardTitle>
          </CardHeader>
          <CardContent className="text-sm space-y-1">
            {conflict.resolved_by && (
              <p className="text-xs text-muted-foreground">Resolved by: {conflict.resolved_by}</p>
            )}
            {conflict.resolved_at && (
              <p className="text-xs text-muted-foreground">Resolved: {formatRelativeTime(conflict.resolved_at)}</p>
            )}
            {conflict.resolution_note && (
              <p className="text-sm mt-2">{conflict.resolution_note}</p>
            )}
          </CardContent>
        </Card>
      )}

      {/* Resolve actions (for open conflicts) */}
      {isOpen && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">Resolve this conflict</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Winner (optional)</label>
              <div className="grid grid-cols-2 gap-2">
                <Button
                  variant={resolveWinner === conflict.decision_a_id ? "default" : "outline"}
                  size="sm"
                  className="text-xs justify-start"
                  onClick={() => setResolveWinner(resolveWinner === conflict.decision_a_id ? null : conflict.decision_a_id)}
                >
                  {conflict.agent_a}: {truncate(conflict.outcome_a, 60)}
                </Button>
                <Button
                  variant={resolveWinner === conflict.decision_b_id ? "default" : "outline"}
                  size="sm"
                  className="text-xs justify-start"
                  onClick={() => setResolveWinner(resolveWinner === conflict.decision_b_id ? null : conflict.decision_b_id)}
                >
                  {conflict.agent_b}: {truncate(conflict.outcome_b, 60)}
                </Button>
              </div>
            </div>
            <div>
              <label className="text-xs text-muted-foreground mb-1 block">Resolution note</label>
              <Textarea
                value={resolveNote}
                onChange={(e) => setResolveNote(e.target.value)}
                placeholder="Why are you resolving this conflict?"
                className="text-sm"
                rows={2}
              />
            </div>
            {resolveError && (
              <p className="text-xs text-destructive">{resolveError}</p>
            )}
            <div className="flex gap-2">
              <Button
                size="sm"
                onClick={() =>
                  resolveMutation.mutate({
                    status: "resolved",
                    ...(resolveNote.trim() ? { resolution_note: resolveNote.trim() } : {}),
                    ...(resolveWinner ? { winning_decision_id: resolveWinner } : {}),
                  })
                }
                disabled={resolveMutation.isPending}
              >
                <CheckCircle2 className="h-3 w-3 mr-1" />
                Resolve
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() =>
                  resolveMutation.mutate({
                    status: "false_positive",
                    ...(resolveNote.trim() ? { resolution_note: resolveNote.trim() } : {}),
                  })
                }
                disabled={resolveMutation.isPending}
              >
                <XCircle className="h-3 w-3 mr-1" />
                False Positive
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Metadata */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm text-muted-foreground">Details</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-xs">
            <div>
              <dt className="text-muted-foreground">Conflict ID</dt>
              <dd className="font-mono">{conflict.id}</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Conflict kind</dt>
              <dd>{conflict.conflict_kind}</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Decision A</dt>
              <dd className="font-mono">{conflict.decision_a_id.slice(0, 8)}…</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Decision B</dt>
              <dd className="font-mono">{conflict.decision_b_id.slice(0, 8)}…</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Detected</dt>
              <dd>{formatDate(conflict.detected_at)}</dd>
            </div>
            {conflict.resolved_at && (
              <div>
                <dt className="text-muted-foreground">Resolved</dt>
                <dd>{formatDate(conflict.resolved_at)}</dd>
              </div>
            )}
          </dl>
        </CardContent>
      </Card>
    </div>
  );
}
