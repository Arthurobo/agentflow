"use client";

// /loop — the engine-owned loops: every mission the engine drives, plus the
// wizard.
//
// The wizard opens by DEFAULT when there is nothing to look at. A page whose
// only content is a "Start a loop" button reads as a feature that has not been
// built, which is exactly how the play picker went unnoticed while it was
// deployed and working.
import { useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle, Plus, Users } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { EmptyState, PageHeader, PageShell } from "@/components/ui/surface";
import { LoopWizard } from "@/features/loop/wizard";
import { listLoopsWithAttention, loopStanding, LoopError, type LoopAttention } from "@/lib/loop";

export default function LoopsPage() {
  const router = useRouter();
  // null means "the engineer has not said": the page decides from what it
  // found. An explicit open or close wins from then on.
  const [wizardChoice, setWizardChoice] = useState<boolean | null>(null);
  const loops = useQuery({ queryKey: ["loop", "list"], queryFn: () => listLoopsWithAttention() });

  const rows = loops.data?.loops ?? [];
  const attention = loops.data?.attention ?? {};
  // Only auto-open once the list is known to be empty. An error is NOT
  // emptiness and must not be answered with a create form.
  const wizardOpen = wizardChoice ?? (loops.isSuccess && rows.length === 0);

  return (
    <PageShell>
      <PageHeader
        eyebrow="Loops"
        title="Engine-owned loops"
        description="Start a crew of agents on one task and watch them work. The engine owns the sequence: members pull their work, acknowledge it explicitly, and a message that does not belong to the current step is refused."
        actions={
          <Button onClick={() => setWizardChoice(!wizardOpen)}>
            <Plus /> {wizardOpen ? "Close" : "Start a loop"}
          </Button>
        }
      />

      {loops.isError && (
        <div
          role="alert"
          className="border-destructive/40 bg-destructive/5 text-destructive mb-4 flex flex-wrap items-center gap-2 rounded-md border px-3 py-2 text-sm"
        >
          <AlertCircle className="size-4 shrink-0" />
          <span className="min-w-0">
            Could not load your loops:{" "}
            {loops.error instanceof LoopError
              ? `${loops.error.message}${loops.error.code ? ` (${loops.error.code})` : ""}`
              : "the loop surface did not answer"}
          </span>
          <Button size="sm" variant="outline" className="ml-auto" onClick={() => void loops.refetch()}>
            Retry
          </Button>
        </div>
      )}

      {wizardOpen && (
        <div className="af-panel mb-6 p-4">
          {/* Straight into the orchestrator's CONVERSATION, not the board.
              Creating a loop and landing with the composer in front of him is
              the whole point of the redirect: the next thing he does is type
              the task. */}
          <LoopWizard
            onCreated={(id) => {
              setWizardChoice(false);
              router.push(`/loop/board/?id=${encodeURIComponent(id)}`);
            }}
          />
        </div>
      )}

      {rows.length === 0 ? (
        !loops.isError &&
        !wizardOpen && (
          <EmptyState icon={<Users className="size-4" />} title="No loops yet">
            Start one: pick a play, pick who runs it, and the members are launched for you.
          </EmptyState>
        )
      ) : (
        <ul className="flex flex-col gap-2">
          {rows.map((l) => (
            <li key={l.id}>
              <Link
                href={`/loop/board/?id=${encodeURIComponent(l.id)}`}
                className="af-panel block rounded-lg p-3 hover:border-primary/35"
              >
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-sm font-semibold">{l.title || l.task}</span>
                  {/* Four open loops all read "active", which told him nothing
                      about which one had stopped moving. */}
                  <AttentionBadge attention={attention[l.id]} />
                  <Badge variant="outline">{loopStanding(l)}</Badge>
                  {l.play && <Badge variant="outline">{l.play}</Badge>}
                  {l.stepId && (
                    <span className="text-muted-foreground font-mono text-[11px]">
                      {l.stepId} · round {l.round}
                    </span>
                  )}
                </div>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </PageShell>
  );
}

// AttentionBadge names what a loop needs a human for, or renders nothing.
function AttentionBadge({ attention }: { attention?: LoopAttention }) {
  if (!attention) return null;
  const { stranded = [], failed = [] } = attention;
  if (stranded.length === 0 && failed.length === 0) return null;
  const what = stranded.length > 0 ? "not reading its mail" : "did not start";
  const who = (stranded.length > 0 ? stranded : failed).join(", ");
  return (
    <span className="border-destructive/45 bg-destructive/10 text-destructive inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium">
      {who} {what}
    </span>
  );
}
