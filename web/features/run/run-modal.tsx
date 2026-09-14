"use client";

// RunModal — the spawn composer in a dialog, behind a button on the sessions
// page.
import { Play } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { RunForm } from "./run-form";

export function RunModal({
  open,
  onOpenChange,
  resumeSessionId,
  label = "Run agent",
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  resumeSessionId?: string;
  label?: string;
}) {
  return (
    <>
      <Button onClick={() => onOpenChange(true)}>
        <Play className="size-4" /> {label}
      </Button>
      <Dialog
        open={open}
        onOpenChange={onOpenChange}
        title={resumeSessionId ? "Continue session" : "Run agent"}
        description="Name it, pick a folder — the interactive claude terminal opens ready to drive."
      >
        <RunForm resumeSessionId={resumeSessionId} />
      </Dialog>
    </>
  );
}
