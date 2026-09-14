// prompts.ts — the override surface. Machine-scoped: one set per agentd
// install, editable from any paired device, so this client carries no device
// identity and never should.
import { z } from "zod";
import { agentdBase, getDeviceToken } from "@/lib/agentd";

export const promptsBase = agentdBase + "/api/v1/prompts";

export const OVERRIDE_APPLIED = "applied";
export const OVERRIDE_STALE = "stale";
export const OVERRIDE_ORPHANED = "orphaned";

// The Go backend marshals an empty slice/map as JSON `null`, and zod's
// `.default()` only fills for `undefined`, not `null` — so a `null` here throws
// a ZodError that the editor surfaces as "the surface did not answer". These
// helpers accept null/undefined/missing and normalise to an empty collection.
// (Same fix as the session-config schema.)
const arr = <T extends z.ZodTypeAny>(el: T) =>
  z.array(el).nullish().transform((v) => v ?? []);
const rec = <T extends z.ZodTypeAny>(el: T) =>
  z.record(z.string(), el).nullish().transform((v) => v ?? ({} as Record<string, z.infer<T>>));

const ruleSchema = z.object({
  name: z.string(),
  text: z.string(),
  binding: z.boolean().optional().default(false),
});
export type PromptRule = z.infer<typeof ruleSchema>;

const stepSchema = z.object({
  ID: z.string(),
  Actor: z.string(),
  Note: z.string().optional().default(""),
  Brief: z.string().optional().default(""),
  FanIn: z.boolean().optional().default(false),
});
export type PromptStep = z.infer<typeof stepSchema>;

const playSchema = z.object({
  Name: z.string(),
  Title: z.string(),
  Purpose: z.string().optional().default(""),
  Entry: z.string().optional().default(""),
  Steps: arr(stepSchema),
});
export type PromptPlay = z.infer<typeof playSchema>;

const overrideSchema = z.object({
  id: z.string(),
  kind: z.string(),
  play: z.string().optional().default(""),
  step: z.string().optional().default(""),
  rule: z.string().optional().default(""),
  field: z.string(),
  value: z.string(),
  baseDigest: z.string().optional().default(""),
  // status is computed against the CURRENT defaults on every read, never
  // stored, because a stored one would itself go stale.
  status: z.string().optional().default(""),
  // base is what the shipped text says NOW, so a stale override can offer
  // take-the-new-one without a second round trip.
  base: z.string().optional().default(""),
  updatedAt: z.number().optional().default(0),
});
export type PromptOverride = z.infer<typeof overrideSchema>;

export const promptsSchema = z.object({
  plays: arr(playSchema),
  rules: rec(arr(ruleSchema)),
  solo: z
    .object({ Intro: z.string().optional().default(""), Rules: arr(z.string()) })
    .nullish()
    .transform((v) => v ?? { Intro: "", Rules: [] }),
  overrides: arr(overrideSchema),
  // dropped names the plays whose merged form does not validate, with the
  // validator's own message. They are absent from plays, and the wizard shows
  // them as disabled rather than as an absence.
  dropped: rec(z.string()),
  defaultPlays: arr(playSchema),
  defaultRules: rec(arr(ruleSchema)),
});
export type Prompts = z.infer<typeof promptsSchema>;

export class PromptError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "PromptError";
  }
}

async function req<T>(schema: z.ZodType<T>, path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const tok = getDeviceToken();
  if (tok) headers.Authorization = `Bearer ${tok}`;
  let resp: Response;
  try {
    resp = await fetch(promptsBase + path, { ...init, headers, cache: "no-store" });
  } catch {
    throw new PromptError(0, "unreachable", "agentflow is unreachable — is the daemon running?");
  }
  if (!resp.ok) {
    let code = "error";
    let message = `HTTP ${resp.status}`;
    try {
      const body = (await resp.json()) as { error?: { code?: string; message?: string } };
      code = body.error?.code ?? code;
      // The validator's own message, verbatim. It was written for a human and
      // anything we substitute for it will be worse.
      message = body.error?.message ?? message;
    } catch {
      // a non-JSON body stays as the status line
    }
    throw new PromptError(resp.status, code, message);
  }
  return schema.parse(await resp.json());
}

export function getPrompts(): Promise<Prompts> {
  return req(promptsSchema, "");
}

export interface OverrideTarget {
  kind: "play" | "rule" | "solo";
  play?: string;
  step?: string;
  rule?: string;
  field: "brief" | "note" | "purpose" | "title" | "text" | "intro";
}

export function putOverride(target: OverrideTarget, value: string): Promise<PromptOverride> {
  return req(z.object({ override: overrideSchema }), "", {
    method: "PUT",
    body: JSON.stringify({ ...target, value }),
  }).then((r) => r.override);
}

export function deleteOverride(id: string): Promise<void> {
  return req(z.object({ deleted: z.boolean() }), `/${encodeURIComponent(id)}`, {
    method: "DELETE",
  }).then(() => undefined);
}

/** overrideFor finds the stored edit for one leaf, if there is one. */
export function overrideFor(
  overrides: PromptOverride[],
  target: OverrideTarget,
): PromptOverride | undefined {
  return overrides.find(
    (o) =>
      o.kind === target.kind &&
      (o.play ?? "") === (target.play ?? "") &&
      (o.step ?? "") === (target.step ?? "") &&
      (o.rule ?? "") === (target.rule ?? "") &&
      o.field === target.field,
  );
}

/** playIsEdited says whether any leaf of this play carries an override, which
 *  is what a play card shows so a past decision is discoverable. */
export function playIsEdited(overrides: PromptOverride[], play: string): boolean {
  return overrides.some(
    (o) => o.kind === "play" && o.play === play && o.status !== OVERRIDE_ORPHANED,
  );
}

/** standingIsEdited covers the run form's control: an edit to the solo block
 *  or to any rule it references shapes every session he starts. */
export function standingIsEdited(overrides: PromptOverride[], soloRules: string[]): boolean {
  return overrides.some(
    (o) =>
      o.status !== OVERRIDE_ORPHANED &&
      ((o.kind === "solo" && o.field === "intro") ||
        (o.kind === "rule" && soloRules.includes(o.rule ?? ""))),
  );
}
