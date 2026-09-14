// agentd.ts — typed client for the agentflow daemon's control API. The daemon
// serves this UI and the API from the same origin, so requests go to relative
// paths. Device-token auth: the token gained on the /pair page is stored in
// localStorage and every call sends Authorization: Bearer <token>.
import { z } from "zod";

// agentdBase is "" (same origin) in every shipped build. NEXT_PUBLIC_AGENTD_URL
// exists only so `next dev` on its own port can talk to a daemon elsewhere.
export const agentdBase = (process.env.NEXT_PUBLIC_AGENTD_URL ?? "").replace(/\/+$/, "");

// agentdWsUrl turns an API path into a WebSocket URL on the daemon: the page's
// own host (ws: or wss: to match the page), or the dev override's host.
export function agentdWsUrl(path: string): string {
  if (agentdBase) return agentdBase.replace(/^http/, "ws") + path;
  const scheme = window.location.protocol === "https:" ? "wss" : "ws";
  return `${scheme}://${window.location.host}${path}`;
}

// The device token lives only in localStorage and is revocable from the pair
// page. It is never stored anywhere the browser attaches automatically: with
// bearer-only auth a cross-site page cannot ride on this device's access.
const TOKEN_KEY = "agentflow.agentd.deviceToken";

export function getDeviceToken(): string {
  if (typeof window === "undefined") return "";
  try {
    return window.localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

// TOKEN_SET_EVENT fires when a device token is stored (pairing succeeded).
// The AuthGate listens so it flips to "in" because the TOKEN changed, rather
// than incidentally because the route did — which is what it relied on
// before, and which would have started flashing the gate the moment anything
// navigated differently.
export const TOKEN_SET_EVENT = "af:token-set";

export function setDeviceToken(token: string) {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(TOKEN_KEY, token);
  window.dispatchEvent(new Event(TOKEN_SET_EVENT));
}

// TOKEN_CLEARED_EVENT fires whenever a stored device token is evicted (401
// revoked/expired). The AuthGate listens and bounces to /pair immediately
// instead of on the next navigation.
export const TOKEN_CLEARED_EVENT = "af:token-cleared";

export function clearDeviceToken() {
  if (typeof window === "undefined") return;
  window.localStorage.removeItem(TOKEN_KEY);
  window.dispatchEvent(new Event(TOKEN_CLEARED_EVENT));
}

// The only secret that can enroll a browser is a pairing token from
// `agentflow pair`. A device that loses its stored token pairs again.
export function agentdReady(): boolean {
  return getDeviceToken() !== "";
}

// --- zod schemas (mirror internal/agentapi + spawner) -----------------------------

export const runSession = z.object({
  id: z.string(),
  sessionId: z.string().optional().default(""),
  kind: z.string().optional().default("chat"),
  cwd: z.string().optional().default(""),
  project: z.string().optional().default(""),
  model: z.string().optional().default(""),
  prompt: z.string().optional().default(""),
  resumeFrom: z.string().optional().default(""),
  state: z.string(), // starting | running | awaiting | finished | stopped | crashed
  blocked: z.boolean().optional().default(false),
  approvalsEnabled: z.boolean().optional().default(false),
  pid: z.number().optional().default(0),
  startedAt: z.number().optional().default(0),
  endedAt: z.number().optional().default(0),
  exitCode: z.number().optional().default(0),
  eventCount: z.number().optional().default(0),
  permissionDenials: z.number().optional().default(0),
  totalCostUsd: z.number().optional().default(0),
  terminalReason: z.string().optional().default(""),
  lastError: z.string().optional().default(""),
  createdBy: z.string().optional().default(""),
  updatedAt: z.number().optional().default(0),
  // engine defaults to "claude" so rows written before OpenCode support
  // deserialize without complaint; the control fields default to empty.
  engine: z.string().optional().default("claude"),
  controlPort: z.number().optional().default(0),
  controlBase: z.string().optional().default(""),
  // title is the name the session's own transcript knows it by (a /rename
  // in the TUI, an ai-title, or the spawn-time title). Terminal runs have no
  // prompt, so without this every run in a repo listed as the repo's name.
  title: z.string().optional().default(""),
  // loop is present only when this run drives a loop member. The join lives
  // on the server because loop_members.run_id is the only thing that knows;
  // absent for a session started from the run form.
  loop: z
    .object({
      id: z.string(),
      title: z.string().optional().default(""),
      task: z.string().optional().default(""),
      role: z.string(),
      play: z.string().optional().default(""),
      stepId: z.string().optional().default(""),
      health: z.string().optional().default(""),
    })
    .optional(),
});

export type RunSession = z.infer<typeof runSession>;

export const spawnedRun = z.object({
  id: z.string(),
  state: z.string(),
  kind: z.string().optional().default("chat"),
  prompt: z.string().optional().default(""),
  resumeFrom: z.string().optional().default(""),
});
export type SpawnedRun = z.infer<typeof spawnedRun>;

export const deviceRow = z.object({
  id: z.string(),
  name: z.string().optional().default(""),
  machineId: z.string().optional().default(""),
  status: z.string().optional().default(""),
  createdAt: z.number().optional().default(0),
  lastSeen: z.number().optional().default(0),
});
export type DeviceRow = z.infer<typeof deviceRow>;

export const deviceList = z.object({ devices: z.array(deviceRow).default([]) });

export const pairComplete = z.object({
  deviceId: z.string(),
  deviceToken: z.string(),
  machineId: z.string().optional().default(""),
});

// --- request helper ---------------------------------------------------------------

export class AgentdError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function req<T>(
  schema: z.ZodType<T>,
  path: string,
  init?: RequestInit,
): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(init?.headers as Record<string, string> | undefined),
  };
  const tok = getDeviceToken();
  if (tok) headers.Authorization = `Bearer ${tok}`;
  let resp: Response;
  try {
    resp = await fetch(agentdBase + path, {
      ...init,
      headers,
      cache: "no-store",
    });
  } catch {
    throw new AgentdError(
      0,
      "unreachable",
      "agentflow is unreachable — is the daemon running?",
    );
  }
  if (!resp.ok) {
    let code = "error";
    let message = `HTTP ${resp.status}`;
    if (resp.status === 401 && tok) {
      // the token this browser holds is revoked/expired/unknown — a stale one
      // from a past revoke lingers in storage and would fail forever. Evict
      // it so the next /pair visit starts clean.
      clearDeviceToken();
      message = "This device is not paired — open /pair/ to pair.";
    }
    try {
      const body = (await resp.json()) as {
        error?: { code?: string; message?: string };
      };
      code = body.error?.code ?? code;
      message = body.error?.message ?? message;
    } catch {
      // non-JSON error body
    }
    throw new AgentdError(resp.status, code, message);
  }
  const parsed = schema.safeParse(await resp.json());
  if (!parsed.success) {
    throw new AgentdError(500, "schema_mismatch", parsed.error.message);
  }
  return parsed.data;
}

// --- API --------------------------------------------------------------------------

export function completePair(args: {
  token: string;
  name?: string;
}): Promise<z.infer<typeof pairComplete>> {
  return req(pairComplete, "/api/v1/agentd/pair/complete", {
    method: "POST",
    body: JSON.stringify({ token: args.token, name: args.name }),
  });
}

// Access requests: a browser without a pairing link asks for access and the
// person at the computer approves it. The request id and poll secret are the
// only things that can collect the device token, so they stay in memory on
// the page that asked.
const pairRequestCreated = z.object({
  requestId: z.string(),
  pollSecret: z.string(),
  matchCode: z.string(),
  expiresAt: z.number(),
});
export type PairRequestCreated = z.infer<typeof pairRequestCreated>;

export function requestPairAccess(name: string): Promise<PairRequestCreated> {
  return req(pairRequestCreated, "/api/v1/agentd/pair/request", {
    method: "POST",
    body: JSON.stringify({ name }),
  });
}

const pairRequestStatus = z.object({
  status: z.enum(["pending", "approved", "denied", "expired"]),
  deviceToken: z.string().optional(),
  deviceId: z.string().optional(),
});
export type PairRequestStatus = z.infer<typeof pairRequestStatus>;

// pollPairRequest answers 404 (AgentdError) for a request the daemon no
// longer knows, including one whose token was already collected.
export function pollPairRequest(requestId: string, pollSecret: string): Promise<PairRequestStatus> {
  return req(pairRequestStatus, `/api/v1/agentd/pair/request/${encodeURIComponent(requestId)}`, {
    headers: { "X-Pair-Secret": pollSecret },
  });
}

export const pendingPairRequest = z.object({
  id: z.string(),
  name: z.string(),
  matchCode: z.string(),
  clientIp: z.string().optional().default(""),
  createdAt: z.number().optional().default(0),
  expiresAt: z.number().optional().default(0),
});
export type PendingPairRequest = z.infer<typeof pendingPairRequest>;

// listPairRequests is only served on this computer's own address; elsewhere it
// answers 404 and the page hides the section.
export function listPairRequests(): Promise<PendingPairRequest[]> {
  return req(
    z.object({ requests: z.array(pendingPairRequest).nullish().transform((v) => v ?? []) }),
    "/api/v1/agentd/pair/requests",
  ).then((r) => r.requests);
}

export function decidePairRequest(id: string, approve: boolean) {
  return req(
    z.object({ decision: z.string() }),
    `/api/v1/agentd/pair/requests/${encodeURIComponent(id)}/${approve ? "approve" : "deny"}`,
    { method: "POST" },
  );
}

// refreshDeviceCookie asks the daemon for the cookie that lets a paired browser
// load the app on the public address. Pairing sets it; a browser paired
// before the cookie existed picks it up here. A 204 has no body to parse.
export async function refreshDeviceCookie(): Promise<void> {
  const tok = getDeviceToken();
  if (!tok) return;
  const resp = await fetch(agentdBase + "/api/v1/agentd/pair/cookie", {
    method: "POST",
    headers: { Authorization: `Bearer ${tok}` },
    cache: "no-store",
  });
  if (resp.status === 401) clearDeviceToken();
}

export function listDevices(): Promise<z.infer<typeof deviceList>> {
  return req(deviceList, "/api/v1/agentd/devices");
}

export function revokeDevice(id: string) {
  return req(
    z.object({ revoked: z.string() }),
    `/api/v1/agentd/devices/${encodeURIComponent(id)}/revoke`,
    {
      method: "POST",
    },
  );
}

// cwds — recently used working directories for the run composer, most recent
// first. Read-only on the daemon side.
export const cwdEntry = z.object({
  cwd: z.string(),
  lastUsedAt: z.number().optional().default(0),
});
export type CwdEntry = z.infer<typeof cwdEntry>;

export const cwdsResponse = z.object({
  // Go encodes a nil slice as null; treat it as an empty list.
  items: z
    .array(cwdEntry)
    .nullish()
    .transform((v) => v ?? []),
});

export function listCwds(): Promise<CwdEntry[]> {
  return req(cwdsResponse, "/api/v1/agentd/cwds").then((r) => r.items);
}

export interface SpawnRequest {
  kind?: "chat" | "one_shot" | "tty";
  prompt: string;
  title?: string;
  model?: string;
  agent?: string;
  engine?: "claude" | "opencode" | "";
  cwd?: string;
  project?: string;
  allowedTools?: string[];
  permissionMode?: string;
  effort?: string;
  resumeSessionId?: string;
  // promptMode selects the standing text this session carries. "scratch" is
  // NOT off: it drops the advisory rules and keeps the binding ones, because
  // someone reaching for scratch wants to skip the reporting contract on a
  // throwaway, not to authorise a push to main.
  promptMode?: "standing" | "scratch";
}

export function spawnRun(body: SpawnRequest): Promise<SpawnedRun> {
  return req(spawnedRun, "/api/v1/agentd/sessions", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function resumeRun(id: string, prompt?: string): Promise<SpawnedRun> {
  return req(
    spawnedRun,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/resume`,
    {
      method: "POST",
      body: JSON.stringify({ prompt }),
    },
  );
}

export function listRuns(): Promise<{ items: RunSession[] }> {
  return req(
    z.object({ items: z.array(runSession) }),
    "/api/v1/agentd/sessions",
  );
}

// allSessionItem is one row of the sessions home: every Claude Code and
// OpenCode session on the machine, whether or not agentflow started it. id is
// the engine's session id (or the run id of a run that has none yet); state is
// the live run state while a run holds the session, "" otherwise. model is ""
// when the daemon doesn't report one.
export const allSessionItem = z.object({
  id: z.string(),
  engine: z.string().optional().default("claude"),
  model: z.string().optional().default(""),
  title: z.string().optional().default(""),
  cwd: z.string().optional().default(""),
  project: z.string().optional().default(""),
  updatedAt: z.number().optional().default(0),
  state: z.string().optional().default(""),
  runId: z.string().optional().default(""),
});

export type AllSessionItem = z.infer<typeof allSessionItem>;

const allSessionsPage = z.object({
  items: z.array(allSessionItem),
  nextCursor: z.string().optional().default(""),
});

export type AllSessionsPage = z.infer<typeof allSessionsPage>;

export function listAllSessions(
  opts: { limit?: number; cursor?: string } = {},
): Promise<AllSessionsPage> {
  const q = new URLSearchParams();
  if (opts.limit) q.set("limit", String(opts.limit));
  if (opts.cursor) q.set("cursor", opts.cursor);
  const qs = q.toString();
  return req(allSessionsPage, `/api/v1/agentd/all-sessions${qs ? `?${qs}` : ""}`);
}

export function runStatus(id: string): Promise<RunSession> {
  return req(runSession, `/api/v1/agentd/sessions/${encodeURIComponent(id)}`);
}

// runForSession finds the managed run that holds a claude session id.
export function runForSession(sessionId: string): Promise<RunSession> {
  return req(
    z.object({ run: runSession }),
    `/api/v1/agentd/runs/by-session/${encodeURIComponent(sessionId)}`,
  ).then((r) => r.run);
}

// isRunStopped reports the daemon's answer for a run that was stopped on
// purpose and needs an explicit restart.
export function isRunStopped(err: unknown): boolean {
  return err instanceof AgentdError && err.code === "run_stopped";
}

export function sendRunMessage(id: string, text: string) {
  return req(
    z.object({ sent: z.boolean() }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/message`,
    {
      method: "POST",
      body: JSON.stringify({ text }),
    },
  );
}

export function stopRun(id: string) {
  return req(
    z.object({ interrupted: z.boolean() }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/stop`,
    {
      method: "POST",
    },
  );
}

export function terminateRun(id: string) {
  return req(
    z.object({ terminated: z.boolean() }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/terminate`,
    {
      method: "POST",
    },
  );
}

// --- engine control layer -----------------------------------------------------
//
// The control sheet reads/writes engine state through these endpoints; the
// underlying Control implementations differ per engine (opencode: HTTP;
// claude: thin) but the surface is identical.

export const engineListItem = z.object({
  id: z.string(),
  version: z.string().optional().default(""),
});
export type EngineListItem = z.infer<typeof engineListItem>;

export function listEngines(): Promise<{ engines: EngineListItem[] }> {
  return req(
    z.object({ engines: z.array(engineListItem) }),
    "/api/v1/agentd/engines",
  );
}

export const engineCaps = z.object({
  engine: z.string(),
  caps: z.object({
    listModels: z.boolean().optional().default(false),
    setModel: z.boolean().optional().default(false),
    listAgents: z.boolean().optional().default(false),
    setAgent: z.boolean().optional().default(false),
    listCommands: z.boolean().optional().default(false),
    runCommand: z.boolean().optional().default(false),
    listSkills: z.boolean().optional().default(false),
    listProviders: z.boolean().optional().default(false),
    setProvider: z.boolean().optional().default(false),
    listMcpServers: z.boolean().optional().default(false),
    prompt: z.boolean().optional().default(false),
    abort: z.boolean().optional().default(false),
    replyPermission: z.boolean().optional().default(false),
    replyQuestion: z.boolean().optional().default(false),
    events: z.boolean().optional().default(false),
  }),
});
export type EngineCaps = z.infer<typeof engineCaps>;

export function getEngineCaps(id: string): Promise<EngineCaps> {
  return req(
    engineCaps,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/caps`,
  );
}

export const engineSessionState = z.object({
  model: z.string().optional().default(""),
  agent: z.string().optional().default(""),
  provider: z.string().optional().default(""),
});
export type EngineSessionState = z.infer<typeof engineSessionState>;

export const engineStateResponse = z.object({
  engine: z.string(),
  state: engineSessionState,
  note: z.string().optional().default(""),
});
export type EngineStateResponse = z.infer<typeof engineStateResponse>;

export function getEngineState(id: string): Promise<EngineStateResponse> {
  return req(
    engineStateResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/state`,
  );
}

export const engineModel = z.object({
  id: z.string(),
  displayName: z.string().optional().default(""),
  provider: z.string().optional().default(""),
  default: z.boolean().optional().default(false),
});
export type EngineModel = z.infer<typeof engineModel>;

export const engineModelsResponse = z.object({
  engine: z.string(),
  models: z.array(engineModel).default([]),
  note: z.string().optional().default(""),
});

export function listEngineModels(
  id: string,
): Promise<z.infer<typeof engineModelsResponse>> {
  return req(
    engineModelsResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/models`,
  );
}

export function setEngineModel(id: string, model: string) {
  return req(
    z.object({
      engine: z.string(),
      applied: z.string(),
      state: engineSessionState,
    }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/model`,
    { method: "POST", body: JSON.stringify({ model }) },
  );
}

export const engineAgent = z.object({
  name: z.string(),
  description: z.string().optional().default(""),
  model: z.string().optional().default(""),
  default: z.boolean().optional().default(false),
});
export type EngineAgent = z.infer<typeof engineAgent>;

export const engineAgentsResponse = z.object({
  engine: z.string(),
  agents: z.array(engineAgent).default([]),
});

export function listEngineAgents(
  id: string,
): Promise<z.infer<typeof engineAgentsResponse>> {
  return req(
    engineAgentsResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/agents`,
  );
}

export function setEngineAgent(id: string, agent: string) {
  return req(
    z.object({
      engine: z.string(),
      applied: z.string(),
      state: engineSessionState,
    }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/agent`,
    { method: "POST", body: JSON.stringify({ agent }) },
  );
}

export const engineProvider = z.object({
  id: z.string(),
  name: z.string().optional().default(""),
});
export type EngineProvider = z.infer<typeof engineProvider>;

export const engineProvidersResponse = z.object({
  engine: z.string(),
  providers: z.array(engineProvider).default([]),
});

export function listEngineProviders(
  id: string,
): Promise<z.infer<typeof engineProvidersResponse>> {
  return req(
    engineProvidersResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/providers`,
  );
}

export function setEngineProvider(id: string, provider: string) {
  return req(
    z.object({
      engine: z.string(),
      applied: z.string(),
      state: engineSessionState,
    }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/provider`,
    { method: "POST", body: JSON.stringify({ provider }) },
  );
}

export const engineMCPServer = z.object({
  name: z.string(),
  status: z.string().optional().default(""),
  url: z.string().optional().default(""),
});
export type EngineMCPServer = z.infer<typeof engineMCPServer>;

export const engineMCPResponse = z.object({
  engine: z.string(),
  servers: z.array(engineMCPServer).default([]),
});

export function listEngineMCP(
  id: string,
): Promise<z.infer<typeof engineMCPResponse>> {
  return req(
    engineMCPResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/mcp`,
  );
}

export const engineSkill = z.object({
  name: z.string(),
  description: z.string().optional().default(""),
  path: z.string().optional().default(""),
});
export type EngineSkill = z.infer<typeof engineSkill>;

export const engineSkillsResponse = z.object({
  engine: z.string(),
  skills: z.array(engineSkill).default([]),
});

export function listEngineSkills(
  id: string,
): Promise<z.infer<typeof engineSkillsResponse>> {
  return req(
    engineSkillsResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/skills`,
  );
}

export const engineCommand = z.object({
  name: z.string(),
  description: z.string().optional().default(""),
  // argsHint names what the command expects after its name ("path",
  // "alias"). Empty means it takes none, which is what lets the control
  // sheet decide between running it straight away and asking first.
  argsHint: z.string().optional().default(""),
  // source is "builtin" | "project" | "user" | "skill".
  source: z.string().optional().default(""),
});
export type EngineCommand = z.infer<typeof engineCommand>;

export const engineCommandsResponse = z.object({
  engine: z.string(),
  commands: z.array(engineCommand).default([]),
});

export function listEngineCommands(
  id: string,
): Promise<z.infer<typeof engineCommandsResponse>> {
  return req(
    engineCommandsResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/commands`,
  );
}

// sendEnginePrompt delivers a turn that carries attachments.
//
// IDS ONLY — never paths, never bytes. The bytes went straight to the PC
// over the direct data channel; this is a few hundred bytes of JSON telling
// agentd which of the files it already has to put in front of the engine.
// Text-only sends do not come here at all: they keep using the terminal's
// own paced submit.
export const enginePromptResponse = z.object({
  engine: z.string(),
  delivered: z.string(),
  attachments: z.number().optional().default(0),
  turn: z.string().optional().default(""),
});
export type EnginePromptResponse = z.infer<typeof enginePromptResponse>;

export function sendEnginePrompt(
  id: string,
  text: string,
  attachmentIds: string[] = [],
) {
  return req(
    enginePromptResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/prompt`,
    { method: "POST", body: JSON.stringify({ text, attachmentIds }) },
  );
}

export function runEngineCommand(id: string, name: string, args = "") {
  return req(
    z.object({ engine: z.string(), applied: z.string(), args: z.string() }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/command`,
    { method: "POST", body: JSON.stringify({ name, args }) },
  );
}

// --- native permissions + questions ------------------------------------------

export const enginePermission = z.object({
  id: z.string(),
  sessionID: z.string().optional().default(""),
  tool: z.string().optional().default(""),
  input: z.record(z.string(), z.any()).optional().default({}),
  title: z.string().optional().default(""),
});
export type EnginePermission = z.infer<typeof enginePermission>;

export const enginePermissionsResponse = z.object({
  engine: z.string(),
  permissions: z.array(enginePermission).default([]),
  note: z.string().optional().default(""),
});

export function listEnginePermissions(
  id: string,
): Promise<z.infer<typeof enginePermissionsResponse>> {
  return req(
    enginePermissionsResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/permissions`,
  );
}

export type PermissionScope = "session" | "always";

export function replyEnginePermission(
  id: string,
  permissionId: string,
  allow: boolean,
  scope: PermissionScope = "session",
) {
  return req(
    z.object({
      engine: z.string(),
      permissionId: z.string(),
      applied: z.boolean(),
      scope: z.string(),
    }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/permission`,
    { method: "POST", body: JSON.stringify({ permissionId, allow, scope }) },
  );
}

export const engineQuestionOption = z.object({
  label: z.string(),
  description: z.string().optional().default(""),
  preview: z.string().optional().default(""),
});
export type EngineQuestionOption = z.infer<typeof engineQuestionOption>;

export const engineQuestion = z.object({
  id: z.string(),
  sessionID: z.string().optional().default(""),
  header: z.string().optional().default(""),
  prompt: z.string().optional().default(""),
  question: z.string().optional().default(""),
  options: z.array(engineQuestionOption).default([]),
});
export type EngineQuestion = z.infer<typeof engineQuestion>;

export const engineQuestionsResponse = z.object({
  engine: z.string(),
  questions: z.array(engineQuestion).default([]),
});

export function listEngineQuestions(
  id: string,
): Promise<z.infer<typeof engineQuestionsResponse>> {
  return req(
    engineQuestionsResponse,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/questions`,
  );
}

export function replyEngineQuestion(
  id: string,
  questionId: string,
  answer: string,
) {
  return req(
    z.object({
      engine: z.string(),
      questionId: z.string(),
      applied: z.string(),
    }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/control/question`,
    { method: "POST", body: JSON.stringify({ questionId, answer }) },
  );
}

// --- "what rules are actually in effect" surface ----------------------------

export const configFile = z.object({
  path: z.string(),
  origin: z.string(),
  loaded: z.boolean(),
  bytes: z.number(),
  excerpt: z.string().optional().default(""),
});
export type ConfigFile = z.infer<typeof configFile>;

export const configHook = z.object({
  event: z.string(),
  matcher: z.string().optional().default(""),
  command: z.string(),
  fireCount: z.number().optional().default(0),
});
export type ConfigHook = z.infer<typeof configHook>;

export const configSkill = z.object({
  name: z.string(),
  description: z.string().optional().default(""),
  path: z.string(),
});
export type ConfigSkill = z.infer<typeof configSkill>;

export const configAgent = z.object({
  name: z.string(),
  description: z.string().optional().default(""),
  path: z.string(),
  model: z.string().optional().default(""),
});
export type ConfigAgent = z.infer<typeof configAgent>;

// A nil Go slice marshals as `null`, not `[]`, and zod's .default() only
// fills in for `undefined` — so `"skills": null` was a hard parse failure
// and the config page rendered "Failed to load config." for every Claude
// run. Absent, null and empty all mean the same thing to a reader, so they
// mean the same thing here.
const listOf = <T extends z.ZodTypeAny>(item: T) =>
  z
    .array(item)
    .nullish()
    .transform((v) => v ?? []);

export const configSettings = z.object({
  model: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
  permissionMode: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
  allowedTools: listOf(z.string()),
  hooks: listOf(configHook),
});
export type ConfigSettings = z.infer<typeof configSettings>;

export const configSummary = z.object({
  files: listOf(configFile),
  settings: configSettings,
  skills: listOf(configSkill),
  agents: listOf(configAgent),
});
export type ConfigSummary = z.infer<typeof configSummary>;

export const sessionConfig = z.object({
  engine: z.string(),
  cwd: z.string().optional().default(""),
  summary: configSummary,
  // null for every Claude run (no ControlBase), which is the common case.
  engineExtras: z.record(z.string(), z.any()).nullish(),
});
export type SessionConfig = z.infer<typeof sessionConfig>;

export function getSessionConfig(id: string): Promise<SessionConfig> {
  return req(
    sessionConfig,
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/config`,
  );
}

// startRunTty is the idempotent tty revive path (Refresh connection): a live
// run is only re-registered with the hub; a finished one is resumed. A run
// that was stopped on purpose stays stopped (the daemon answers 410
// run_stopped) unless the caller asks for an explicit restart.
// geom lets a fresh spawn open at the viewer's real size from birth.
export function startRunTty(
  id: string,
  geom?: { cols: number; rows: number } | null,
  opts?: { restart?: boolean },
): Promise<{ run: string; tty: boolean }> {
  const q = new URLSearchParams();
  if (geom && geom.cols >= 20 && geom.rows >= 5) {
    q.set("cols", String(Math.round(geom.cols)));
    q.set("rows", String(Math.round(geom.rows)));
  }
  if (opts?.restart) q.set("restart", "1");
  const qs = q.toString();
  return req(
    z.object({ run: z.string(), tty: z.boolean() }),
    `/api/v1/agentd/sessions/${encodeURIComponent(id)}/tty${qs ? `?${qs}` : ""}`,
    { method: "POST" },
  );
}

// ensureTtyBySession is the session→terminal bridge: any claude session —
// managed run or plain local transcript — comes back as a LIVE tty run id
// (existing run attached, terminal run restarted, unmanaged session resumed
// into a fresh TUI run). Every session is resumable as a terminal. geom is
// the caller's best-known viewport size so the fresh TUI's resume replay is
// wrapped correctly from its very first byte.
export function ensureTtyBySession(
  sessionId: string,
  geom?: { cols: number; rows: number } | null,
): Promise<{ id: string; sessionId: string }> {
  const q = new URLSearchParams();
  if (geom && geom.cols >= 20 && geom.rows >= 5) {
    q.set("cols", String(Math.round(geom.cols)));
    q.set("rows", String(Math.round(geom.rows)));
  }
  const qs = q.toString();
  return req(
    z.object({ id: z.string(), sessionId: z.string().optional().default("") }),
    `/api/v1/agentd/sessions/by-session/${encodeURIComponent(sessionId)}/tty${qs ? `?${qs}` : ""}`,
    { method: "POST" },
  );
}
