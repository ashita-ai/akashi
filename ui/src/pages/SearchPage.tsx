import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { searchDecisions } from "@/lib/api";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate } from "@/lib/utils";
import { Search } from "lucide-react";

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
    <div className="space-y-6">
      <h1 className="text-2xl font-bold tracking-tight">Search</h1>

      <form onSubmit={handleSubmit} className="flex gap-3">
        <div className="relative flex-1">
          <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search decisions"
            className="pl-10"
            autoFocus
          />
        </div>
        <Button type="submit" disabled={!query.trim()}>
          Search
        </Button>
      </form>

      {isPending && (
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
          {data.results.map((result) => (
            <Link
              key={result.decision.id}
              to={`/decisions/${result.decision.run_id}`}
            >
              <Card className="transition-colors hover:bg-accent/50">
                <CardContent className="p-4">
                  <div className="space-y-1.5 min-w-0">
                    <div className="flex items-center gap-2">
                      <Badge variant="outline" className="font-mono text-xs">
                        {result.decision.agent_id}
                      </Badge>
                      <Badge variant="secondary">
                        {result.decision.decision_type}
                      </Badge>
                    </div>
                    <p className="text-sm font-medium">
                      {result.decision.outcome}
                    </p>
                    {result.decision.reasoning && (
                      <p className="text-xs text-muted-foreground line-clamp-2">
                        {result.decision.reasoning}
                      </p>
                    )}
                    <p className="text-xs text-muted-foreground">
                      {formatDate(result.decision.created_at)}
                    </p>
                  </div>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
