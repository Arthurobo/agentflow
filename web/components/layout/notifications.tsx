"use client";

// NotificationCenter — registers the service worker and turns run lifecycle
// transitions into notifications ("session finished", "session blocked on a
// denied tool", "session crashed"). It learns about transitions by polling
// the run list; there is no event stream to listen to.
import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { notifyLocal, registerServiceWorker } from "@/lib/notify";
import { listRuns, type RunSession } from "@/lib/agentd";

// POLL_MS is how often the run list is read. Each read is one small request
// to the daemon; a few seconds is quick enough for "your run finished".
export const POLL_MS = 5000;

export interface RunNotice {
  title: string;
  body: string;
  tag: string;
}

// runTransitions compares two snapshots of the run list and returns the
// notifications the change deserves. A run seen for the first time never
// notifies: on page load every existing run would otherwise announce itself.
export function runTransitions(
  prev: Map<string, RunSession>,
  runs: RunSession[],
): RunNotice[] {
  const out: RunNotice[] = [];
  for (const r of runs) {
    const old = prev.get(r.id);
    if (!old) continue;
    const label = r.title?.slice(0, 60) || r.prompt?.slice(0, 60) || r.id.slice(0, 8);
    const tag = `run-${r.id}`;
    if (old.state !== r.state) {
      if (r.state === "finished") {
        out.push({ title: "Session finished", body: `${label} completed.`, tag });
      } else if (r.state === "crashed") {
        out.push({ title: "Session crashed", body: `${label} — resume it.`, tag });
      } else if (r.state === "stopped") {
        out.push({ title: "Session stopped", body: `${label} was stopped.`, tag });
      }
    }
    if (r.blocked && !old.blocked && r.state === "awaiting") {
      out.push({
        title: "Session blocked on a denied tool",
        body: `${label} — a tool was denied, steer it or stop.`,
        tag,
      });
    }
  }
  return out;
}

export function NotificationCenter() {
  const queryClient = useQueryClient();
  const prev = useRef<Map<string, RunSession> | null>(null);

  useEffect(() => {
    void registerServiceWorker();
  }, []);

  useEffect(() => {
    let cancelled = false;
    let inFlight = false;

    const check = async () => {
      if (inFlight || document.visibilityState === "hidden") return;
      inFlight = true;
      try {
        const runs = await listRuns();
        if (cancelled) return;
        const current = new Map(runs.items.map((r) => [r.id, r]));
        if (prev.current) {
          const notices = runTransitions(prev.current, runs.items);
          for (const n of notices) {
            void notifyLocal(n.title, n.body, n.tag);
          }
          // A run that just ended should lose its live dot in the sessions
          // list now, not on that list's next poll.
          if (notices.length) void queryClient.invalidateQueries({ queryKey: ["all-sessions"] });
        }
        prev.current = current;
      } catch {
        // daemon asleep or unreachable: try again on the next tick
      } finally {
        inFlight = false;
      }
    };

    void check();
    const timer = setInterval(() => void check(), POLL_MS);
    const onVisible = () => {
      if (document.visibilityState === "visible") void check();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      cancelled = true;
      clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [queryClient]);

  return null;
}
