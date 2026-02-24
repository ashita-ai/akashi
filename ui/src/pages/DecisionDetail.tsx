import { useParams, Link } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { getRun, getDecisionRevisions, getDecisionConflicts, verifyDecisionIntegrity } from "@/lib/api";
import type { Decision, DecisionConflict } from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDate, formatRelativeTime } from "@/lib/utils";
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
  XCircle,
} from "lucide-react";

const statusIcon = {
  running: <Clock className="h-4 w-4 text-amber-500" />,
  completed: <CheckCircle2 className="h-4 w-4 text-emerald-500" />,
  failed: <XCircle className="h-4 w-4 text-destructive" />,
};

function IntegrityBadge({ decisionId }: { decisionId: string }) {
  const { data, isPending } = useQuery({
    queryKey: ["integrity", decisionId],
    queryFn: () => verifyDecisionIntegrity(decisionId),
    staleTime: 60_000,
  });

  if (isPending) return <Skeleton className="h-5 w-20 inline-block" />;
  if (!data) return null;

  if (data.status === "verified") {
    return (
      <Badge variant="success" className="text-xs gap-1">
        <ShieldCheck className="h-3 w-3" />
        Verified
      </Badge>
    );
  }
  if (data.status === "tampered") {
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

function RevisionChain({ decisionId }: { decisionId: string }) {
  const { data, isPending } = useQuery({
    queryKey: ["revisions", decisionId],
    queryFn: () => getDecisionRevisions(decisionId),
    staleTime: 30_000,
  });

  if (isPending) return <Skeleton className="h-24 w-full" />;
  if (!data || data.count <= 1) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <GitBranch className="h-4 w-4" />
          Revision History ({data.count} versions)
        </CardTitle>
      </CardHeader>
      <CardContent>
        <div className="relative space-y-3 pl-6 before:absolute before:left-[11px] before:top-2 before:h-[calc(100%-16px)] before:w-px before:bg-border">
          {data.revisions.map((rev: Decision, idx: number) => (
            <div key={rev.id} className="relative">
              <div className="absolute -left-6 top-1 h-2.5 w-2.5 rounded-full border-2 border-background bg-primary" />
              <div className="space-y-1">
                <div className="flex items-center gap-2">
                  <Badge variant={idx === 0 ? "default" : "outline"} className="text-xs">
                    {idx === 0 ? "Current" : `v${data.count - idx}`}
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

function DecisionConflicts({ decisionId }: { decisionId: string }) {
  const { data, isPending } = useQuery({
    queryKey: ["decision-conflicts", decisionId],
    queryFn: () => getDecisionConflicts(decisionId),
    staleTime: 30_000,
  });

  if (isPending) return <Skeleton className="h-24 w-full" />;
  if (!data || data.total === 0) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <AlertTriangle className="h-4 w-4 text-amber-500" />
          Related Conflicts ({data.total})
        </CardTitle>
      </CardHeader>
      <CardContent>
        <div className="space-y-3">
          {data.conflicts.map((c: DecisionConflict) => (
            <div
              key={c.id ?? `${c.decision_a_id}-${c.decision_b_id}`}
              className="rounded-md border p-3 text-sm"
            >
              <div className="flex items-center justify-between mb-1">
                <div className="flex items-center gap-2">
                  <Badge variant="outline" className="font-mono text-xs">
                    {c.agent_a}
                  </Badge>
                  <span className="text-xs text-muted-foreground">vs</span>
                  <Badge variant="outline" className="font-mono text-xs">
                    {c.agent_b}
                  </Badge>
                </div>
                <div className="flex items-center gap-2">
                  <Badge
                    variant={
                      c.status === "open"
                        ? "warning"
                        : c.status === "resolved"
                          ? "success"
                          : "secondary"
                    }
                    className="text-xs"
                  >
                    {c.status}
                  </Badge>
                  <span className="text-xs text-muted-foreground">
                    {formatRelativeTime(c.detected_at)}
                  </span>
                </div>
              </div>
              {c.explanation && (
                <p className="text-xs text-muted-foreground italic mt-1">
                  {c.explanation}
                </p>
              )}
            </div>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

function SessionContext({ decision }: { decision: Decision }) {
  const ctx = decision.metadata as Record<string, unknown> | null;
  if (!ctx) return null;

  // Extract agent_context fields (session_id, tool, model, project)
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

export default function DecisionDetail() {
  const { runId } = useParams<{ runId: string }>();

  const { data: run, isPending, error } = useQuery({
    queryKey: ["run", runId],
    queryFn: () => getRun(runId!),
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

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-4">
        <Link to="/decisions" className="flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          Back
        </Link>
        <h1 className="text-2xl font-bold tracking-tight">Run Detail</h1>
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
            <div className="relative space-y-4 pl-6 before:absolute before:left-[11px] before:top-2 before:h-[calc(100%-16px)] before:w-px before:bg-border">
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

      {/* Decisions */}
      {run.decisions && run.decisions.length > 0 && (
        <>
          {run.decisions.map((decision) => (
            <Card key={decision.id}>
              <CardHeader>
                <div className="flex items-center justify-between">
                  <CardTitle className="text-sm font-medium">
                    Decision: {decision.decision_type}
                  </CardTitle>
                  <div className="flex items-center gap-2">
                    <IntegrityBadge decisionId={decision.id} />
                    <Badge variant="secondary">
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
                          <TableHead className="text-right">Score</TableHead>
                          <TableHead>Selected</TableHead>
                          <TableHead>Rejection Reason</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {decision.alternatives.map((alt) => (
                          <TableRow key={alt.id}>
                            <TableCell className="font-medium">
                              {alt.label}
                            </TableCell>
                            <TableCell className="text-right font-mono">
                              {alt.score != null
                                ? (alt.score * 100).toFixed(0) + "%"
                                : "\u2014"}
                            </TableCell>
                            <TableCell>
                              {alt.selected ? (
                                <CheckCircle2 className="h-4 w-4 text-emerald-500" />
                              ) : (
                                <span className="text-muted-foreground">\u2014</span>
                              )}
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
                          className="rounded-md border p-3 text-sm"
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
                          <p className="whitespace-pre-wrap">{ev.content}</p>
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
                <RevisionChain decisionId={decision.id} />

                {/* Related conflicts */}
                <DecisionConflicts decisionId={decision.id} />
              </CardContent>
            </Card>
          ))}
        </>
      )}
    </div>
  );
}
