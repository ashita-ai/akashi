import { useParams, Link } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { getRun, getDecision } from "@/lib/api";
import type { Decision, DecisionConflict, DecisionEnrichments, LineageEntry } from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDate, formatRelativeTime, truncate } from "@/lib/utils";
import {
  AlertTriangle,
  ArrowLeft,
  CheckCircle2,
  Clock,
  GitBranch,
  Hash,
  Shield,
  ShieldCheck,
  ShieldX,
  Trophy,
  XCircle,
} from "lucide-react";

// ---------------------------------------------------------------------------
// Precedent link — still fetches individually (single lookup, not per-decision)
// ---------------------------------------------------------------------------

function PrecedentLink({ decisionId }: { decisionId: string }) {
  const { data, isPending, error } = useQuery({
    queryKey: ["precedent", decisionId],
    queryFn: () => getDecision(decisionId),
    staleTime: 60_000,
    retry: false,
  });

  if (isPending) return <Skeleton className="h-4 w-32 inline-block" />;

  if (error || !data) {
    return (
      <span className="font-mono text-xs text-muted-foreground" title="Referenced decision not found">
        {decisionId.slice(0, 8)}… <span className="text-destructive">(not found)</span>
      </span>
    );
  }

  return (
    <Link
      to={`/decisions/${data.run_id}`}
      className="font-mono text-xs text-primary hover:underline inline-flex items-center gap-1"
    >
      <GitBranch className="h-3 w-3" />
      {decisionId.slice(0, 8)}…
    </Link>
  );
}

// ---------------------------------------------------------------------------
// Evidence source colors
// ---------------------------------------------------------------------------

const evidenceSourceColors: Record<string, string> = {
  tool_output: "border-l-cyan-500/60",
  api_response: "border-l-blue-500/60",
  document: "border-l-purple-500/60",
  agent_output: "border-l-emerald-500/60",
  user_input: "border-l-amber-500/60",
  search_result: "border-l-blue-400/60",
  memory: "border-l-pink-500/60",
  database_query: "border-l-cyan-600/60",
};

// ---------------------------------------------------------------------------
// Run status icons
// ---------------------------------------------------------------------------

const statusIcon = {
  running: <Clock className="h-4 w-4 text-amber-500" />,
  completed: <CheckCircle2 className="h-4 w-4 text-emerald-500" />,
  failed: <XCircle className="h-4 w-4 text-destructive" />,
};

// ---------------------------------------------------------------------------
// Integrity badge — reads from pre-fetched enrichments
// ---------------------------------------------------------------------------

function IntegrityBadge({ enrichments }: { enrichments?: DecisionEnrichments }) {
  if (!enrichments) return null;
  const { status } = enrichments.integrity;

  if (status === "verified") {
    return (
      <Badge variant="success" className="text-xs gap-1">
        <ShieldCheck className="h-3 w-3" />
        Verified
      </Badge>
    );
  }
  if (status === "tampered") {
    return (
      <Badge variant="destructive" className="text-xs gap-1">
        <ShieldX className="h-3 w-3" />
        Tampered
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="text-xs gap-1">
      <Shield className="h-3 w-3" />
      No hash
    </Badge>
  );
}

// ---------------------------------------------------------------------------
// Revision chain — reads from pre-fetched enrichments
// ---------------------------------------------------------------------------

function RevisionChain({ enrichments }: { enrichments?: DecisionEnrichments }) {
  if (!enrichments) return null;
  const { items, count, degraded } = enrichments.revisions;
  if (count <= 1 && !degraded) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <GitBranch className="h-4 w-4" />
          Revision History ({count} versions)
        </CardTitle>
      </CardHeader>
      <CardContent>
        {degraded && (
          <p className="text-xs text-amber-600 dark:text-amber-400 flex items-center gap-1 mb-3">
            <AlertTriangle className="h-3 w-3" />
            Revision data may be incomplete due to an authorization error.
          </p>
        )}
        <div className="relative space-y-3 pl-6 before:absolute before:left-[11px] before:top-2 before:h-[calc(100%-16px)] before:w-px before:bg-gradient-to-b before:from-primary/60 before:to-border">
          {items.map((rev: Decision, idx: number) => (
            <div key={rev.id} className="relative">
              <div className="absolute -left-6 top-1 h-2.5 w-2.5 rounded-full border-2 border-background bg-primary" />
              <div className="space-y-1">
                <div className="flex items-center gap-2">
                  <Badge variant={idx === 0 ? "default" : "outline"} className="text-xs">
                    {idx === 0 ? "Current" : `v${count - idx}`}
                  </Badge>
                  <span className="text-xs text-muted-foreground">
                    {formatDate(rev.valid_from)}
                  </span>
                  <Badge variant="secondary" className="text-xs">
                    {(rev.confidence * 100).toFixed(0)}%
                  </Badge>
                </div>
                <p className="text-sm">{rev.outcome}</p>
              </div>
            </div>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Conflict status helpers
// ---------------------------------------------------------------------------

const conflictStatusLabel: Record<string, string> = {
  open: "Open",
  resolved: "Resolved",
  false_positive: "False Positive",
};

const conflictStatusVariant: Record<string, "warning" | "secondary" | "success" | "outline"> = {
  open: "warning",
  resolved: "success",
  false_positive: "outline",
};

// ---------------------------------------------------------------------------
// Decision conflicts — reads from pre-fetched enrichments
// ---------------------------------------------------------------------------

function DecisionConflicts({ decisionId, enrichments }: { decisionId: string; enrichments?: DecisionEnrichments }) {
  if (!enrichments) return null;
  const { items, count, has_more, degraded } = enrichments.conflicts;
  if (count === 0 && !degraded) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <AlertTriangle className="h-4 w-4 text-amber-500" />
          Related Conflicts ({count})
        </CardTitle>
      </CardHeader>
      <CardContent>
        {degraded && (
          <p className="text-xs text-amber-600 dark:text-amber-400 flex items-center gap-1 mb-3">
            <AlertTriangle className="h-3 w-3" />
            Conflict data may be incomplete due to an authorization error.
          </p>
        )}
        <div className="space-y-3">
          {items.map((c: DecisionConflict) => {
            const isA = c.decision_a_id === decisionId;
            const otherAgent = isA ? c.agent_b : c.agent_a;
            const otherOutcome = isA ? c.outcome_b : c.outcome_a;
            const otherRunId = isA ? c.run_b : c.run_a;
            return (
              <div
                key={c.id ?? `${c.decision_a_id}-${c.decision_b_id}`}
                className="rounded-md border p-3 text-sm space-y-2"
              >
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">conflicts with</span>
                    <Badge variant="outline" className="font-mono text-xs">
                      {otherAgent}
                    </Badge>
                    {c.conflict_kind === "self_contradiction" && (
                      <Badge variant="outline" className="text-[10px] px-1.5 py-0">self</Badge>
                    )}
                  </div>
                  <div className="flex items-center gap-2">
                    <Badge
                      variant={conflictStatusVariant[c.status] ?? "secondary"}
                      className="text-xs"
                    >
                      {conflictStatusLabel[c.status] ?? c.status}
                    </Badge>
                    <span className="text-xs text-muted-foreground">
                      {formatRelativeTime(c.detected_at)}
                    </span>
                  </div>
                </div>
                <p className="text-xs text-muted-foreground leading-relaxed">
                  {truncate(otherOutcome, 160)}
                </p>
                <Link
                  to={`/decisions/${otherRunId}`}
                  className="text-xs text-primary hover:underline inline-flex items-center gap-1"
                >
                  View conflicting decision →
                </Link>
              </div>
            );
          })}
          {has_more && (
            <p className="text-xs text-muted-foreground italic text-center pt-2">
              More conflicts exist than are shown here.
            </p>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Lineage entry row
// ---------------------------------------------------------------------------

function LineageEntryRow({ entry }: { entry: LineageEntry }) {
  return (
    <Link
      to={`/decisions/${entry.run_id}`}
      className="flex items-start gap-3 rounded-md border p-3 text-sm hover:bg-accent/50 transition-colors"
    >
      <GitBranch className="h-4 w-4 mt-0.5 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1 space-y-1">
        <div className="flex items-center gap-2 flex-wrap">
          <Badge variant={decisionTypeBadgeVariant(entry.decision_type)} className="text-xs">
            {entry.decision_type}
          </Badge>
          <Badge variant="secondary" className="text-xs">
            {(entry.confidence * 100).toFixed(0)}%
          </Badge>
          <span className="font-mono text-xs text-muted-foreground">
            {entry.agent_id}
          </span>
          {entry.project && (
            <span className="font-mono text-xs text-muted-foreground">
              {entry.project}
            </span>
          )}
          <span className="text-xs text-muted-foreground ml-auto shrink-0">
            {formatRelativeTime(entry.created_at)}
          </span>
        </div>
        <p className="text-xs text-muted-foreground leading-relaxed">
          {truncate(entry.outcome, 200)}
        </p>
      </div>
    </Link>
  );
}

// ---------------------------------------------------------------------------
// Decision lineage — reads from pre-fetched enrichments
// ---------------------------------------------------------------------------

function DecisionLineageSection({ enrichments }: { enrichments?: DecisionEnrichments }) {
  if (!enrichments) return null;
  const lineage = enrichments.lineage;
  if (!lineage.preceded_by && (!lineage.cited_by || lineage.cited_by.length === 0)) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <GitBranch className="h-4 w-4" />
          Lineage
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div>
          <h4 className="text-xs font-medium text-muted-foreground mb-2">Preceded by</h4>
          {lineage.preceded_by ? (
            <LineageEntryRow entry={lineage.preceded_by} />
          ) : (
            <p className="text-xs text-muted-foreground italic">No precedents recorded</p>
          )}
        </div>

        <div>
          <h4 className="text-xs font-medium text-muted-foreground mb-2">
            Cited by
            {lineage.cited_by && lineage.cited_by.length > 0 && (
              <span className="ml-1 text-foreground">({lineage.cited_by.length}{lineage.cited_by_has_more ? "+" : ""})</span>
            )}
          </h4>
          {lineage.cited_by && lineage.cited_by.length > 0 ? (
            <div className="space-y-2">
              {lineage.cited_by.map((entry) => (
                <LineageEntryRow key={entry.id} entry={entry} />
              ))}
              {lineage.cited_by_has_more && (
                <p className="text-xs text-muted-foreground italic text-center pt-1">
                  More citations exist
                </p>
              )}
            </div>
          ) : (
            <p className="text-xs text-muted-foreground italic">Not yet cited</p>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Session context
// ---------------------------------------------------------------------------

function SessionContext({ decision }: { decision: Decision }) {
  const ctx = decision.metadata as Record<string, unknown> | null;
  if (!ctx) return null;

  const agentContext = ctx.agent_context as Record<string, unknown> | undefined;
  const sessionId = (ctx.session_id ?? agentContext?.session_id) as string | undefined;
  const tool = agentContext?.tool as string | undefined;
  const model = agentContext?.model as string | undefined;
  const project = (ctx.project ?? agentContext?.project ?? agentContext?.repo) as string | undefined;

  if (!sessionId && !tool && !model && !project) return null;

  return (
    <div className="space-y-2">
      <h4 className="text-xs font-medium text-muted-foreground mb-1">Session Context</h4>
      <div className="flex flex-wrap gap-2">
        {sessionId && (
          <Link
            to={`/sessions/${sessionId}`}
            className="inline-flex items-center gap-1 text-xs bg-muted rounded px-2 py-1 hover:bg-accent transition-colors"
          >
            <Hash className="h-3 w-3" />
            {sessionId.slice(0, 8)}...
          </Link>
        )}
        {tool && (
          <Badge variant="outline" className="text-xs">
            {tool}
          </Badge>
        )}
        {model && (
          <Badge variant="outline" className="text-xs">
            {model}
          </Badge>
        )}
        {project && (
          <Badge variant="outline" className="text-xs font-mono">
            {project}
          </Badge>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Outcome signals card — assessment summary, conflict fate, citation count,
// supersession velocity, consensus counts
// ---------------------------------------------------------------------------

function OutcomeSignals({ decision }: { decision: Decision }) {
  const hasFate = decision.conflict_fate && (decision.conflict_fate.won > 0 || decision.conflict_fate.lost > 0 || decision.conflict_fate.resolved_no_winner > 0);
  const hasAssessment = decision.assessment_summary && decision.assessment_summary.total > 0;
  const hasCitations = (decision.precedent_citation_count ?? 0) > 0;
  const hasSupersession = decision.supersession_velocity != null;
  const hasConsensus = (decision.agreement_count ?? 0) > 0 || (decision.conflict_count ?? 0) > 0;

  if (!hasFate && !hasAssessment && !hasCitations && !hasSupersession && !hasConsensus) return null;

  return (
    <div className="space-y-2">
      <h4 className="text-xs font-medium text-muted-foreground mb-1">Outcome Signals</h4>
      <div className="flex flex-wrap gap-3">
        {/* Assessment summary */}
        {hasAssessment && (
          <div className="flex items-center gap-1.5 text-xs">
            <Trophy className="h-3 w-3 text-amber-500" />
            <span>
              {decision.assessment_summary!.correct} correct
              {decision.assessment_summary!.incorrect > 0 && `, ${decision.assessment_summary!.incorrect} incorrect`}
              {decision.assessment_summary!.partially_correct > 0 && `, ${decision.assessment_summary!.partially_correct} partial`}
              <span className="text-muted-foreground ml-1">({decision.assessment_summary!.total} total)</span>
            </span>
          </div>
        )}

        {/* Conflict fate */}
        {hasFate && (
          <div className="flex items-center gap-1.5 text-xs">
            <span className="font-medium">Conflict record:</span>
            <span className="text-emerald-600">{decision.conflict_fate!.won}W</span>
            <span className="text-destructive">{decision.conflict_fate!.lost}L</span>
            {decision.conflict_fate!.resolved_no_winner > 0 && (
              <span className="text-muted-foreground">{decision.conflict_fate!.resolved_no_winner}D</span>
            )}
          </div>
        )}

        {/* Citation count */}
        {hasCitations && (
          <div className="text-xs">
            <span className="font-medium">Cited {decision.precedent_citation_count} time{decision.precedent_citation_count !== 1 ? "s" : ""}</span>
          </div>
        )}

        {/* Supersession velocity */}
        {hasSupersession && (
          <div className="text-xs text-muted-foreground">
            Superseded after {decision.supersession_velocity! < 1
              ? `${Math.round(decision.supersession_velocity! * 60)}m`
              : `${decision.supersession_velocity!.toFixed(1)}h`}
          </div>
        )}

        {/* Consensus counts */}
        {hasConsensus && (
          <div className="text-xs text-muted-foreground">
            {(decision.agreement_count ?? 0) > 0 && <span className="text-emerald-600">{decision.agreement_count} agree</span>}
            {(decision.agreement_count ?? 0) > 0 && (decision.conflict_count ?? 0) > 0 && " / "}
            {(decision.conflict_count ?? 0) > 0 && <span className="text-amber-600">{decision.conflict_count} conflict</span>}
          </div>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main page component
// ---------------------------------------------------------------------------

export default function DecisionDetail() {
  const { runId } = useParams<{ runId: string }>();

  const { data: run, isPending, error } = useQuery({
    queryKey: ["run", runId],
    queryFn: () => getRun(runId!, { includeEnrichments: true }),
    enabled: !!runId,
  });

  if (isPending) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }

  if (error || !run) {
    return (
      <div className="space-y-4">
        <Link to="/decisions" className="flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          Back to decisions
        </Link>
        <p className="text-destructive">
          {error instanceof Error ? error.message : "Run not found"}
        </p>
      </div>
    );
  }

  const enrichmentsMap = run.decision_enrichments;

  return (
    <div className="space-y-8 animate-page">
      <div className="page-header flex items-center gap-4">
        <Link to="/decisions" className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors">
          <ArrowLeft className="h-3.5 w-3.5" />
          Decisions
        </Link>
        <span className="text-muted-foreground/30">/</span>
        <h1 className="text-2xl font-semibold">Agent Run</h1>
      </div>

      {/* Run header */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="font-mono text-sm">
              {run.id}
            </CardTitle>
            <div className="flex items-center gap-2">
              {statusIcon[run.status]}
              <Badge
                variant={
                  run.status === "completed"
                    ? "success"
                    : run.status === "failed"
                      ? "destructive"
                      : "warning"
                }
              >
                {run.status}
              </Badge>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
            <div>
              <dt className="text-muted-foreground">Agent</dt>
              <dd className="font-medium">{run.agent_id}</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Started</dt>
              <dd>{formatDate(run.started_at)}</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Completed</dt>
              <dd>{run.completed_at ? formatDate(run.completed_at) : "\u2014"}</dd>
            </div>
            {run.trace_id && (
              <div>
                <dt className="text-muted-foreground">Trace ID</dt>
                <dd className="font-mono text-xs">{run.trace_id}</dd>
              </div>
            )}
          </dl>
        </CardContent>
      </Card>

      {/* Events timeline */}
      {run.events && run.events.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Event Timeline</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="relative space-y-4 pl-6 before:absolute before:left-[11px] before:top-2 before:h-[calc(100%-16px)] before:w-px before:bg-gradient-to-b before:from-primary/60 before:to-border">
              {run.events.map((event) => (
                <div key={event.id} className="relative">
                  <div className="absolute -left-6 top-1 h-2.5 w-2.5 rounded-full border-2 border-background bg-primary" />
                  <div className="space-y-1">
                    <div className="flex items-center gap-2">
                      <Badge variant="outline" className="text-xs">
                        {event.event_type}
                      </Badge>
                      <span className="text-xs text-muted-foreground">
                        #{event.sequence_num}
                      </span>
                      <span className="text-xs text-muted-foreground">
                        {formatDate(event.occurred_at)}
                      </span>
                    </div>
                    {event.payload &&
                      Object.keys(event.payload).length > 0 && (
                        <pre className="rounded-md bg-muted p-2 text-xs overflow-x-auto">
                          {JSON.stringify(event.payload, null, 2)}
                        </pre>
                      )}
                  </div>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Enrichment truncation warning */}
      {run.truncated_enrichments && (
        <div className="rounded-md border border-amber-200 bg-amber-50 dark:border-amber-800 dark:bg-amber-950 p-3 flex items-center gap-2 text-sm text-amber-700 dark:text-amber-300">
          <AlertTriangle className="h-4 w-4 shrink-0" />
          Enrichments were computed for the first {run.enriched_count} of {run.decisions?.length} decisions.
        </div>
      )}

      {/* Decisions */}
      {run.decisions && run.decisions.length > 0 && (
        <>
          {run.decisions.map((decision) => {
            const enrichments = enrichmentsMap?.[decision.id];
            return (
              <Card key={decision.id}>
                <CardHeader>
                  <div className="flex items-center justify-between">
                    <CardTitle className="text-sm font-medium">
                      Decision: {decision.decision_type}
                    </CardTitle>
                    <div className="flex items-center gap-2">
                      <IntegrityBadge enrichments={enrichments} />
                      <Badge variant={decisionTypeBadgeVariant(decision.decision_type)}>
                        {(decision.confidence * 100).toFixed(0)}% confidence
                      </Badge>
                    </div>
                  </div>
                </CardHeader>
                <CardContent className="space-y-4">
                  <div>
                    <h4 className="text-xs font-medium text-muted-foreground mb-1">
                      Outcome
                    </h4>
                    <p className="text-sm">{decision.outcome}</p>
                  </div>

                  {/* Decision metadata grid */}
                  <div className="grid grid-cols-2 gap-x-6 gap-y-2 text-sm sm:grid-cols-4">
                    {decision.project && (
                      <div>
                        <dt className="text-xs text-muted-foreground">Project</dt>
                        <dd className="font-mono text-xs">{decision.project}</dd>
                      </div>
                    )}
                    <div>
                      <dt className="text-xs text-muted-foreground">Completeness</dt>
                      <dd className="font-mono text-xs">{(decision.completeness_score * 100).toFixed(0)}%</dd>
                    </div>
                    {decision.outcome_score != null && (
                      <div>
                        <dt className="text-xs text-muted-foreground">Outcome Score</dt>
                        <dd className="font-mono text-xs">{(decision.outcome_score * 100).toFixed(0)}%</dd>
                      </div>
                    )}
                    <div>
                      <dt className="text-xs text-muted-foreground">Valid From</dt>
                      <dd className="text-xs">{formatDate(decision.valid_from)}</dd>
                    </div>
                    {decision.valid_to && (
                      <div>
                        <dt className="text-xs text-muted-foreground">Valid To</dt>
                        <dd className="text-xs">{formatDate(decision.valid_to)}</dd>
                      </div>
                    )}
                    <div>
                      <dt className="text-xs text-muted-foreground">Transaction Time</dt>
                      <dd className="text-xs">{formatDate(decision.transaction_time)}</dd>
                    </div>
                    {enrichments?.integrity.content_hash && (
                      <div className="col-span-2">
                        <dt className="text-xs text-muted-foreground">Content Hash</dt>
                        <dd className="font-mono text-[10px] text-muted-foreground break-all">{enrichments.integrity.content_hash}</dd>
                      </div>
                    )}
                    {decision.precedent_ref && (
                      <div className="col-span-2">
                        <dt className="text-xs text-muted-foreground">Precedent</dt>
                        <dd><PrecedentLink decisionId={decision.precedent_ref} /></dd>
                      </div>
                    )}
                    {decision.precedent_reason && (
                      <div className="col-span-2">
                        <dt className="text-xs text-muted-foreground">Precedent Reason</dt>
                        <dd className="text-xs">{decision.precedent_reason}</dd>
                      </div>
                    )}
                  </div>

                  {/* Outcome signals */}
                  <OutcomeSignals decision={decision} />

                  {decision.reasoning && (
                    <div>
                      <h4 className="text-xs font-medium text-muted-foreground mb-1">
                        Reasoning
                      </h4>
                      <p className="text-sm whitespace-pre-wrap">
                        {decision.reasoning}
                      </p>
                    </div>
                  )}

                  {/* Session context */}
                  <SessionContext decision={decision} />

                  {/* Alternatives */}
                  {decision.alternatives && decision.alternatives.length > 0 && (
                    <div>
                      <h4 className="text-xs font-medium text-muted-foreground mb-2">
                        Alternatives
                      </h4>
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>Option</TableHead>
                            <TableHead>Rejection Reason</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {decision.alternatives.map((alt) => (
                            <TableRow key={alt.id}>
                              <TableCell className="font-medium">
                                {alt.label}
                              </TableCell>
                              <TableCell className="text-sm text-muted-foreground">
                                {alt.rejection_reason ?? "\u2014"}
                              </TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                    </div>
                  )}

                  {/* Evidence */}
                  {decision.evidence && decision.evidence.length > 0 && (
                    <div>
                      <h4 className="text-xs font-medium text-muted-foreground mb-2">
                        Evidence
                      </h4>
                      <div className="space-y-2">
                        {decision.evidence.map((ev) => (
                          <div
                            key={ev.id}
                            className={cn("rounded-md border border-l-[3px] p-3 text-sm", evidenceSourceColors[ev.source_type] ?? "border-l-border")}
                          >
                            <div className="flex items-center gap-2 mb-1">
                              <Badge variant="outline" className="text-xs">
                                {ev.source_type}
                              </Badge>
                              {ev.relevance_score != null && (
                                <span className="text-xs text-muted-foreground">
                                  relevance:{" "}
                                  {(ev.relevance_score * 100).toFixed(0)}%
                                </span>
                              )}
                            </div>
                            {["tool_output", "api_response", "database_query"].includes(ev.source_type)
                              ? (
                                <pre className="mt-2 rounded-md bg-muted px-3 py-2 text-xs font-mono overflow-x-auto whitespace-pre-wrap leading-relaxed">
                                  {ev.content}
                                </pre>
                              )
                              : (
                                <p className="mt-1 whitespace-pre-wrap text-sm">{ev.content}</p>
                              )
                            }
                            {ev.source_uri && (
                              <p className="mt-1 text-xs text-muted-foreground font-mono">
                                {ev.source_uri}
                              </p>
                            )}
                          </div>
                        ))}
                      </div>
                    </div>
                  )}

                  {/* Revision chain */}
                  <RevisionChain enrichments={enrichments} />

                  {/* Lineage */}
                  <DecisionLineageSection enrichments={enrichments} />

                  {/* Related conflicts */}
                  <DecisionConflicts decisionId={decision.id} enrichments={enrichments} />
                </CardContent>
              </Card>
            );
          })}
        </>
      )}
    </div>
  );
}
