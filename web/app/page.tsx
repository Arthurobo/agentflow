"use client";

// / — the sessions home (and the PWA start_url): every Claude Code and
// OpenCode session on this machine, newest first, whether agentflow started
// it or someone ran the engine in a terminal. Tapping one opens its terminal,
// resuming it if nothing holds it. "New session" opens the run composer.
//
// Search and the project, engine, model and state filters narrow the rows
// already loaded; the list pages through the daemon with a cursor, so a
// machine with thousands of transcripts still opens fast.
import { useMemo, useState } from "react";
import Link from "next/link";
import { useInfiniteQuery } from "@tanstack/react-query";
import { Search, Terminal } from "lucide-react";
import { listAllSessions, type AllSessionItem } from "@/lib/agentd";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { EmptyState, PageHeader, PageShell } from "@/components/ui/surface";
import { RunModal } from "@/features/run/run-modal";
import { SessionFilters } from "@/features/sessions/session-filters";
import { engineMeta } from "@/lib/engine";
import { relTime } from "@/lib/format";
import { sessionPath } from "@/lib/paths";
import {
  hasFilters,
  matchesFilters,
  matchesSearch,
  modelOptions,
  NO_FILTERS,
  projectOptions,
  type SessionFilterValues,
  SESSIONS_PAGE_SIZE,
  sessionLabel,
} from "@/lib/sessions";
import { isLiveState } from "@/lib/run";
import { cn } from "@/lib/utils";

export default function SessionsHomePage() {
  const [runOpen, setRunOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [filters, setFilters] = useState<SessionFilterValues>(NO_FILTERS);
  const { data, error, isPending, isFetchingNextPage, hasNextPage, fetchNextPage } =
    useInfiniteQuery({
      queryKey: ["all-sessions"],
      queryFn: ({ pageParam }) => listAllSessions({ limit: SESSIONS_PAGE_SIZE, cursor: pageParam }),
      initialPageParam: "",
      getNextPageParam: (last) => last.nextCursor || undefined,
      // Live dots follow runs starting and stopping without a manual reload.
      refetchInterval: 10_000,
    });

  const sessions = useMemo(() => {
    // A session whose file changed between two page reads can appear on both.
    const seen = new Set<string>();
    const out: AllSessionItem[] = [];
    for (const page of data?.pages ?? []) {
      for (const s of page.items) {
        if (seen.has(s.id)) continue;
        seen.add(s.id);
        out.push(s);
      }
    }
    return out;
  }, [data]);
  const shown = sessions.filter((s) => matchesFilters(s, filters) && matchesSearch(s, query));
  const projects = useMemo(() => projectOptions(sessions), [sessions]);
  const models = useMemo(() => modelOptions(sessions, filters.engine), [sessions, filters.engine]);

  return (
    <PageShell>
      <PageHeader
        eyebrow="Console"
        title="Sessions"
        description="Every Claude Code and OpenCode session on this computer. Open one to drive its terminal."
        actions={<RunModal open={runOpen} onOpenChange={setRunOpen} label="New session" />}
      />
      {error && (
        <p role="alert" className="text-destructive mb-4 text-sm">
          {error instanceof Error ? error.message : "Could not load sessions."}
        </p>
      )}
      <div className="mb-3 flex flex-col gap-2 sm:flex-row sm:flex-wrap">
        <div className="relative min-w-0 flex-1 sm:min-w-56">
          <Search
            className="text-muted-foreground pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2"
            aria-hidden
          />
          <Input
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search by name or folder"
            aria-label="Search sessions"
            className="pl-9"
          />
        </div>
        <SessionFilters value={filters} onChange={setFilters} projects={projects} models={models} />
      </div>
      <div className="af-panel overflow-hidden p-2">
        {shown.length ? (
          <ul className="flex flex-col gap-1">
            {shown.map((s) => (
              <li key={s.id}>
                <SessionRow session={s} />
              </li>
            ))}
          </ul>
        ) : isPending ? (
          <EmptyState title="Loading sessions…" loading />
        ) : sessions.length ? (
          <EmptyState title={noMatchTitle(hasFilters(filters), hasNextPage)} />
        ) : (
          !error && (
            <EmptyState icon={<Terminal className="size-4" />} title="No sessions yet.">
              Start one with New session, or run claude or opencode in a terminal.
            </EmptyState>
          )
        )}
      </div>
      {hasNextPage && (
        <div className="mt-3 flex justify-center">
          <Button
            variant="outline"
            onClick={() => void fetchNextPage()}
            disabled={isFetchingNextPage}
          >
            {isFetchingNextPage ? "Loading…" : "Load more"}
          </Button>
        </div>
      )}
    </PageShell>
  );
}

// noMatchTitle explains an empty filtered list. The filters only see the pages
// loaded so far, so while more remain it says so rather than implying there
// are no such sessions at all.
function noMatchTitle(filtered: boolean, more: boolean): string {
  if (!filtered) return "No sessions match your search.";
  return more
    ? "No sessions match these filters in the sessions loaded so far."
    : "No sessions match these filters.";
}

function SessionRow({ session: s }: { session: AllSessionItem }) {
  const label = sessionLabel(s);
  const live = isLiveState(s.state);
  const where = s.cwd || s.project;
  // A live session already has a run; opening it by run id skips the
  // session lookup the terminal page would otherwise do.
  const href = sessionPath(live && s.runId ? s.runId : s.id);
  return (
    <Link href={href} className="hover:bg-muted/50 flex items-center gap-3 rounded-md px-3 py-2.5">
      <span
        className={cn(
          "size-2 shrink-0 rounded-full",
          live ? "bg-success motion-safe:animate-pulse" : "bg-muted-foreground/25",
        )}
        role={live ? "img" : undefined}
        aria-label={live ? s.state : undefined}
        title={live ? s.state : undefined}
      />
      <span className="min-w-0 flex-1">
        <span className="block truncate text-sm">{label}</span>
        {where && where !== label && (
          <span className="text-muted-foreground block truncate text-xs">{where}</span>
        )}
      </span>
      <Badge variant="outline">
        {engineMeta(s.engine).displayName}
      </Badge>
      <span className="text-muted-foreground w-14 shrink-0 text-right text-xs tabular-nums">
        {relTime(s.updatedAt)}
      </span>
    </Link>
  );
}
