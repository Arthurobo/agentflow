import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import {
  ChipToggle,
  CodePreview,
  EmptyState,
  Field,
  FieldGroup,
  Metric,
  MetricStrip,
  PageHeader,
  PageShell,
  SectionHeader,
  StatusDot,
} from "./surface";

describe("surface primitives", () => {
  it("renders the shared page shell and header vocabulary", () => {
    render(
      <PageShell>
        <PageHeader
          eyebrow="Signal"
          title="Activity"
          description="Live stream"
          actions={<button>Act</button>}
        />
      </PageShell>,
    );
    expect(screen.getByRole("main")).toHaveClass("af-page");
    expect(screen.getByText("Signal")).toHaveClass("af-label");
    expect(screen.getByRole("button", { name: "Act" })).toBeInTheDocument();
  });

  it("renders metrics, empty states, and status dots", () => {
    render(
      <>
        <MetricStrip>
          <Metric label="Events" value="123" hint="indexed" />
        </MetricStrip>
        <EmptyState title="Nothing pending.">All clear.</EmptyState>
        <StatusDot state="live" pulse />
      </>,
    );
    expect(screen.getByText("Events")).toBeInTheDocument();
    expect(screen.getByText("123")).toBeInTheDocument();
    expect(screen.getByText("Nothing pending.")).toBeInTheDocument();
  });

  it("renders form and code primitives with accessible state", () => {
    render(
      <>
        <SectionHeader
          eyebrow="Compose"
          title="Run a focused agent"
          description="Scoped autonomy."
        />
        <Field label="Prompt" hint="Be specific.">
          <input />
        </Field>
        <ChipToggle selected onClick={() => undefined}>
          Edit
        </ChipToggle>
        <CodePreview>{`{"tool":"Edit"}`}</CodePreview>
      </>,
    );
    expect(screen.getByText("Compose")).toHaveClass("af-label");
    expect(screen.getByText("Prompt")).toHaveClass("af-label");
    expect(screen.getByRole("button", { name: "Edit" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByText('{"tool":"Edit"}')).toBeInTheDocument();
  });

  // Field is a <label>. A label wrapping several controls donates its whole
  // text to every one of them as an accessible name, so a caption over a grid
  // of buttons has to be a group instead.
  it("gives a multi-control section a caption without renaming its controls", () => {
    render(
      <>
        <Field label="Task" hint="One line.">
          <input aria-label="task" />
        </Field>
        <FieldGroup label="Play" hint="The sequence the engine enforces.">
          <button type="button">Audit</button>
          <button type="button">Patch</button>
        </FieldGroup>
      </>,
    );
    const group = screen.getByRole("group", { name: "Play" });
    expect(group).toBeInTheDocument();
    expect(screen.getByText("Play")).toHaveClass("af-label");
    expect(screen.getByText("The sequence the engine enforces.")).toBeInTheDocument();
    // Each button keeps its own name.
    expect(screen.getByRole("button", { name: "Audit" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Patch" })).toBeInTheDocument();
  });
});