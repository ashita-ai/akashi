import { useCallback, useEffect, useState } from "react";
import { listGrants, createGrant, deleteGrant, listAgents } from "@/lib/api";
import type { Grant, Agent } from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
import { Shield, Plus, Trash2 } from "lucide-react";

function formatDate(iso: string): string {
  return new Date(iso).toLocaleString();
}

function isExpired(grant: Grant): boolean {
  return grant.expires_at !== null && new Date(grant.expires_at) < new Date();
}

export default function GrantsPage() {
  const [grants, setGrants] = useState<Grant[]>([]);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Create dialog state.
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [granteeAgentId, setGranteeAgentId] = useState("");
  const [resourceId, setResourceId] = useState("");
  const [expiresAt, setExpiresAt] = useState("");
  const [createError, setCreateError] = useState<string | null>(null);

  // Delete confirmation state.
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  const fetchGrants = useCallback(async () => {
    try {
      setLoading(true);
      const result = await listGrants({ limit: 100 });
      setGrants(result.grants);
      setTotal(result.total);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load grants");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchGrants();
    listAgents()
      .then(setAgents)
      .catch(() => {});
  }, [fetchGrants]);

  // Build a map from agent UUID to agent_id for display.
  const agentMap = new Map<string, string>();
  for (const a of agents) {
    agentMap.set(a.id, a.agent_id);
  }

  async function handleCreate() {
    if (!granteeAgentId.trim()) {
      setCreateError("Grantee agent ID is required");
      return;
    }
    setCreating(true);
    setCreateError(null);
    try {
      await createGrant({
        grantee_agent_id: granteeAgentId.trim(),
        resource_type: "agent_traces",
        resource_id: resourceId.trim() || undefined,
        permission: "read",
        expires_at: expiresAt ? new Date(expiresAt).toISOString() : undefined,
      });
      setShowCreate(false);
      setGranteeAgentId("");
      setResourceId("");
      setExpiresAt("");
      await fetchGrants();
    } catch (err) {
      setCreateError(
        err instanceof Error ? err.message : "Failed to create grant",
      );
    } finally {
      setCreating(false);
    }
  }

  async function handleDelete(grantId: string) {
    setDeleting(true);
    try {
      await deleteGrant(grantId);
      setDeleteTarget(null);
      await fetchGrants();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to delete grant");
    } finally {
      setDeleting(false);
    }
  }

  const activeGrants = grants.filter((g) => !isExpired(g));
  const expiredGrants = grants.filter((g) => isExpired(g));

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold tracking-tight">Grants</h1>
        <Button onClick={() => setShowCreate(true)}>
          <Plus className="h-4 w-4 mr-2" />
          Create Grant
        </Button>
      </div>

      {error && (
        <div className="rounded-md bg-destructive/10 p-3 text-sm text-destructive">
          {error}
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium flex items-center gap-2">
            <Shield className="h-4 w-4" />
            Active Grants ({activeGrants.length} of {total})
          </CardTitle>
        </CardHeader>
        <CardContent>
          {loading ? (
            <p className="text-sm text-muted-foreground">Loading...</p>
          ) : activeGrants.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No active grants. Create one to give an agent access to another
              agent's traces.
            </p>
          ) : (
            <GrantTable
              grants={activeGrants}
              agentMap={agentMap}
              onDelete={setDeleteTarget}
            />
          )}
        </CardContent>
      </Card>

      {expiredGrants.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium text-muted-foreground">
              Expired Grants ({expiredGrants.length})
            </CardTitle>
          </CardHeader>
          <CardContent>
            <GrantTable
              grants={expiredGrants}
              agentMap={agentMap}
              onDelete={setDeleteTarget}
              expired
            />
          </CardContent>
        </Card>
      )}

      {/* Create dialog */}
      <Dialog open={showCreate} onOpenChange={setShowCreate}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create Grant</DialogTitle>
          </DialogHeader>
          <div className="space-y-4">
            <p className="text-sm text-muted-foreground">
              Grant read access to an agent's decision traces. The grantee will
              be able to view decisions made by the specified resource agent.
            </p>
            {createError && (
              <div className="rounded-md bg-destructive/10 p-3 text-sm text-destructive">
                {createError}
              </div>
            )}
            <div className="space-y-2">
              <Label htmlFor="grantee">Grantee Agent ID</Label>
              <Input
                id="grantee"
                placeholder="e.g. reader-bot"
                value={granteeAgentId}
                onChange={(e) => setGranteeAgentId(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="resource">
                Resource Agent ID{" "}
                <span className="text-muted-foreground">(optional)</span>
              </Label>
              <Input
                id="resource"
                placeholder="All agents (leave blank for wildcard)"
                value={resourceId}
                onChange={(e) => setResourceId(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="expires">
                Expires At{" "}
                <span className="text-muted-foreground">(optional)</span>
              </Label>
              <Input
                id="expires"
                type="datetime-local"
                value={expiresAt}
                onChange={(e) => setExpiresAt(e.target.value)}
              />
            </div>
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setShowCreate(false)}
              disabled={creating}
            >
              Cancel
            </Button>
            <Button onClick={handleCreate} disabled={creating}>
              {creating ? "Creating..." : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete confirmation dialog */}
      <Dialog
        open={deleteTarget !== null}
        onOpenChange={() => setDeleteTarget(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke Grant</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            Are you sure you want to revoke this grant? The grantee will
            immediately lose access.
          </p>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setDeleteTarget(null)}
              disabled={deleting}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => deleteTarget && handleDelete(deleteTarget)}
              disabled={deleting}
            >
              {deleting ? "Revoking..." : "Revoke"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function GrantTable({
  grants,
  agentMap,
  onDelete,
  expired,
}: {
  grants: Grant[];
  agentMap: Map<string, string>;
  onDelete: (id: string) => void;
  expired?: boolean;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Grantor</TableHead>
          <TableHead>Grantee</TableHead>
          <TableHead>Resource</TableHead>
          <TableHead>Permission</TableHead>
          <TableHead>Granted</TableHead>
          <TableHead>Expires</TableHead>
          <TableHead className="w-[60px]" />
        </TableRow>
      </TableHeader>
      <TableBody>
        {grants.map((g) => (
          <TableRow key={g.id} className={expired ? "opacity-50" : undefined}>
            <TableCell className="font-mono text-xs">
              {agentMap.get(g.grantor_id) ?? g.grantor_id.slice(0, 8)}
            </TableCell>
            <TableCell className="font-mono text-xs">
              {agentMap.get(g.grantee_id) ?? g.grantee_id.slice(0, 8)}
            </TableCell>
            <TableCell>
              <Badge variant="outline" className="text-xs">
                {g.resource_type}
              </Badge>
              {g.resource_id && (
                <span className="ml-1 text-xs text-muted-foreground">
                  {g.resource_id}
                </span>
              )}
            </TableCell>
            <TableCell className="text-xs">{g.permission}</TableCell>
            <TableCell className="text-xs text-muted-foreground">
              {formatDate(g.granted_at)}
            </TableCell>
            <TableCell className="text-xs text-muted-foreground">
              {g.expires_at ? formatDate(g.expires_at) : "Never"}
            </TableCell>
            <TableCell>
              <Button
                variant="ghost"
                size="icon"
                onClick={() => onDelete(g.id)}
                aria-label="Revoke grant"
              >
                <Trash2 className="h-4 w-4 text-destructive" />
              </Button>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
