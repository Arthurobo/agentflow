// loop.ts — typed client for the engineer-facing loop surface
// (/api/v1/loops, device-token auth). Separate from the agentd control API
// client above it: this drives the engine-owned loop, where members pull work
// over HTTP and the engine owns the sequence.
//
import { z } from "zod";
import { agentdBase, getDeviceToken } from "@/lib/agentd";

export const loopBase = agentdBase + "/api/v1/loops";

// The code-tool registry is not a loop-owned resource: it is the only
// session-independent list of model names in the product. It is served from a
// neutral route so a caller that is not a loop can read it without pretending
// to be one.
export const toolsBase = agentdBase + "/api/v1/tools";

const loopSchema = z.object({
  id: z.string(),
  title: z.string().optional().default(""),
  task: z.string().optional().default(""),
  cwd: z.string().optional().default(""),
  status: z.string(),
  round: z.number().optional().default(0),
  state: z.string().optional().default(""),
  parentLoopId: z.string().optional().default(""),
  endReason: z.string().optional().default(""),
  play: z.string().optional().default(""),
  playStatus: z.string().optional().default(""),
  stepId: z.string().optional().default(""),
  stepAwaiting: z.array(z.string()).optional().default([]),
  stepEnteredAt: z.number().optional().default(0),
  createdAt: z.number().optional().default(0),
  completedAt: z.number().optional().default(0),
});
export type Loop = z.infer<typeof loopSchema>;

const crewMemberSchema = z.object({
  role: z.string(),
  agentId: z.string(),
  status: z.string(),
  tool: z.string().optional().default(""),
  model: z.string().optional().default(""),
  charsIn: z.number().optional().default(0),
  notes: z.number().optional().default(0),
  lastSeenAt: z.number().optional().default(0),
  // runId is the link to the process driving this member, empty when the
  // member runs somewhere we did not start it. It exists before the process
  // does, which is why it and not sessionId is what every screen keys on.
  runId: z.string().optional().default(""),
  // sessionId is the engine's own id once it reports one. Permanently empty
  // for an OpenCode member: it keeps its own store and writes nothing to the
  // claude corpus.
  sessionId: z.string().optional().default(""),
  // health is the server's own verdict on whether anyone is reading this
  // member's mail. The client cannot compute it: only agentd knows which long
  // polls it is holding. Defaulted so an older agentd still parses.
  health: z.string().optional().default(""),
  listening: z.boolean().optional().default(false),
  listeningSince: z.number().optional().default(0),
  /** held: briefs it has and has not discharged. This is what "working" is. */
  held: z.number().optional().default(0),
  /** pending: mail nobody has claimed. */
  pending: z.number().optional().default(0),
  oldestPendingAt: z.number().optional().default(0),
  strandedSince: z.number().optional().default(0),
  runState: z.string().optional().default(""),
  // lastError is why this member has no process.
  lastError: z.string().optional().default(""),
});
export type CrewMember = z.infer<typeof crewMemberSchema>;

const messageSchema = z.object({
  id: z.string(),
  loopId: z.string(),
  senderRole: z.string().optional().default(""),
  recipientRole: z.string().optional().default(""),
  subject: z.string().optional().default(""),
  body: z.string().optional().default(""),
  bodyChars: z.number().optional().default(0),
  status: z.string(),
  deliveryCount: z.number().optional().default(0),
  forwardedFrom: z.string().optional().default(""),
  mirrorOf: z.string().optional().default(""),
  outcome: z.string().optional().default(""),
  stepId: z.string().optional().default(""),
  createdAt: z.number().optional().default(0),
  deliveredAt: z.number().optional().default(0),
  ackedAt: z.number().optional().default(0),
});
export type LoopMessage = z.infer<typeof messageSchema>;


// LoopRefusal is a refusal as a ROW. Refusals used to exist only as live
// events, so one was visible if you happened to be watching and gone if you
// were not. The chat is a projection over rows; this is the row.
const refusalSchema = z.object({
  id: z.string(),
  loopId: z.string(),
  kind: z.string(),
  fromRole: z.string().optional().default(""),
  toRole: z.string().optional().default(""),
  play: z.string().optional().default(""),
  stepId: z.string().optional().default(""),
  content: z.string().optional().default(""),
  detail: z.record(z.string(), z.unknown()).optional().default({}),
  createdAt: z.number().optional().default(0),
});
export type LoopRefusal = z.infer<typeof refusalSchema>;

const playStepSchema = z.object({
  id: z.string(),
  actor: z.string(),
  note: z.string().optional().default(""),
  fanIn: z.boolean().optional().default(false),
});
export type PlayStep = z.infer<typeof playStepSchema>;

const playSchema = z.object({
  name: z.string(),
  title: z.string(),
  // purpose is one sentence saying what the play is FOR. The role chain says
  // how it works, which is not the same question.
  purpose: z.string().optional().default(""),
  entry: z.string(),
  maxRounds: z.number().optional().default(0),
  hasCycle: z.boolean().optional().default(false),
  roles: z.array(z.string()).optional().default([]),
  steps: z.array(playStepSchema).optional().default([]),
});
export type Play = z.infer<typeof playSchema>;

// ToolSpec carries `probed` on purpose: an unprobed tool's model list has not
// been checked against the real thing, and the wizard must say so rather than
// let someone pick a model believing it was verified.
const toolSchema = z.object({
  id: z.string(),
  title: z.string(),
  models: z.array(z.string()).optional().default([]),
  probed: z.boolean(),
  defaultModelAllowed: z.boolean(),
  note: z.string().optional().default(""),
});
export type ToolSpec = z.infer<typeof toolSchema>;

const handoffSchema = z.object({
  role: z.string(),
  agentId: z.string(),
  tool: z.string().optional().default(""),
  model: z.string().optional().default(""),
  token: z.string().optional().default(""),
  prompt: z.string().optional().default(""),
});
export type Handoff = z.infer<typeof handoffSchema>;

const loopDetailSchema = z.object({
  loop: loopSchema,
  crew: z.array(crewMemberSchema).optional().default([]),
  messageCounts: z.record(z.string(), z.number()).optional().default({}),
  stepBrief: z.string().optional().default(""),
  stepNote: z.string().optional().default(""),
  stepActor: z.string().optional().default(""),
  maxRounds: z.number().optional().default(0),
});
export type LoopDetail = z.infer<typeof loopDetailSchema>;

const createResultSchema = z.object({
  loop: loopSchema,
  handoffs: z.array(handoffSchema).optional().default([]),
  // spawning says agentd started the members itself, so there is nothing to
  // paste and the engineer goes straight to the board.
  spawning: z.boolean().optional().default(false),
});
export type CreateResult = z.infer<typeof createResultSchema>;

export class LoopError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly details?: unknown,
  ) {
    super(message);
    this.name = "LoopError";
  }
}

async function reqAt<T>(
  base: string,
  schema: z.ZodType<T>,
  path: string,
  init?: RequestInit,
): Promise<T> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const tok = getDeviceToken();
  if (tok) headers.Authorization = `Bearer ${tok}`;
  let resp: Response;
  try {
    resp = await fetch(base + path, { ...init, headers, cache: "no-store" });
  } catch {
    throw new LoopError(0, "unreachable", "agentflow is unreachable — is the daemon running?");
  }
  if (!resp.ok) {
    let code = "error";
    let message = `HTTP ${resp.status}`;
    let details: unknown;
    try {
      const body = (await resp.json()) as {
        error?: { code?: string; message?: string; details?: unknown };
      };
      code = body.error?.code ?? code;
      message = body.error?.message ?? message;
      details = body.error?.details;
    } catch {
      // a non-JSON error body stays as the status line
    }
    throw new LoopError(resp.status, code, message, details);
  }
  return schema.parse(await resp.json());
}

function req<T>(schema: z.ZodType<T>, path: string, init?: RequestInit): Promise<T> {
  return reqAt(loopBase, schema, path, init);
}

export function listPlays(): Promise<Play[]> {
  return req(z.object({ plays: z.array(playSchema) }), "/plays").then((r) => r.plays);
}

const toolModelsSchema = z.object({
  models: z
    .array(
      z.object({
        id: z.string(),
        displayName: z.string().optional().default(""),
        provider: z.string().optional().default(""),
        status: z.string().optional().default(""),
      }),
    )
    .optional()
    .default([]),
  probed: z.boolean().optional().default(false),
  live: z.boolean().optional().default(false),
  note: z.string().optional().default(""),
});
export type ToolModels = z.infer<typeof toolModelsSchema>;

// listToolModels asks what one tool can run RIGHT NOW. For opencode the answer
// is a property of this install rather than of opencode, so it cannot be a
// constant. The response is a projection: opencode's own listing carries a
// live provider credential on every entry and agentd drops it.
export function listToolModels(tool: string, cwd?: string): Promise<ToolModels> {
  const q = cwd ? `?cwd=${encodeURIComponent(cwd)}` : "";
  return reqAt(toolsBase, toolModelsSchema, `/${encodeURIComponent(tool)}/models${q}`);
}

export function listTools(): Promise<ToolSpec[]> {
  return reqAt(toolsBase, z.object({ tools: z.array(toolSchema) }), "").then((r) => r.tools);
}

/** loopStanding is what a row should say a loop is doing.
 *
 *  A play reaching its terminal step does not end the loop, by design: the
 *  engineer may continue it. But the row said "active · recon · conclude ·
 *  round 0", which reads exactly like a loop still working, so two finished
 *  loops sat in the list looking like two running ones. */
export function loopStanding(l: {
  status: string;
  play?: string;
  playStatus?: string;
}): string {
  if (l.status !== "active") return l.status;
  if (l.play && l.playStatus === "done") return "play finished, loop open";
  return l.status;
}

export function listLoops(status?: string): Promise<Loop[]> {
  const q = status ? `?status=${encodeURIComponent(status)}` : "";
  return req(z.object({ loops: z.array(loopSchema) }), q).then((r) => r.loops);
}

/** listLoopsWithAttention also returns what each loop needs a human for. */
export function listLoopsWithAttention(
  status?: string,
): Promise<{ loops: Loop[]; attention: Record<string, LoopAttention> }> {
  const q = status ? `?status=${encodeURIComponent(status)}` : "";
  return req(
    z.object({
      loops: z.array(loopSchema),
      attention: z.record(z.string(), attentionSchema).optional().default({}),
    }),
    q,
  );
}

export function getLoop(id: string): Promise<LoopDetail> {
  return req(loopDetailSchema, `/${id}`);
}

export interface MemberDraft {
  role: string;
  tool?: string;
  model?: string;
}

export function createLoop(input: {
  title: string;
  task: string;
  cwd?: string;
  play: string;
  crew: MemberDraft[];
  pollIntervalSeconds?: number;
}): Promise<CreateResult> {
  return req(createResultSchema, "", { method: "POST", body: JSON.stringify(input) });
}

/** LoopAttention is what a loop needs a human for, keyed by loop id. Absent
 *  for a loop that needs nothing, and absent entirely from an older agentd. */
const attentionSchema = z.object({
  stranded: z.array(z.string()).optional().default([]),
  failed: z.array(z.string()).optional().default([]),
});
export type LoopAttention = z.infer<typeof attentionSchema>;

export function loopMessages(id: string): Promise<LoopMessage[]> {
  return req(z.object({ messages: z.array(messageSchema) }), `/${id}/messages`).then(
    (r) => r.messages,
  );
}

export function loopRefusals(id: string): Promise<LoopRefusal[]> {
  return req(z.object({ refusals: z.array(refusalSchema) }), `/${id}/refusals`).then(
    (r) => r.refusals,
  );
}

export function endLoop(
  id: string,
  input: { reason?: string; dismissAgents?: boolean },
): Promise<{ loopId: string; status: string; agents: string }> {
  return req(
    z.object({ loopId: z.string(), status: z.string(), agents: z.string() }),
    `/${id}/end`,
    { method: "POST", body: JSON.stringify(input) },
  );
}

export function rotateMember(id: string, role: string): Promise<Handoff> {
  return req(z.object({ handoff: handoffSchema }), `/${id}/members/${role}/rotate`, {
    method: "POST",
  }).then((r) => r.handoff);
}

export function retireMember(id: string, role: string): Promise<{ role: string; status: string }> {
  return req(
    z.object({ role: z.string(), status: z.string() }),
    `/${id}/members/${role}/retire`,
    { method: "POST" },
  );
}

// dismissLoop retires every member outright. Ending a loop only PARKS them
// with live tokens, so this is the separate, harsher verb and the UI has to
// say which one it is calling.
export function dismissLoop(id: string): Promise<{ loopId: string; dismissed: number }> {
  return req(z.object({ loopId: z.string(), dismissed: z.number() }), `/${id}/dismiss`, {
    method: "POST",
  });
}

const continueResultSchema = z.object({
  loop: loopSchema,
  continuedFrom: z.string().optional().default(""),
  keptAgents: z.array(z.string()).optional().default([]),
  notesCarried: z.number().optional().default(0),
  handoffs: z.array(handoffSchema).optional().default([]),
});
export type ContinueResult = z.infer<typeof continueResultSchema>;

// continueLoop ends this loop and starts a successor that ADOPTS the crew, so
// the members keep the tokens already pasted into their sessions. Only a role
// the successor gains gets a fresh handoff.
export function continueLoop(
  id: string,
  input: { task: string; play?: string; carryNotes?: boolean },
): Promise<ContinueResult> {
  return req(continueResultSchema, `/${id}/continue`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export function addMember(id: string, spec: MemberDraft): Promise<Handoff> {
  return req(z.object({ handoff: handoffSchema }), `/${id}/members`, {
    method: "POST",
    body: JSON.stringify(spec),
  }).then((r) => r.handoff);
}
