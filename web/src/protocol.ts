export const SESSION_STATES = ["PARKED", "PARKING", "QUIET", "LIVE"] as const;
export type SessionState = (typeof SESSION_STATES)[number];

export const JOB_STATUSES = [
  "queued",
  "running",
  "needs_input",
  "needs_approval",
  "parked_approval",
  "done",
  "failed",
  "cancelled",
] as const;
export type JobStatus = (typeof JOB_STATUSES)[number];

export const EVENT_KINDS = [
  "job.queued",
  "job.running",
  "job.needs_input",
  "job.needs_approval",
  "job.approval_parked",
  "job.approval_resolved",
  "job.approval_auto_denied",
  "job.approval_auto_allowed",
  "job.progress",
  "job.done",
  "job.failed",
  "job.cancelled",
  "job.renamed",
  "transcript.user",
  "transcript.assistant",
  "session.state",
  "hermes.hook",
  "error.voice",
  "usage",
] as const;
export type EventKind = (typeof EVENT_KINDS)[number];

export const JOB_SNAPSHOT_EVENT_KINDS = [
  "job.queued",
  "job.running",
  "job.needs_input",
  "job.needs_approval",
  "job.approval_parked",
  "job.approval_resolved",
  "job.progress",
  "job.done",
  "job.failed",
  "job.cancelled",
] as const;
type JobSnapshotEventKind = (typeof JOB_SNAPSHOT_EVENT_KINDS)[number];

export const HANDLED_EVENT_KINDS = [
  ...JOB_SNAPSHOT_EVENT_KINDS,
  "job.renamed",
  "transcript.user",
  "transcript.assistant",
  "session.state",
  "error.voice",
] as const;

// These events are intentionally telemetry-only in the browser. Keeping the
// list explicit makes the Go/Web contract test fail when either side changes.
export const INTENTIONALLY_IGNORED_EVENT_KINDS = [
  "job.approval_auto_denied",
  "job.approval_auto_allowed",
  "hermes.hook",
  "usage",
] as const;
type IgnoredEventKind = (typeof INTENTIONALLY_IGNORED_EVENT_KINDS)[number];

export interface Approval {
  id: string;
  state: "pending" | "sending" | "parked" | "indeterminate";
  description?: string;
  command?: string;
  pattern_keys?: string[];
  allow_permanent: boolean;
  parked_at?: string;
  park_reason?: string;
}

export interface Progress {
  tool?: string;
  label?: string;
  state: string;
  updated_at: string;
}

export interface Job {
  id: string;
  display_name: string;
  status: JobStatus;
  question?: string;
  approvals?: Approval[];
  progress?: Progress;
  result_digest?: string;
  error?: string;
  updated_at?: string;
}

interface EventBase<K extends EventKind, P> {
  seq: number;
  ts: string;
  kind: K;
  job_id?: string;
  payload: P;
}

export type JobSnapshotEvent = {
  [K in JobSnapshotEventKind]: EventBase<K, Job> & { job_id: string };
}[JobSnapshotEventKind];
type TranscriptEvent = {
  [K in "transcript.user" | "transcript.assistant"]: EventBase<K, { text: string }>;
}["transcript.user" | "transcript.assistant"];
type IgnoredEvent = {
  [K in IgnoredEventKind]: EventBase<K, unknown>;
}[IgnoredEventKind];
export type BusEvent =
  | JobSnapshotEvent
  | (EventBase<"job.renamed", { to: string }> & { job_id: string })
  | TranscriptEvent
  | EventBase<"session.state", { state: SessionState; reason: string }>
  | EventBase<"error.voice", { message: string; play?: string }>
  | IgnoredEvent;

export type ServerMessage = { type: "hb" } | { type: "event"; event: BusEvent };
export type ServerMsg = ServerMessage;

const eventKinds = new Set<string>(EVENT_KINDS);
const jobSnapshotKinds = new Set<string>(JOB_SNAPSHOT_EVENT_KINDS);
const ignoredKinds = new Set<string>(INTENTIONALLY_IGNORED_EVENT_KINDS);
const jobStatuses = new Set<string>(JOB_STATUSES);
const approvalStates = new Set<string>(["pending", "sending", "parked", "indeterminate"]);
const terminalStatuses = new Set<JobStatus>(["done", "failed", "cancelled"]);

export function parseServerMessage(raw: unknown): ServerMessage | null {
  let value = raw;
  if (typeof raw === "string") {
    try {
      value = JSON.parse(raw);
    } catch {
      return null;
    }
  }
  if (!isRecord(value) || typeof value.type !== "string") return null;
  if (value.type === "hb") return { type: "hb" };
  if (value.type !== "event") return null;
  const event = parseBusEvent(value.event);
  return event ? { type: "event", event } : null;
}

export function parseJobSnapshot(event: unknown): Job | null {
  const parsed = parseBusEvent(event);
  return parsed && jobSnapshotKinds.has(parsed.kind) ? sanitizeJob(parsed.payload, parsed.job_id) : null;
}

export function parseSessionPayload(value: unknown): { state: SessionState; reason: string } | null {
  if (!isRecord(value) || typeof value.state !== "string" || !SESSION_STATES.includes(value.state as SessionState)) {
    return null;
  }
  if (!optionalString(value, "reason")) return null;
  return { state: value.state as SessionState, reason: typeof value.reason === "string" ? value.reason : "" };
}

export function upsertJobHistory(current: Map<string, Job>, job: Job, maxTerminal = 50): Map<string, Job> {
  const existing = current.get(job.id);
  if (existing?.updated_at && job.updated_at) {
    const existingTime = Date.parse(existing.updated_at);
    const incomingTime = Date.parse(job.updated_at);
    if (Number.isFinite(existingTime) && Number.isFinite(incomingTime) && incomingTime < existingTime) return current;
  }

  const next = new Map(current);
  // Refresh insertion order so pruning retains the most recently updated
  // terminal snapshots while active and parked work is never discarded.
  next.delete(job.id);
  next.set(job.id, job);
  const terminal = [...next.values()].filter((item) => terminalStatuses.has(item.status));
  for (const expired of terminal.slice(0, Math.max(0, terminal.length - maxTerminal))) {
    next.delete(expired.id);
  }
  return next;
}

function parseBusEvent(value: unknown): BusEvent | null {
  if (
    !isRecord(value) ||
    typeof value.seq !== "number" ||
    !Number.isSafeInteger(value.seq) ||
    value.seq < 0 ||
    typeof value.ts !== "string" ||
    value.ts.length === 0 ||
    typeof value.kind !== "string" ||
    !eventKinds.has(value.kind) ||
    !optionalString(value, "job_id")
  ) {
    return null;
  }

  const base = { seq: value.seq, ts: value.ts };
  const jobID = typeof value.job_id === "string" ? value.job_id : undefined;
  if (jobSnapshotKinds.has(value.kind)) {
    if (!jobID) return null;
    const job = sanitizeJob(value.payload, jobID);
    if (!job) return null;
    return { ...base, kind: value.kind as JobSnapshotEventKind, job_id: jobID, payload: job };
  }
  if (value.kind === "job.renamed") {
    if (!jobID || !isRecord(value.payload) || typeof value.payload.to !== "string") return null;
    return { ...base, kind: value.kind, job_id: jobID, payload: { to: value.payload.to } };
  }
  if (value.kind === "transcript.user" || value.kind === "transcript.assistant") {
    if (!isRecord(value.payload) || typeof value.payload.text !== "string") return null;
    return { ...base, kind: value.kind, ...(jobID ? { job_id: jobID } : {}), payload: { text: value.payload.text } };
  }
  if (value.kind === "session.state") {
    const payload = parseSessionPayload(value.payload);
    return payload ? { ...base, kind: value.kind, payload } : null;
  }
  if (value.kind === "error.voice") {
    if (!isRecord(value.payload) || typeof value.payload.message !== "string" || !optionalString(value.payload, "play")) {
      return null;
    }
    return {
      ...base,
      kind: value.kind,
      payload: {
        message: value.payload.message,
        ...(typeof value.payload.play === "string" ? { play: value.payload.play } : {}),
      },
    };
  }
  if (ignoredKinds.has(value.kind)) {
    return { ...base, kind: value.kind as IgnoredEventKind, ...(jobID ? { job_id: jobID } : {}), payload: value.payload };
  }
  return null;
}

function sanitizeJob(value: unknown, jobID: string | undefined): Job | null {
  if (
    !jobID ||
    !isRecord(value) ||
    value.id !== jobID ||
    typeof value.display_name !== "string" ||
    typeof value.status !== "string" ||
    !jobStatuses.has(value.status) ||
    !optionalString(value, "question") ||
    !optionalString(value, "result_digest") ||
    !optionalString(value, "error") ||
    !optionalString(value, "updated_at")
  ) {
    return null;
  }
  let approvals: Approval[] | undefined;
  if (value.approvals !== undefined) {
    if (!Array.isArray(value.approvals)) return null;
    approvals = [];
    for (const approval of value.approvals) {
      const sanitized = sanitizeApproval(approval);
      if (!sanitized) return null;
      approvals.push(sanitized);
    }
  }
  const progress = value.progress === undefined ? undefined : sanitizeProgress(value.progress);
  if (value.progress !== undefined && !progress) return null;
  return {
    id: jobID,
    display_name: value.display_name,
    status: value.status as JobStatus,
    ...(typeof value.question === "string" ? { question: value.question } : {}),
    ...(approvals ? { approvals } : {}),
    ...(progress ? { progress } : {}),
    ...(typeof value.result_digest === "string" ? { result_digest: value.result_digest } : {}),
    ...(typeof value.error === "string" ? { error: value.error } : {}),
    ...(typeof value.updated_at === "string" ? { updated_at: value.updated_at } : {}),
  };
}

function sanitizeApproval(value: unknown): Approval | null {
  if (
    !isRecord(value) ||
    typeof value.id !== "string" ||
    typeof value.state !== "string" ||
    !approvalStates.has(value.state) ||
    typeof value.allow_permanent !== "boolean" ||
    !optionalString(value, "description") ||
    !optionalString(value, "command") ||
    !optionalString(value, "parked_at") ||
    !optionalString(value, "park_reason") ||
    (value.pattern_keys !== undefined &&
      (!Array.isArray(value.pattern_keys) || !value.pattern_keys.every((entry) => typeof entry === "string")))
  ) {
    return null;
  }
  return {
    id: value.id,
    state: value.state as Approval["state"],
    allow_permanent: value.allow_permanent,
    ...(typeof value.description === "string" ? { description: value.description } : {}),
    ...(typeof value.command === "string" ? { command: value.command } : {}),
    ...(Array.isArray(value.pattern_keys) ? { pattern_keys: [...value.pattern_keys] as string[] } : {}),
    ...(typeof value.parked_at === "string" ? { parked_at: value.parked_at } : {}),
    ...(typeof value.park_reason === "string" ? { park_reason: value.park_reason } : {}),
  };
}

function sanitizeProgress(value: unknown): Progress | null {
  if (
    !isRecord(value) ||
    typeof value.state !== "string" ||
    typeof value.updated_at !== "string" ||
    !optionalString(value, "tool") ||
    !optionalString(value, "label")
  ) {
    return null;
  }
  return {
    state: value.state,
    updated_at: value.updated_at,
    ...(typeof value.tool === "string" ? { tool: value.tool } : {}),
    ...(typeof value.label === "string" ? { label: value.label } : {}),
  };
}

function optionalString(value: Record<string, unknown>, key: string): boolean {
  return value[key] === undefined || typeof value[key] === "string";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
