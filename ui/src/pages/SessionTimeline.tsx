import { useParams, Link } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { getSession } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate, formatRelativeTime } from "@/lib/utils";
import { ArrowLeft, Clock, FileText } from "lucide-react";

export default function SessionTimeline() {
  const { sessionId } = useParams<{ sessionId: string }>();

  const { data, isPending, error } = useQuery({
    queryKey: ["session", sessionId],
    queryFn: () => getSession(sessionId!),
    enabled: !!sessionId,
  });

  if (isPending) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-32 w-full" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }

  if (error || !data) {
    return (
      <div className="space-y-4">
        <Link to="/" className="flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          Back to dashboard
        </Link>
        <p className="text-destructive">
          {error instanceof Error ? error.message : "Session not found"}
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-4">
        <Link to="/" className="flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          Back
        </Link>
        <h1 className="text-2xl font-bold tracking-tight">Session Timeline</h1>
      </div>

      {/* Session summary */}
      <Card>
        <CardHeader>
          <CardTitle className="font-mono text-sm">
            {data.session_id}
          </CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
            <div>
              <dt className="text-muted-foreground">Decisions</dt>
              <dd className="text-2xl font-bold">{data.decision_count}</dd>
            </div>
            {data.summary && (
              <>
                <div>
                  <dt className="text-muted-foreground">Duration</dt>
                  <dd className="font-medium">
                    {data.summary.duration_secs < 60
                      ? `${Math.round(data.summary.duration_secs)}s`
                      : data.summary.duration_secs < 3600
                        ? `${Math.round(data.summary.duration_secs / 60)}m`
                        : `${(data.summary.duration_secs / 3600).toFixed(1)}h`}
                  </dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Avg Confidence</dt>
                  <dd className="font-medium">
                    {(data.summary.avg_confidence * 100).toFixed(0)}%
                  </dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Started</dt>
                  <dd>{formatRelativeTime(data.summary.started_at)}</dd>
                </div>
              </>
            )}
          </dl>
          {data.summary?.decision_types && Object.keys(data.summary.decision_types).length > 0 && (
            <div className="mt-4 flex flex-wrap gap-2">
              {Object.entries(data.summary.decision_types).map(([type, count]) => (
                <Badge key={type} variant="secondary" className="text-xs">
                  {type}: {count}
                </Badge>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Decision timeline */}
      {data.decisions.length === 0 ? (
        <div className="flex flex-col items-center py-12 text-center">
          <FileText className="h-12 w-12 text-muted-foreground/30 mb-4" />
          <p className="text-sm text-muted-foreground">
            No decisions in this session.
          </p>
        </div>
      ) : (
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Decisions</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="relative space-y-4 pl-6 before:absolute before:left-[11px] before:top-2 before:h-[calc(100%-16px)] before:w-px before:bg-border">
              {data.decisions.map((decision) => (
                <Link
                  key={decision.id}
                  to={`/decisions/${decision.run_id}`}
                  className="relative block rounded-md border p-4 transition-colors hover:bg-accent"
                >
                  <div className="absolute -left-6 top-5 h-2.5 w-2.5 rounded-full border-2 border-background bg-primary" />
                  <div className="space-y-2">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <Badge variant="outline" className="font-mono text-xs">
                          {decision.agent_id}
                        </Badge>
                        <Badge variant="secondary" className="text-xs">
                          {decision.decision_type}
                        </Badge>
                      </div>
                      <div className="flex items-center gap-2">
                        <Badge variant="secondary" className="text-xs">
                          {(decision.confidence * 100).toFixed(0)}%
                        </Badge>
                        <span className="flex items-center gap-1 text-xs text-muted-foreground">
                          <Clock className="h-3 w-3" />
                          {formatDate(decision.created_at)}
                        </span>
                      </div>
                    </div>
                    <p className="text-sm font-medium">{decision.outcome}</p>
                    {decision.reasoning && (
                      <p className="text-xs text-muted-foreground line-clamp-2">
                        {decision.reasoning}
                      </p>
                    )}
                  </div>
                </Link>
              ))}
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
