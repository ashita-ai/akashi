import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { searchDecisions } from "@/lib/api";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge, decisionTypeBadgeVariant } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { formatRelativeTime } from "@/lib/utils";
import { Search, Gauge, Brain, Wrench, FolderOpen, Cpu } from "lucide-react";

export default function SearchPage() {
  const [query, setQuery] = useState("");
  const [submittedQuery, setSubmittedQuery] = useState("");

  const { data, isPending, isFetched } = useQuery({
    queryKey: ["search", submittedQuery],
    queryFn: () => searchDecisions(submittedQuery, true),
    enabled: submittedQuery.length > 0,
  });

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (query.trim()) {
      setSubmittedQuery(query.trim());
    }
  }

  return (
    <div className="space-y-8 animate-page">
      <div className="page-header">
        <h1 className="text-2xl font-semibold">Search</h1>
        <p className="mt-1 text-sm text-muted-foreground">Semantic and keyword search across all decisions</p>
      </div>

      <form onSubmit={handleSubmit} className="flex gap-3">
        <div className="relative flex-1">
          <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search decisions"
            className="pl-10 dark:focus-visible:shadow-glow-sm"
            autoFocus
          />
        </div>
        <Button type="submit" disabled={!query.trim()}>
          Search
        </Button>
      </form>

      {!submittedQuery && (
        <p className="text-sm text-muted-foreground py-8 text-center">
          Enter a query and press Search to find decisions.
        </p>
      )}

      {isPending && submittedQuery && (
        <div className="space-y-3">
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} className="h-20 w-full" />
          ))}
        </div>
      )}

      {isFetched && !data?.results?.length && submittedQuery && (
        <p className="text-sm text-muted-foreground py-8 text-center">
          No results found for &quot;{submittedQuery}&quot;.
        </p>
      )}

      {data?.results && data.results.length > 0 && (
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">
            {data.total} result{data.total !== 1 ? "s" : ""}
          </p>
          {data.results.map((result) => {
            const d = result.decision;
            const relevance = Math.round(result.similarity_score * 100);
            return (
              <Link
                key={d.id}
                to={`/decisions/${d.run_id}`}
                className="animate-list-item block"
              >
                <Card className="transition-all duration-200 hover:bg-accent/50 hover:shadow-glow-sm">
                  <CardContent className="p-4">
                    <div className="space-y-2 min-w-0">
                      <div className="flex items-center justify-between gap-2">
                        <div className="flex items-center gap-2 flex-wrap min-w-0">
                          <Badge variant="outline" className="font-mono text-xs">
                            {d.agent_id}
                          </Badge>
                          <Badge variant={decisionTypeBadgeVariant(d.decision_type)}>
                            {d.decision_type}
                          </Badge>
                          {d.project && (
                            <Badge variant="secondary" className="text-xs gap-1">
                              <FolderOpen className="h-3 w-3" />
                              {d.project}
                            </Badge>
                          )}
                        </div>
                        <span className="text-xs font-medium tabular-nums text-muted-foreground shrink-0" title={`Relevance: ${relevance}%`}>
                          {relevance}%
                        </span>
                      </div>
                      <p className="text-sm font-medium">{d.outcome}</p>
                      {d.reasoning && (
                        <p className="text-xs text-muted-foreground line-clamp-2">
                          {d.reasoning}
                        </p>
                      )}
                      <div className="flex items-center gap-3 text-xs text-muted-foreground flex-wrap">
                        <span className="flex items-center gap-1" title={`Confidence: ${Math.round(d.confidence * 100)}%`}>
                          <Gauge className="h-3 w-3" />
                          {Math.round(d.confidence * 100)}%
                        </span>
                        {d.completeness_score > 0 && (
                          <span className="flex items-center gap-1" title={`Completeness: ${Math.round(d.completeness_score * 100)}%`}>
                            <Brain className="h-3 w-3" />
                            {Math.round(d.completeness_score * 100)}%
                          </span>
                        )}
                        {d.tool && (
                          <span className="flex items-center gap-1">
                            <Wrench className="h-3 w-3" />
                            {d.tool}
                          </span>
                        )}
                        {d.model && (
                          <span className="flex items-center gap-1">
                            <Cpu className="h-3 w-3" />
                            {d.model}
                          </span>
                        )}
                        <span className="ml-auto shrink-0">
                          {formatRelativeTime(d.created_at)}
                        </span>
                      </div>
                    </div>
                  </CardContent>
                </Card>
              </Link>
            );
          })}
        </div>
      )}
    </div>
  );
}
