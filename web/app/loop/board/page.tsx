"use client";

// /loop/board/?id=… — the board the engineer leaves open. A static export
// can't build unknown dynamic segments, so the id is in the query string.
import { Suspense } from "react";
import { useSearchParams } from "next/navigation";
import { PageShell } from "@/components/ui/surface";
import { LoopBoard } from "@/features/loop/loop-board";

function LoopBoardInner() {
  const sp = useSearchParams();
  const id = sp.get("id") ?? "";
  return (
    <PageShell>
      <LoopBoard loopId={id} />
    </PageShell>
  );
}

export default function LoopBoardPage() {
  return (
    <Suspense fallback={null}>
      <LoopBoardInner />
    </Suspense>
  );
}