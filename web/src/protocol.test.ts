import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  EVENT_KINDS,
  HANDLED_EVENT_KINDS,
  INTENTIONALLY_IGNORED_EVENT_KINDS,
  parseJobSnapshot,
  parseServerMessage,
  type Job,
  upsertJobHistory,
} from "./protocol";

const timestamp = "2026-07-16T00:00:00Z";

describe("control protocol", () => {
  it("rejects malformed envelopes, unknown kinds, and kind-specific payload errors", () => {
    expect(parseServerMessage("not json")).toBeNull();
    expect(parseServerMessage("null")).toBeNull();
    expect(parseServerMessage({ type: "hb" })).toEqual({ type: "hb" });
    expect(parseServerMessage(eventFrame({ seq: -1, kind: "usage", payload: null }))).toBeNull();
    expect(parseServerMessage(eventFrame({ seq: 1.5, kind: "usage", payload: null }))).toBeNull();
    expect(parseServerMessage(eventFrame({ seq: 1, kind: "job.invented", payload: null }))).toBeNull();
    expect(parseServerMessage(eventFrame({ seq: 1, kind: "transcript.user", payload: { text: 7 } }))).toBeNull();
    expect(parseServerMessage(eventFrame({ seq: 1, kind: "session.state", payload: { state: "UNKNOWN" } }))).toBeNull();
    expect(parseServerMessage(eventFrame({ seq: 1, kind: "job.done", job_id: "j1", payload: null }))).toBeNull();
  });

  it("rejects malformed job snapshots", () => {
    const event = {
      seq: 1,
      ts: timestamp,
      kind: "job.done",
      job_id: "job-1",
      payload: { id: "job-1", display_name: "One" },
    };
    expect(parseJobSnapshot(event)).toBeNull();
    expect(parseJobSnapshot({ ...event, payload: { ...event.payload, status: "invented" } })).toBeNull();
    expect(parseJobSnapshot({ ...event, payload: { ...event.payload, status: "done" } })).toEqual({
      id: "job-1",
      display_name: "One",
      status: "done",
    });
  });

  it("sanitizes nested job data and drops unknown authority-bearing fields", () => {
    const raw = eventFrame({
      seq: 4,
      kind: "job.needs_approval",
      job_id: "job-1",
      payload: {
        id: "job-1",
        display_name: "One",
        status: "needs_approval",
        ignored_job_field: "not copied",
        approvals: [
          {
            id: "approval-1",
            state: "pending",
            allow_permanent: false,
            command: "deploy",
            pattern_keys: ["deploy:prod"],
            decision_nonce: "must-not-reach-app-state",
            ignored_approval_field: true,
          },
        ],
        progress: {
          state: "running",
          updated_at: timestamp,
          tool: "terminal",
          ignored_progress_field: "not copied",
        },
      },
    });
    const parsed = parseServerMessage(raw);
    expect(parsed?.type).toBe("event");
    if (parsed?.type !== "event" || parsed.event.kind !== "job.needs_approval") return;
    expect(parsed.event.payload).toEqual({
      id: "job-1",
      display_name: "One",
      status: "needs_approval",
      approvals: [
        {
          id: "approval-1",
          state: "pending",
          allow_permanent: false,
          command: "deploy",
          pattern_keys: ["deploy:prod"],
        },
      ],
      progress: { state: "running", updated_at: timestamp, tool: "terminal" },
    });
  });

  it("accounts exactly once for every Go event kind", () => {
    const go = readFileSync(new URL("../../internal/events/bus.go", import.meta.url), "utf8");
    const goKinds = [...go.matchAll(/Kind\w+\s+=\s+"([^"]+)"/g)].map((match) => match[1]).sort();
    const classified = [...HANDLED_EVENT_KINDS, ...INTENTIONALLY_IGNORED_EVENT_KINDS];
    expect(EVENT_KINDS).toHaveLength(19);
    expect(new Set(EVENT_KINDS).size).toBe(EVENT_KINDS.length);
    expect(new Set(classified).size).toBe(classified.length);
    expect([...EVENT_KINDS].sort()).toEqual(goKinds);
    expect(classified.sort()).toEqual(goKinds);
  });

  it("bounds only truly terminal history and retains parked approvals", () => {
    let jobs = new Map<string, Job>();
    for (let i = 0; i < 4; i += 1) {
      jobs = upsertJobHistory(jobs, job(`done-${i}`, "done"), 2);
    }
    jobs = upsertJobHistory(jobs, job("parked", "parked_approval"), 2);
    jobs = upsertJobHistory(jobs, job("active", "needs_approval"), 2);
    jobs = upsertJobHistory(jobs, job("done-4", "done"), 2);

    expect([...jobs.keys()]).toEqual(["done-3", "parked", "active", "done-4"]);
    expect(jobs.get("parked")?.status).toBe("parked_approval");
    expect(jobs.get("active")?.status).toBe("needs_approval");
  });

  it("ignores stale snapshots even when publication order is newer", () => {
    const current = new Map<string, Job>([["j1", { ...job("j1", "cancelled"), updated_at: "2026-07-16T12:00:00Z" }]]);
    const next = upsertJobHistory(current, { ...job("j1", "running"), updated_at: "2026-07-16T11:59:59Z" });
    expect(next).toBe(current);
    expect(next.get("j1")?.status).toBe("cancelled");
  });
});

function eventFrame(event: Record<string, unknown>): Record<string, unknown> {
  return { type: "event", event: { ts: timestamp, ...event } };
}

function job(id: string, status: Job["status"]): Job {
  return { id, display_name: id, status };
}
