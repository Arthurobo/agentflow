"use client";

// AccessRequests — requests from browsers that tapped Request access, with
// Approve and Deny, for a paired browser on this computer. The daemon only
// serves the list on its local address; anywhere else it answers 404 and
// this renders nothing, so a phone on the public URL never approves anyone.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Check, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { SectionHeader } from "@/components/ui/surface";
import { AgentdError, decidePairRequest, listPairRequests } from "@/lib/agentd";
import { relTime } from "@/lib/format";

export const ACCESS_REQUESTS_KEY = ["pair-requests"];

export function AccessRequests() {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const { data, error: loadError } = useQuery({
    queryKey: ACCESS_REQUESTS_KEY,
    queryFn: listPairRequests,
    refetchInterval: (q) =>
      q.state.error instanceof AgentdError && q.state.error.status === 404 ? false : 3000,
    retry: false,
  });

  if (loadError || !data?.length) return null;

  const decide = async (id: string, approve: boolean) => {
    setError(null);
    try {
      await decidePairRequest(id, approve);
    } catch (e) {
      setError(e instanceof Error ? e.message : "That request could not be updated.");
    }
    await queryClient.invalidateQueries({ queryKey: ACCESS_REQUESTS_KEY });
  };

  return (
    <section className="mt-6" aria-label="Access requests">
      <SectionHeader
        eyebrow="Waiting for you"
        title="Access requests"
        description="Approve only a device you're holding, and check that it shows the same code."
      />
      <ul className="flex flex-col gap-1.5">
        {data.map((r) => (
          <li key={r.id} className="af-subtle-panel flex flex-wrap items-center gap-2 px-3 py-2 text-sm">
            <span className="font-mono text-base font-semibold tracking-widest">{r.matchCode}</span>
            <span className="min-w-0 flex-1 truncate">{r.name}</span>
            <span className="text-muted-foreground hidden text-xs sm:inline">
              {r.clientIp} · {relTime(r.createdAt)}
            </span>
            <Button size="sm" onClick={() => void decide(r.id, true)} aria-label={`Approve ${r.name}`}>
              <Check className="size-3.5" /> Approve
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void decide(r.id, false)}
              aria-label={`Deny ${r.name}`}
            >
              <X className="size-3.5" /> Deny
            </Button>
          </li>
        ))}
      </ul>
      {error && (
        <p role="alert" className="text-destructive mt-2 text-xs">
          {error}
        </p>
      )}
    </section>
  );
}
