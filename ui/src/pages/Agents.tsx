import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { listAgents, createAgent, deleteAgent, ApiError } from "@/lib/api";
import type { AgentRole, CreateAgentRequest } from "@/types/api";
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
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate } from "@/lib/utils";
import { Plus, Trash2 } from "lucide-react";

const roleColors: Record<AgentRole, "default" | "secondary" | "destructive" | "success" | "warning" | "outline"> = {
  platform_admin: "destructive",
  org_owner: "warning",
  admin: "default",
  agent: "secondary",
  reader: "outline",
};

const metadataKeys = ["model", "framework", "owner", "description"] as const;

function AgentDetails({ metadata }: { metadata: Record<string, unknown> | null }) {
  if (!metadata) return <span className="text-muted-foreground">{"\u2014"}</span>;

  const entries = metadataKeys
    .filter((k) => metadata[k] != null && String(metadata[k]).trim() !== "")
    .map((k) => ({ key: k, value: String(metadata[k]) }));

  if (entries.length === 0) return <span className="text-muted-foreground">{"\u2014"}</span>;

  return (
    <div className="flex flex-wrap gap-1">
      {entries.map(({ key, value }) => (
        <Badge key={key} variant="outline" className="text-xs font-normal">
          {key}: {value}
        </Badge>
      ))}
    </div>
  );
}

export default function Agents() {
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);

  const { data: agents, isPending } = useQuery({
    queryKey: ["agents"],
    queryFn: listAgents,
  });

  const createMutation = useMutation({
    mutationFn: createAgent,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["agents"] });
      setCreateOpen(false);
      setFormError(null);
    },
    onError: (err) => {
      setFormError(err instanceof ApiError ? err.message : "Failed to create agent");
    },
  });

  const deleteMutation = useMutation({
    mutationFn: deleteAgent,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["agents"] });
      setDeleteTarget(null);
    },
  });

  function handleCreate(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = new FormData(e.currentTarget);
    const metadata: Record<string, string> = {};
    for (const key of ["model", "framework", "owner", "description"] as const) {
      const val = (form.get(key) as string | null)?.trim();
      if (val) metadata[key] = val;
    }
    const req: CreateAgentRequest = {
      agent_id: form.get("agent_id") as string,
      name: form.get("name") as string,
      role: (form.get("role") as AgentRole) || "agent",
      api_key: form.get("api_key") as string,
      ...(Object.keys(metadata).length > 0 ? { metadata } : {}),
    };
    setFormError(null);
    createMutation.mutate(req);
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold tracking-tight">Agents</h1>
        <Dialog open={createOpen} onOpenChange={setCreateOpen}>
          <DialogTrigger asChild>
            <Button size="sm">
              <Plus className="h-4 w-4" />
              Create Agent
            </Button>
          </DialogTrigger>
          <DialogContent>
            <form onSubmit={handleCreate}>
              <DialogHeader>
                <DialogTitle>Create Agent</DialogTitle>
                <DialogDescription>
                  Register a new agent with API key credentials.
                </DialogDescription>
              </DialogHeader>
              <div className="space-y-4 py-4">
                <div className="space-y-2">
                  <Label htmlFor="agent_id">Agent ID</Label>
                  <Input id="agent_id" name="agent_id" required placeholder="my-agent" />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="name">Name</Label>
                  <Input id="name" name="name" required placeholder="My Agent" />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="role">Role</Label>
                  <Select name="role" defaultValue="agent">
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="reader">Reader</SelectItem>
                      <SelectItem value="agent">Agent</SelectItem>
                      <SelectItem value="admin">Admin</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-2">
                  <Label htmlFor="api_key">API Key</Label>
                  <Input
                    id="api_key"
                    name="api_key"
                    type="password"
                    required
                    placeholder="strong-secret-key"
                  />
                </div>
                <div className="border-t pt-3 mt-1">
                  <p className="text-xs text-muted-foreground mb-3">Optional metadata</p>
                  <div className="grid grid-cols-2 gap-3">
                    <div className="space-y-1">
                      <Label htmlFor="model" className="text-xs">Model</Label>
                      <Input id="model" name="model" placeholder="claude-opus-4-6" className="h-8 text-sm" />
                    </div>
                    <div className="space-y-1">
                      <Label htmlFor="framework" className="text-xs">Framework</Label>
                      <Input id="framework" name="framework" placeholder="LangGraph" className="h-8 text-sm" />
                    </div>
                    <div className="space-y-1">
                      <Label htmlFor="owner" className="text-xs">Owner</Label>
                      <Input id="owner" name="owner" placeholder="platform-team" className="h-8 text-sm" />
                    </div>
                    <div className="space-y-1">
                      <Label htmlFor="description" className="text-xs">Description</Label>
                      <Input id="description" name="description" placeholder="Reviews PRs" className="h-8 text-sm" />
                    </div>
                  </div>
                </div>
                {formError && (
                  <p className="text-sm text-destructive">{formError}</p>
                )}
              </div>
              <DialogFooter>
                <Button type="submit" disabled={createMutation.isPending}>
                  {createMutation.isPending ? "Creating\u2026" : "Create"}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </div>

      {isPending ? (
        <div className="space-y-2">
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} className="h-10 w-full" />
          ))}
        </div>
      ) : !agents?.length ? (
        <p className="text-sm text-muted-foreground py-8 text-center">
          No agents registered.
        </p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Agent ID</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Details</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-12" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {agents.map((agent) => (
              <TableRow key={agent.id}>
                <TableCell className="font-mono text-sm">
                  {agent.agent_id}
                </TableCell>
                <TableCell>{agent.name}</TableCell>
                <TableCell>
                  <Badge variant={roleColors[agent.role] ?? "secondary"}>
                    {agent.role}
                  </Badge>
                </TableCell>
                <TableCell>
                  <AgentDetails metadata={agent.metadata} />
                </TableCell>
                <TableCell className="text-sm text-muted-foreground">
                  {formatDate(agent.created_at)}
                </TableCell>
                <TableCell>
                  {agent.agent_id !== "admin" && (
                    <Dialog
                      open={deleteTarget === agent.agent_id}
                      onOpenChange={(open) =>
                        setDeleteTarget(open ? agent.agent_id : null)
                      }
                    >
                      <DialogTrigger asChild>
                        <Button variant="ghost" size="icon" aria-label={`Delete ${agent.agent_id}`}>
                          <Trash2 className="h-4 w-4 text-muted-foreground" />
                        </Button>
                      </DialogTrigger>
                      <DialogContent>
                        <DialogHeader>
                          <DialogTitle>Delete Agent</DialogTitle>
                          <DialogDescription>
                            This will permanently delete agent{" "}
                            <strong>{agent.agent_id}</strong> and all
                            associated runs, events, and decisions. This
                            action cannot be undone.
                          </DialogDescription>
                        </DialogHeader>
                        <DialogFooter>
                          <Button
                            variant="outline"
                            onClick={() => setDeleteTarget(null)}
                          >
                            Cancel
                          </Button>
                          <Button
                            variant="destructive"
                            disabled={deleteMutation.isPending}
                            onClick={() =>
                              deleteMutation.mutate(agent.agent_id)
                            }
                          >
                            {deleteMutation.isPending
                              ? "Deleting\u2026"
                              : "Delete"}
                          </Button>
                        </DialogFooter>
                      </DialogContent>
                    </Dialog>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
