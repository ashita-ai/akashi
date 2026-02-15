import { useState, type FormEvent } from "react";
import { useAuth } from "@/lib/auth";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Download } from "lucide-react";

export default function ExportPage() {
  const { token } = useAuth();
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [agentId, setAgentId] = useState("");
  const [decisionType, setDecisionType] = useState("");

  function handleExport(e: FormEvent) {
    e.preventDefault();
    const params = new URLSearchParams();
    if (from) params.set("from", new Date(from).toISOString());
    if (to) params.set("to", new Date(to).toISOString());
    if (agentId.trim()) params.set("agent_id", agentId.trim());
    if (decisionType.trim()) params.set("decision_type", decisionType.trim());

    const qs = params.toString();
    const url = `/v1/export/decisions${qs ? `?${qs}` : ""}`;

    // Use fetch with auth header to trigger download
    fetch(url, {
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((res) => {
        if (!res.ok) throw new Error(`Export failed: ${res.status}`);
        const disposition = res.headers.get("Content-Disposition");
        const match = disposition?.match(/filename="(.+?)"/);
        const filename = match?.[1] ?? "akashi-export.ndjson";
        return res.blob().then((blob) => ({ blob, filename }));
      })
      .then(({ blob, filename }) => {
        const a = document.createElement("a");
        a.href = URL.createObjectURL(blob);
        a.download = filename;
        a.click();
        URL.revokeObjectURL(a.href);
      })
      .catch((err) => {
        console.error("Export failed:", err);
      });
  }

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold tracking-tight">Export</h1>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">Export Decisions</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleExport} className="space-y-4">
            <p className="text-sm text-muted-foreground">
              Export decisions as NDJSON (newline-delimited JSON). Each line contains a decision
              with its alternatives and evidence. Requires admin role.
            </p>

            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="from">From date</Label>
                <Input
                  id="from"
                  type="datetime-local"
                  value={from}
                  onChange={(e) => setFrom(e.target.value)}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="to">To date</Label>
                <Input
                  id="to"
                  type="datetime-local"
                  value={to}
                  onChange={(e) => setTo(e.target.value)}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="export-agent">Agent ID</Label>
                <Input
                  id="export-agent"
                  placeholder="All agents"
                  value={agentId}
                  onChange={(e) => setAgentId(e.target.value)}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="export-type">Decision type</Label>
                <Input
                  id="export-type"
                  placeholder="All types"
                  value={decisionType}
                  onChange={(e) => setDecisionType(e.target.value)}
                />
              </div>
            </div>

            <Button type="submit">
              <Download className="h-4 w-4 mr-2" />
              Download NDJSON
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
