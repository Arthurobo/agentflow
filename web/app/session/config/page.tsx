"use client";

// /session/config/?id=… — the "what rules are actually in effect" surface. Renders Claude's merged ~/.claude + cwd CLAUDE.md + settings.json
// (or OpenCode's engine config) in one scrollable page:
//
//   - Files (CLAUDE.md, settings.json) — precedence order, with first 512B excerpt
//   - Settings (effective model, permission mode, allowed tools)
//   - Hooks (with future fire counts from hooksrv)
//   - Skills (global .claude/skills/*)
//   - Agents (global .claude/agents/*)
//
// For OpenCode runs the engineExtras block surfaces control-base reachability
// so the operator knows the engine is actually answering the HTTP API.
//
// The id is a query parameter (a static export cannot build unknown [id]
// segments), read with useSearchParams inside <Suspense>.
import { Suspense } from "react";
import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "next/navigation";
import { FileText, Loader2, ShieldCheck, Sparkles, Wrench } from "lucide-react";
import Link from "next/link";
import { getSessionConfig } from "@/lib/agentd";
import { sessionPath } from "@/lib/paths";
import { engineMeta } from "@/lib/engine";

function ConfigPageInner() {
  const sp = useSearchParams();
  const id = sp.get("id") ?? "";
  const q = useQuery({
    queryKey: ["session-config", id],
    queryFn: () => getSessionConfig(id),
    refetchInterval: 30_000,
  });

  if (q.isLoading) {
    return (
      <div className="flex h-dvh items-center justify-center">
        <Loader2 className="text-muted-foreground size-6 animate-spin" />
      </div>
    );
  }
  if (q.isError || !q.data) {
    return (
      <div className="p-6">
        <p className="text-destructive">Failed to load config.</p>
        <Link href={sessionPath(id)} className="text-primary text-sm underline">
          Back to run
        </Link>
      </div>
    );
  }

  const { engine, cwd, summary, engineExtras } = q.data;
  const meta = engineMeta(engine);

  return (
    <main className="bg-background mx-auto min-h-dvh max-w-3xl px-4 py-4 sm:py-6">
      <header className="border-border/40 flex items-center justify-between gap-3 border-b pb-3">
        <div>
          <p className="text-muted-foreground text-[10px] font-semibold tracking-wider uppercase">
            Config
          </p>
          <h1 className="text-foreground text-lg font-semibold">
            What rules are in effect
          </h1>
          <p className="text-muted-foreground text-xs">
            {cwd || "—"} · {engineMeta(engine).displayName}
          </p>
        </div>
        <Link
          href={sessionPath(id)}
          className="text-muted-foreground hover:bg-accent/60 hover:text-foreground grid size-9 place-items-center rounded-full transition-colors"
          aria-label="Back to run"
        >
          ×
        </Link>
      </header>

      {engineExtras && (
        <section className="mt-4">
          <p className="text-muted-foreground text-[10px] font-semibold tracking-wider uppercase">
            Engine
          </p>
          <div className="border-border/40 mt-1 rounded-xl border p-3 text-[13px]">
            <p>
              <span className="text-foreground/70">control base:</span>{" "}
              <code className="font-mono">
                {String(engineExtras.controlBase ?? "—")}
              </code>
            </p>
            <p>
              <span className="text-foreground/70">reachable:</span>{" "}
              {engineExtras.reachable ? "yes" : "no"}
            </p>
          </div>
        </section>
      )}

      <Section
        icon={<FileText className="text-muted-foreground size-4" />}
        title="Files (precedence order)"
      >
        {summary.files.length === 0 ? (
          <p className="text-muted-foreground text-[13px]">
            No rule files found for this session.
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {summary.files.map((f) => (
              <li
                key={f.path}
                className="border-border/40 rounded-xl border p-3 text-[13px]"
              >
                <p className="flex items-center justify-between gap-2">
                  <span className="text-foreground/90 truncate font-mono">
                    {f.path}
                  </span>
                  <span className="bg-muted text-muted-foreground shrink-0 rounded-full px-2 py-0.5 text-[10px] font-semibold tracking-wider uppercase">
                    {f.origin}
                  </span>
                </p>
                <p className="text-muted-foreground mt-1 text-[11px]">
                  {f.loaded ? `${f.bytes} bytes` : "missing"}
                </p>
                {f.excerpt && (
                  <pre className="bg-muted/40 mt-2 max-h-32 overflow-y-auto rounded-md p-2 font-mono text-[11px] whitespace-pre-wrap">
                    {f.excerpt}
                  </pre>
                )}
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section
        icon={<ShieldCheck className="text-muted-foreground size-4" />}
        title="Settings (effective)"
      >
        <div className="border-border/40 rounded-xl border p-3 text-[13px]">
          <p>
            <span className="text-foreground/70">model:</span>{" "}
            <code className="font-mono">{summary.settings.model || "default"}</code>
          </p>
          <p>
            <span className="text-foreground/70">permission mode:</span>{" "}
            <code className="font-mono">
              {summary.settings.permissionMode || "default"}
            </code>
          </p>
          <p>
            <span className="text-foreground/70">allowed tools:</span>{" "}
            {summary.settings.allowedTools.length === 0
              ? "—"
              : summary.settings.allowedTools.join(", ")}
          </p>
        </div>
      </Section>

      <Section
        icon={<Wrench className="text-muted-foreground size-4" />}
        title="Hooks"
      >
        {summary.settings.hooks.length === 0 ? (
          <p className="text-muted-foreground text-[13px]">No hooks configured.</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {summary.settings.hooks.map((h, i) => (
              <li
                key={`${h.event}:${h.matcher}:${i}`}
                className="border-border/40 rounded-xl border p-3 text-[13px]"
              >
                <p className="text-foreground/80">
                  <code className="font-mono">{h.event}</code>
                  {h.matcher && (
                    <span className="text-muted-foreground ml-1">
                      ({h.matcher})
                    </span>
                  )}
                </p>
                <pre className="bg-muted/40 mt-1 overflow-x-auto rounded-md p-2 font-mono text-[11px]">
                  {h.command}
                </pre>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section
        icon={<Sparkles className="text-muted-foreground size-4" />}
        title="Skills"
      >
        {summary.skills.length === 0 ? (
          <p className="text-muted-foreground text-[13px]">
            No global skills installed.
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {summary.skills.map((s) => (
              <li
                key={s.path}
                className="border-border/40 rounded-xl border p-3 text-[13px]"
              >
                <p className="text-foreground/90 font-medium">{s.name}</p>
                {s.description && (
                  <p className="text-muted-foreground text-[12px]">
                    {s.description}
                  </p>
                )}
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section
        icon={<Sparkles className="text-muted-foreground size-4" />}
        title="Agents"
      >
        {summary.agents.length === 0 ? (
          <p className="text-muted-foreground text-[13px]">
            No global agents installed.
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {summary.agents.map((a) => (
              <li
                key={a.path}
                className="border-border/40 rounded-xl border p-3 text-[13px]"
              >
                <p className="flex items-center gap-2">
                  <span className="text-foreground/90 font-medium">{a.name}</span>
                  {a.model && (
                    <span className="bg-muted text-muted-foreground rounded-full px-2 py-0.5 text-[10px] font-mono">
                      {a.model}
                    </span>
                  )}
                </p>
                {a.description && (
                  <p className="text-muted-foreground mt-1 text-[12px]">
                    {a.description}
                  </p>
                )}
              </li>
            ))}
          </ul>
        )}
      </Section>

      <footer className="text-muted-foreground mt-6 text-center text-[11px]">
        {meta.displayName} · {cwd || "no working directory"}
      </footer>
    </main>
  );
}

function Section({
  icon,
  title,
  children,
}: {
  icon: React.ReactNode;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <section className="mt-4">
      <p className="text-muted-foreground flex items-center gap-2 text-[10px] font-semibold tracking-wider uppercase">
        {icon}
        {title}
      </p>
      <div className="mt-1">{children}</div>
    </section>
  );
}

export default function ConfigPage() {
  return (
    <Suspense fallback={null}>
      <ConfigPageInner />
    </Suspense>
  );
}