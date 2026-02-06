import { useQuery } from "@tanstack/react-query";
import { listConflicts } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate } from "@/lib/utils";
import { AlertTriangle } from "lucide-react";

export default function Conflicts() {
  const { data, isPending } = useQuery({
    queryKey: ["conflicts"],
    queryFn: () => listConflicts({ limit: 100 }),
  });

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold tracking-tight">Conflicts</h1>

      {isPending ? (
        <div className="space-y-4">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-40 w-full" />
          ))}
        </div>
      ) : !data?.conflicts?.length ? (
        <div className="flex flex-col items-center py-12 text-center">
          <AlertTriangle className="h-12 w-12 text-muted-foreground/30 mb-4" />
          <p className="text-sm text-muted-foreground">
            No conflicts detected. Agents are in agreement.
          </p>
        </div>
      ) : (
        <div className="space-y-4">
          {data.conflicts.map((conflict, idx) => (
            <Card key={`${conflict.decision_a_id}-${conflict.decision_b_id}-${idx}`}>
              <CardHeader>
                <div className="flex items-center justify-between">
                  <CardTitle className="flex items-center gap-2 text-sm">
                    <AlertTriangle className="h-4 w-4 text-amber-500" />
                    {conflict.decision_type} Conflict
                  </CardTitle>
                  <span className="text-xs text-muted-foreground">
                    Detected {formatDate(conflict.detected_at)}
                  </span>
                </div>
              </CardHeader>
              <CardContent>
                <div className="grid gap-4 sm:grid-cols-2">
                  {/* Decision A */}
                  <div className="space-y-2 rounded-md border p-3">
                    <div className="flex items-center justify-between">
                      <Badge variant="outline" className="font-mono text-xs">
                        {conflict.agent_a}
                      </Badge>
                      <Badge variant="secondary">
                        {(conflict.confidence_a * 100).toFixed(0)}%
                      </Badge>
                    </div>
                    <p className="text-sm font-medium">{conflict.outcome_a}</p>
                    <p className="text-xs text-muted-foreground">
                      {formatDate(conflict.decided_at_a)}
                    </p>
                  </div>

                  {/* Decision B */}
                  <div className="space-y-2 rounded-md border p-3">
                    <div className="flex items-center justify-between">
                      <Badge variant="outline" className="font-mono text-xs">
                        {conflict.agent_b}
                      </Badge>
                      <Badge variant="secondary">
                        {(conflict.confidence_b * 100).toFixed(0)}%
                      </Badge>
                    </div>
                    <p className="text-sm font-medium">{conflict.outcome_b}</p>
                    <p className="text-xs text-muted-foreground">
                      {formatDate(conflict.decided_at_b)}
                    </p>
                  </div>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
