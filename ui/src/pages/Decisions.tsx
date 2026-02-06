import { useQuery } from "@tanstack/react-query";
import { useSearchParams, Link } from "react-router";
import { queryDecisions } from "@/lib/api";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate, truncate } from "@/lib/utils";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { useState, type FormEvent } from "react";

const PAGE_SIZE = 25;

export default function Decisions() {
  const [searchParams, setSearchParams] = useSearchParams();

  const page = Math.max(0, parseInt(searchParams.get("page") ?? "0", 10));
  const agentFilter = searchParams.get("agent") ?? "";
  const typeFilter = searchParams.get("type") ?? "";

  const [agentInput, setAgentInput] = useState(agentFilter);
  const [typeInput, setTypeInput] = useState(typeFilter);

  const { data, isPending } = useQuery({
    queryKey: ["decisions", page, agentFilter, typeFilter],
    queryFn: () =>
      queryDecisions({
        filters: {
          ...(agentFilter ? { agent_ids: [agentFilter] } : {}),
          ...(typeFilter ? { decision_type: typeFilter } : {}),
        },
        include: ["alternatives"],
        order_by: "valid_from",
        order_dir: "desc",
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
      }),
  });

  function applyFilters(e: FormEvent) {
    e.preventDefault();
    const params: Record<string, string> = {};
    if (agentInput) params.agent = agentInput;
    if (typeInput) params.type = typeInput;
    setSearchParams(params);
  }

  function goToPage(p: number) {
    const params: Record<string, string> = {};
    if (agentFilter) params.agent = agentFilter;
    if (typeFilter) params.type = typeFilter;
    if (p > 0) params.page = String(p);
    setSearchParams(params);
  }

  const totalPages = data ? Math.ceil(data.total / PAGE_SIZE) : 0;

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold tracking-tight">Decisions</h1>

      {/* Filters */}
      <form
        onSubmit={applyFilters}
        className="flex flex-wrap items-end gap-3"
      >
        <div className="space-y-1">
          <label className="text-xs text-muted-foreground">Agent</label>
          <Input
            placeholder="agent-id"
            value={agentInput}
            onChange={(e) => setAgentInput(e.target.value)}
            className="w-40"
          />
        </div>
        <div className="space-y-1">
          <label className="text-xs text-muted-foreground">Type</label>
          <Input
            placeholder="decision_type"
            value={typeInput}
            onChange={(e) => setTypeInput(e.target.value)}
            className="w-40"
          />
        </div>
        <Button type="submit" size="sm">
          Filter
        </Button>
        {(agentFilter || typeFilter) && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => {
              setAgentInput("");
              setTypeInput("");
              setSearchParams({});
            }}
          >
            Clear
          </Button>
        )}
      </form>

      {/* Table */}
      {isPending ? (
        <div className="space-y-2">
          {Array.from({ length: 10 }).map((_, i) => (
            <Skeleton key={i} className="h-10 w-full" />
          ))}
        </div>
      ) : !data?.decisions?.length ? (
        <p className="text-sm text-muted-foreground py-8 text-center">
          No decisions found.
        </p>
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Timestamp</TableHead>
                <TableHead>Agent</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Outcome</TableHead>
                <TableHead className="text-right">Confidence</TableHead>
                <TableHead className="text-right">Alternatives</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.decisions.map((d) => (
                <TableRow key={d.id}>
                  <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                    <Link
                      to={`/decisions/${d.run_id}`}
                      className="hover:underline"
                    >
                      {formatDate(d.created_at)}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline" className="font-mono text-xs">
                      {d.agent_id}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <Badge variant="secondary">{d.decision_type}</Badge>
                  </TableCell>
                  <TableCell className="max-w-[200px]">
                    <Link
                      to={`/decisions/${d.run_id}`}
                      className="hover:underline"
                    >
                      {truncate(d.outcome, 60)}
                    </Link>
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {(d.confidence * 100).toFixed(0)}%
                  </TableCell>
                  <TableCell className="text-right">
                    {d.alternatives?.length ?? 0}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>

          {/* Pagination */}
          <div className="flex items-center justify-between">
            <p className="text-sm text-muted-foreground">
              Showing {page * PAGE_SIZE + 1}\u2013
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
        </>
      )}
    </div>
  );
}
