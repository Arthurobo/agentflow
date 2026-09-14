"use client";

// RequestAccess — pairing without a link. The browser asks the daemon for
// access, shows the four-digit code the person at the computer will see next
// to the request, and polls until it is approved (the device token arrives
// once, and is stored), denied, or expired.
import { useEffect, useRef, useState } from "react";
import { Hourglass, Send, ShieldX } from "lucide-react";
import { Button } from "@/components/ui/button";
import { SectionHeader } from "@/components/ui/surface";
import {
  AgentdError,
  pollPairRequest,
  requestPairAccess,
  setDeviceToken,
  type PairRequestCreated,
} from "@/lib/agentd";
import { deviceLabel } from "@/lib/pairing";

// POLL_MS is how often a waiting request asks whether it was decided.
export const POLL_MS = 2000;

type Phase =
  | { kind: "idle" }
  | { kind: "asking" }
  | { kind: "waiting"; request: PairRequestCreated }
  | { kind: "approved" }
  | { kind: "denied" }
  | { kind: "expired" };

export function RequestAccess({ onPaired }: { onPaired: () => void }) {
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });
  const [error, setError] = useState<string | null>(null);
  // The poll loop reads the latest callback without restarting on every
  // render of the page around it.
  const onPairedRef = useRef(onPaired);
  useEffect(() => {
    onPairedRef.current = onPaired;
  }, [onPaired]);

  const ask = async () => {
    setError(null);
    setPhase({ kind: "asking" });
    try {
      const request = await requestPairAccess(deviceLabel(navigator.userAgent));
      setPhase({ kind: "waiting", request });
    } catch (e) {
      setError(e instanceof Error ? e.message : "The request could not be sent.");
      setPhase({ kind: "idle" });
    }
  };

  const waiting = phase.kind === "waiting" ? phase.request : null;
  useEffect(() => {
    if (!waiting) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const check = async () => {
      if (stopped) return;
      try {
        const res = await pollPairRequest(waiting.requestId, waiting.pollSecret);
        if (stopped) return;
        if (res.status === "approved" && res.deviceToken) {
          setDeviceToken(res.deviceToken);
          setPhase({ kind: "approved" });
          onPairedRef.current();
          return;
        }
        if (res.status === "denied" || res.status === "expired") {
          setPhase({ kind: res.status });
          return;
        }
      } catch (e) {
        if (stopped) return;
        // The daemon forgets a request some minutes after it ends; anything
        // else (a dropped connection) is worth another try.
        if (e instanceof AgentdError && e.status === 404) {
          setPhase({ kind: "expired" });
          return;
        }
      }
      if (Date.now() >= waiting.expiresAt + POLL_MS) {
        setPhase({ kind: "expired" });
        return;
      }
      timer = setTimeout(() => void check(), POLL_MS);
    };
    timer = setTimeout(() => void check(), POLL_MS);
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [waiting]);

  return (
    <div className="flex flex-col gap-3">
      <SectionHeader
        eyebrow="No link?"
        title="Request access"
        description="Ask the computer running agentflow to let this browser in. You approve it there."
      />
      {phase.kind === "waiting" ? (
        <div className="af-subtle-panel flex flex-col items-center gap-2 p-4 text-center" role="status">
          <Hourglass className="text-muted-foreground size-5" aria-hidden />
          <p className="text-muted-foreground text-xs">Your code</p>
          <p className="font-mono text-4xl font-semibold tracking-[0.3em]" data-testid="match-code">
            {phase.request.matchCode}
          </p>
          <p className="text-sm">
            Approve this on your computer:{" "}
            <code className="bg-muted rounded px-1">agentflow approve {phase.request.matchCode}</code>
          </p>
          <p className="text-muted-foreground text-xs">
            Check that the computer shows the same code. Waiting for approval…
          </p>
        </div>
      ) : phase.kind === "approved" ? (
        <p className="text-success text-sm" role="status">
          Approved — opening agentflow.
        </p>
      ) : (
        <>
          {phase.kind === "denied" && (
            <p role="alert" className="text-destructive flex items-center gap-2 text-sm">
              <ShieldX className="size-4" /> The request was denied on the computer.
            </p>
          )}
          {phase.kind === "expired" && (
            <p role="alert" className="text-destructive text-sm">
              The request expired before anyone approved it. Ask again when you&apos;re at the computer.
            </p>
          )}
          <Button size="lg" onClick={() => void ask()} disabled={phase.kind === "asking"}>
            <Send className="size-4" />
            {phase.kind === "asking"
              ? "Asking…"
              : phase.kind === "idle"
                ? "Request access"
                : "Ask again"}
          </Button>
        </>
      )}
      {error && (
        <p role="alert" className="text-destructive text-xs">
          {error}
        </p>
      )}
    </div>
  );
}
