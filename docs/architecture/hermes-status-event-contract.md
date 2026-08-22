# Hermes → Thornhill operator-status contract

Status: design only; no runtime behavior is changed here.

## Role and scope

This is the operator-status projection profile for [Hermes–Thornhill session correlation](hermes-session-correlation.md). That document owns identity, authority, and producer-ordering vocabulary; if this document conflicts with it, the correlation contract wins.

Hermes structured Runs events are the per-work-item execution source. Thornhill owns the durable operator projection, attention outbox, reconciliation overlay, and summaries. Generic hook/cron intake is non-authoritative telemetry: it may be retained, but cannot create, advance, resolve, park, cancel, or terminalize work or approval state.

An operator-visible state or summary must be attributable to one durable work item and accepted event receipts—not a title, task text, command description, pattern text, timestamp proximity, mutable UI selection, or an uncorrelated hook.

## Required status envelope

This profile is a required typed `status` module of `thornhill.hermes-event.v1`, not an undeclared extension. The v1 schema release must adopt this module and its fixture corpus before a peer claims v1 support; until then, a status-bearing event is rejected rather than tunneled through generic evidence. Fields are schema-validated, size-bounded, and treated as untrusted until their complete correlation binding matches Thornhill's persisted registry.

```json
{
  "schema": "thornhill.hermes-event.v1",
  "event_id": "evt_...",
  "producer": {
    "producer_id": "hermes:configured-deployment-namespace",
    "component": "hermes",
    "instance_id": "configured-instance",
    "boot_id": "producer-boot"
  },
  "event_type": "approval.requested",
  "occurred_at": "2026-08-13T12:34:56Z",
  "causation_id": "evt_that_caused_this",
  "correlation": {
    "work_id": "thornhill-logical-job-ulid",
    "hermes_endpoint_id": "configured-endpoint-namespace",
    "session_id": "hermes-session-id",
    "session_generation": 3,
    "attempt_id": "thornhill-attempt-ulid",
    "start_idempotency_key": "thornhill-start-key",
    "run_id": "hermes-run-id",
    "approval_id": "hermes-live-approval-id",
    "approval_seq": 1
  },
  "ordering": {
    "source_seq": 17,
    "source_state_version": 7,
    "cursor": "opaque-replay-cursor"
  },
  "status": {
    "phase": "waiting_approval",
    "severity": "notice",
    "attention": "operator",
    "terminal": false,
    "reason_code": "approval_requested"
  },
  "payload": {
    "kind": "approval_request",
    "approval_snapshot_ref": "redacted-evidence-reference",
    "action_digest": "sha256:...",
    "pattern_set_digest": "sha256:...",
    "policy_version": "..."
  },
  "provenance": {
    "trace_ids": ["..."],
    "source_classes": ["tool_output"],
    "taint_bits": ["untrusted_content"]
  }
}
```

Every control event requires `schema`, `event_id`, `producer`, `event_type`, `occurred_at`, `causation_id` (nullable only for an admission root), `correlation`, `ordering`, `status`, `payload`, and `provenance`.

The base binding is `{work_id, hermes_endpoint_id, session_id, session_generation, attempt_id, start_idempotency_key, run_id}`. `run.accepted` must echo the original start key exactly. Every approval event additionally carries the Hermes-issued `{approval_id, approval_seq}`. `approval.decision_applied` additionally carries the Thornhill-issued `decision_id`; it proves matching acknowledgement, not a credential. A voice call/turn ID is a disposable Thornhill presentation fence and is never execution or approval authority.

| Event producer and class | Required ordering and conditional identity |
| --- | --- |
| Hermes run/control event | `producer_id`, `source_seq`, and `cursor`; reducer-changing events also require `source_state_version`. `run.accepted` requires the exact original `start_idempotency_key`. |
| Hermes approval event | all run/control fields plus exact `{approval_id, approval_seq}`. `approval.requested` requires `approval_snapshot_ref`, action/pattern digests, and policy version; `approval.heartbeat` requires `payload.liveness_interval_ms`; `approval.decision_applied` requires `correlation.decision_id` and a broker-canonical `payload.decision_choice` equal to the choice persisted for that `decision_id`. |
| Hermes terminal or historical event | full base binding and applicable Hermes ordering; approval/voice fields may be omitted only when they do not apply. `run.terminal` requires `payload.outcome` (`completed`, `failed`, or `cancelled`) and matching final source version; `run.heartbeat` requires `payload.liveness_interval_ms`. |
| Thornhill local decision or parking receipt | `producer.component="thornhill"`, Thornhill event-ID namespace, the complete binding, and next per-work `ledger_version`; it may quote the causal Hermes cursor but never substitutes a local version for a Hermes source version. |

Hermes-origin events require a durable `producer_id`, increasing `source_seq` per `{producer_id, hermes_endpoint_id, attempt_id}`, and `source_state_version` for reducer-changing events. `cursor` is the opaque replay checkpoint. Thornhill-origin local decision/parking receipts instead carry a per-work `ledger_version`; they may quote a causal source cursor for diagnostics, but reducers never compare a local ledger version with a Hermes source version. `event_id` is immutable across retry/replay. Event time is observability data, never a conflict resolver.

For this profile, `producer.component` is `hermes` or `thornhill`; `status.phase` is one of `starting`, `running`, `waiting_input`, `waiting_approval`, `parked_approval`, `done`, `failed`, or `cancelled`; and `attention` is `none`, `operator`, or `reconciliation`. `payload.kind` is a closed event-specific enum matching the event type (for example `approval_request`, `decision_acknowledgement`, `run_terminal`, `run_snapshot`, `run_heartbeat`, `approval_heartbeat`, `tool_progress`, or `output_delta`). Unknown values are quarantined. `severity` uses the closed levels below.

| Event family | Closed `reason_code` values |
| --- | --- |
| start/run state | `accepted`, `active`, `input_requested`, `approval_waiting` |
| approval | `approval_requested`, `decision_applied`, `invalidated`, `expired`, `parked`, `indeterminate` |
| terminal | `completed`, `failed`, `cancelled`, `approval_parked` (only with terminal phase `cancelled`, and only for the released attempt) |
| mechanical/presentation | `heartbeat`, `progress`, `output_delta` |
| reconciliation overlay | `source_gap`, `stale_heartbeat`, `snapshot_reconciled`, `start_indeterminate`, `correlation_fault`, `terminal_conflict` |

A reason code is valid only with its event family and compatible phase/terminal bit. It is an operator explanation, not a substitute for the typed outcome or an authority instruction.

Payload fields are typed references/digests or bounded redacted evidence. Raw commands, descriptions, arbitrary pattern text, prompts, decision nonces, bearer tokens, and secrets are forbidden. Evidence remains data even if it contains an instruction-shaped string; only typed enums, identifiers, and validated versions can affect a reducer.

Unknown schemas/types, malformed or oversized identifiers, unknown producers, or undeclared fields are quarantined with no state mutation. `producer_id`, endpoint, instance, and boot identifiers are namespace fences, not peer authentication; a remote or independently administered Hermes also requires authenticated transport and separately protected peer identity before any event is accepted.

## Allowed transitions

Thornhill's projected work states are `queued`, `starting`, `running`, `needs_input`, `needs_approval`, `parked_approval`, `done`, `failed`, and `cancelled`. The integrity/reconciliation overlay is separate from this base state and takes operator precedence whenever present.

| Event and precondition | Allowed projection | Operator consequence |
| --- | --- | --- |
| locally admitted start, before Hermes I/O | `queued → starting` | info only |
| `run.accepted`, exact new attempt binding and original start key | `starting → running` | info only |
| `run.state(phase=running)`, exact current attempt | confirms an already-running projection; from `needs_approval`, defer until the exact decision acknowledgement/replay proves the transition | info only |
| `run.state(phase=waiting_input)`, exact active attempt | `running → needs_input` | notice + durable attention |
| `run.state(phase=waiting_approval)`, no exact approval yet | defer pending the exact `approval.requested` or replay/snapshot; do not consume a projected approval transition | no actionable approval |
| `approval.requested`, complete exact binding and FIFO sequence | `running → needs_approval` | notice + durable attention |
| `approval.decision_applied`, matching `{approval_id, approval_seq, decision_id}` | `needs_approval → running`; finalizes the explicit decision record | info; it proves the decision reached the exact wait, not that a later tool side effect succeeded |
| Thornhill-local indeterminate decision after a possible authority crossing | `needs_approval → failed` with typed cause; stop and reconcile | error + attention; never resend the decision |
| `approval.invalidated` or `approval.expired`, exact pending approval | `needs_approval → failed` with typed cause | error + attention; no stale approval remains actionable |
| Thornhill-local `approval.parked`, committed with no decision | `needs_approval → parked_approval` | notice + durable attention |
| explicit answer or explicit resume, locally admitted | `needs_input` or `parked_approval → queued` | creates a fresh attempt; never reuses run/approval authority |
| `run.terminal(outcome=completed)` from current active attempt | active base state → `done` | terminal notice + attention |
| `run.terminal(outcome=failed)` from current active attempt | active base state → `failed` | error + attention |
| `run.terminal(outcome=cancelled)` from current active attempt | active base state → `cancelled` | notice/error as reason requires |
| terminal release with `reason_code=approval_parked` | retain `parked_approval`; record old-attempt resource release | terminal phase/outcome applies only to that old attempt; it is neither a decision nor a logical-work cancellation |

Except for the explicit `approval_parked` terminal-release exception above, `done`, `failed`, and `cancelled` are terminal for the logical work item. That exception records a cancelled/released old attempt without cancelling the logical work item. Here, “current active attempt” means the exact nonterminal `starting`, `running`, `needs_input`, or `needs_approval` attempt; `parked_approval` is nonterminal for the work item but terminal for that attempt. Explicit resume creates a new `attempt_id`, `start_idempotency_key`, `run_id`, and approval identity. Every transition not listed in the table is rejected or retained solely as stale/fault evidence; no incoming event performs an implicit reverse transition or revives a terminal attempt.

The Hermes vocabulary is `run.accepted`, `run.state`, `run.heartbeat`, `tool.progress`, `output.delta`, `approval.requested`, `approval.heartbeat`, `approval.decision_applied`, `approval.invalidated`, `approval.expired`, `run.terminal`, and `run.snapshot`. Thornhill additionally records local `approval.parked` and decision receipts after their durable transitions; neither permits Hermes to infer a local state change. `tool.progress` and `output.delta` are presentation-only and cannot affect work or approval authority.

## Severity, heartbeat, and terminal rules

| Severity | Meaning and Thornhill minimum |
| --- | --- |
| `info` | expected admission, activity, progress, or liveness; board/history only |
| `notice` | meaningful input, approval, parking, or successful completion; durable attention |
| `warning` | retry, stream gap, stale heartbeat, or reconciliation pending; report uncertainty, not invented state |
| `error` | definitive failure, invalidation/expiry, or indeterminate authority outcome; durable attention |
| `critical` | schema/identity violation, contradictory terminal evidence, or uncorrelatable approval; quarantine plus durable reconciliation attention |

Hermes may suggest severity and attention, but Thornhill derives the durable minimum from the typed event, reason code, and local rate policy. Tainted prose cannot lower a mismatch, promote ordinary progress into an authoritative alert, or create an attention obligation.

A Hermes heartbeat carries the exact binding, `source_seq`, source cursor, and negotiated liveness interval; an approval heartbeat additionally carries the exact approval identity. A heartbeat refreshes only the liveness watermark. It cannot create work, extend or decide approval, clear a parked state, create attention, or change a summary. A missed heartbeat creates a local `warning`/reconciliation overlay; it does not invent failure or terminal evidence.

A Hermes terminal event sets `status.terminal=true`, carries one typed outcome and final `source_state_version`, and closes exactly one `{hermes_endpoint_id, attempt_id, run_id}`. Its phase must exactly match its outcome (`completed → done`, `failed → failed`, `cancelled → cancelled`); the `approval_parked` exception is a cancelled/released old attempt, not a logical-work cancellation. Nonterminal events set `terminal=false`. Later lower/equal source-version progress, heartbeat, or state events are auditable no-ops. A different terminal outcome for the same attempt is `critical`; arrival order never decides the winner.

## Thornhill reconciliation and delivery

Thornhill treats the source stream as at-least-once and untrusted at its process boundary.

1. Parse, bound, schema-validate, and validate the producer. Compute a SHA-256 over the schema-defined canonical envelope; a producer-declared digest is evidence only.
2. Look up the durable inbox key `(producer_id, event_id)`. Same key/hash with an applied/duplicate disposition is an auditable no-op; same key/different hash is a critical protocol fault. A duplicate of a deferred receipt may re-trigger only its stored replay/reconciliation obligation after a predecessor arrives; it never creates a second reducer effect. A concurrent insert is resolved by re-reading that same key under the final transaction.
3. For a first `run.accepted`, match the pre-created `starting` registry binding through its exact start idempotency key, then atomically bind the previously unbound `run_id` only if every echoed field matches. For every other first-seen event, match the complete bound correlation before reading evidence.
4. For Hermes, require the next contiguous `source_seq` and, for state mutation, the next `source_state_version`; the only exception is a validated gap-closing `run.snapshot` under step 6. For local Thornhill receipts, apply only the next durable `ledger_version`. A gap, missing predecessor, or old-attempt delivery is deferred/stale evidence and starts replay; it does not alter operator state or make an approval actionable.
5. In one durable transaction, insert the accepted receipt, bind a first accepted run ID if applicable, apply the work/approval reducer, append redacted evidence, create required attention/outbox rows, and assign the next local `ledger_version`. Acknowledge Hermes only after that transaction commits.
6. On reconnect or restart, replay after the last contiguous source cursor. A `run.snapshot` may repair state only when its complete binding matches the registry, its source version/cursor closes an observed gap, and it contains an authoritative typed state consistent with the current approval lifecycle; it may resolve a deferred `waiting_approval` state only with its matching approval identity. A snapshot may make this one gap-closing jump but cannot cross work items, reopen a terminal attempt, or overwrite a locally recorded indeterminate decision without the same `decision_id`.
7. Before a run start, Thornhill persists the new `attempt_id` and start idempotency key. An ambiguous start is reconciled only through that same key plus the expected pre-created binding—via the idempotency lookup or matching `run.accepted`—never by accepting an arbitrary run event or retrying a fresh run.

| Fault | Required disposition |
| --- | --- |
| acknowledgement lost after commit | Hermes retries; receipt deduplication prevents a second transition or attention item |
| receipt/reducer/outbox transaction fails | do not acknowledge; retry the immutable event |
| out-of-order or missing delivery | defer, replay, then snapshot if needed; never order by wall clock |
| stale event from an old session generation or attempt | retain as stale audit evidence; cannot mutate current work |
| missing/wrong work, endpoint, session generation, attempt, start key, run, approval, sequence, or decision ID | no approval row/modal/grant; record `correlation_fault` and reconcile or park known work |
| approval decision acknowledgement does not match a locally `deciding` record, or appears after parking/invalidation/expiry/indeterminate | retain as fault evidence; no running transition or authority retry; reconcile the exact attempt |
| more than one outstanding approval for a run, or unreconciled FIFO sequence | no actionable modal or broad resolve-all action; quarantine and stop/reconcile the exact run; only a documented per-item exact denial may be automated |
| Hermes/Thornhill ordering fields mixed or compared | reject as a protocol fault; no cross-producer version ordering |
| terminal/progress/approval event after a terminal or parked old attempt | retain as stale evidence unless it is the documented `approval_parked` release; never revive, overwrite, or attach fresh authority |
| different terminal payload for one event ID/attempt | critical integrity overlay; no last-arrival-wins state |

Approval authority remains exclusively on the Thornhill-to-Hermes broker path: an atomic local claim writes `decision_id`, Hermes applies one exact decision, and `approval.decision_applied` confirms it. Status events can report that outcome but can never call the approval endpoint, mint a decision, or turn silence, retries, parking, or summaries into consent.

## Traceable summaries and taint handling

Operator state is `durable work projection + integrity/reconciliation overlay`, not the newest status prose. Every voice, push, board, or rolling-summary statement has this basis:

```text
{work_id, hermes_endpoint_id, session_id, session_generation, attempt_id, run_id,
 approval_id, approval_seq, decision_id when applicable, producer_id,
 accepted_source_state_version_or_cursor_range, accepted_event_ids,
 thornhill_ledger_version, reconciliation_state}
```

A per-work summary may use only accepted receipts from the same binding and local ledger version. A multi-work digest retains one basis per work item. Deferred, rejected, stale, or gap-affected receipts may say only that reconciliation is pending; they cannot claim completion, failure, cancellation, or an approval outcome.

All status payloads remain tainted evidence, including validly transported Hermes output. Thornhill quotes/redacts it before UI, voice, logs, retained corpus, or model summarization. It cannot become a prompt instruction, tool request, policy rule, correlation key, or approval scope.

## Minimal telemetry

Retain a redacted receipt containing event ID/hash and producer ID, the complete correlation tuple with the start key represented by a protected reference/digest, source sequence/version/cursor or local ledger version, receipt time, disposition (`applied`, `duplicate`, `deferred`, `stale`, `rejected`, `reconciled`), projected ledger version, and summary/attention basis. Keep a raw start idempotency key only in the protected correlation registry if implementation requires it; receipts and telemetry use a restricted reference or digest. Terminal and authority receipts join the durable decision corpus; progress/heartbeat telemetry may follow the existing mechanical-event retention policy.

At minimum emit counters for received/applied/duplicate/rejected events, schema failures, sequence gaps, snapshot/replay outcomes, correlation faults, active approvals missing complete binding, uncorrelatable approvals, terminal conflicts, and attention retries; gauges for heartbeat age, unresolved gaps, pending/parked/indeterminate approvals, and oldest reconciliation obligation; and histograms for ingest lag and reconciliation duration. No raw prompt, command, description, pattern contents, start key, nonce, token, or secret enters telemetry.

## Conformance matrix

| ID | Fixture | Required oracle |
| --- | --- | --- |
| SE-01 | Normal start admission → exact `run.accepted` → active/progress → completed attempt | one work projection, terminal attention, and summary basis citing accepted receipts and a local ledger version |
| SE-02 | Same Hermes event replayed after lost acknowledgement | one inbox receipt/reducer effect/attention item |
| SE-03 | Same producer/event ID with a different canonical payload | critical quarantine and no state mutation |
| SE-04 | Reordered progress/approval/terminal or a source sequence/state-version gap | no speculative state or actionable approval until replay/snapshot repairs it |
| SE-05 | Old-attempt completion after explicit resume | cannot overwrite the fresh attempt or logical-work state |
| SE-06 | Exact approval request plus matching decision acknowledgement | exactly one pending row; acknowledgement returns the exact work to `running`, but does not claim a later tool side effect succeeded |
| SE-07 | Deny or safer-alternative acknowledgement | the explicit decision is retained and execution may resume; no terminal success/failure summary is emitted without a correlated terminal event |
| SE-08 | Missing/wrong session generation, start key, run, approval sequence, or decision ID | no modal, authority call, or summary claim; traceable correlation fault |
| SE-09 | Parked approval, delayed old-run terminal, then explicit resume | remains parked; stop is release evidence; fresh attempt/approval required |
| SE-10 | Decision transport loss after possible Hermes receipt | indeterminate, stopped/reconciled, never automatically resent |
| SE-11 | Heartbeat replay/loss/late post-terminal heartbeat | liveness/reconciliation only; no authority or state resurrection |
| SE-12 | Receipt/job/outbox partial transaction failure | no acknowledgement before atomic commit; safe retry; no duplicate attention |
| SE-13 | Command-like or role-like tainted evidence | typed fields alone drive state; no control action or prompt injection |
| SE-14 | Two simultaneous jobs with similar text | receipts, approvals, attention, and summary bases never cross bindings |
| SE-15 | Hermes source versions and Thornhill local ledger versions interleaved | reducers never compare cross-producer versions; each ordering rule remains scoped to its producer |
| SE-16 | Ambiguous start request before/after Hermes acceptance, including a mismatched echoed binding | only the original start key plus exact pre-created binding binds one attempt/run; otherwise `start_indeterminate`; no arbitrary acceptance or replacement run |
| SE-17 | Unknown status module/version, unknown enum, or incompatible reason/phase/terminal combination | quarantine with no reducer mutation, approval presentation, or summary claim |
| SE-18 | Terminal whose phase does not match its typed outcome, or a conflicting later terminal | critical integrity overlay; no last-arrival-wins terminal state |
| SE-19 | `waiting_approval` state arrives before its exact approval record | deferred pending replay/snapshot; no source-state shortcut and no actionable approval |
| SE-20 | Snapshot tries to jump a gap with a mismatched binding, inconsistent approval lifecycle, terminal resurrection, or different indeterminate decision ID | rejected/quarantined; only an exact authoritative gap-closing snapshot can advance state |
| SE-21 | Late/mismatched decision acknowledgement or approval event after parking, invalidation, expiry, or indeterminate state | fault evidence only; no running transition, renewed authority, or retry |
| SE-22 | Duplicate of an unresolved deferred receipt, followed by its predecessor/replay | duplicate can re-trigger reconciliation but yields one eventual reducer effect and attention item |
| SE-23 | Second outstanding approval or unreconciled FIFO sequence in one run | no modal/resolve-all action; exact-run quarantine, stop/reconcile, and no cross-approval decision |

Use a deterministic fake Hermes stream, fake authority endpoint, barriers, and fake clock. Do not rely on live models or timing sleeps as the oracle. The implementation suite must tag every normative transition, envelope rule, reconciliation rule, and telemetry/privacy requirement in this document with one or more case IDs, generate a requirement-to-case report in CI, and reject release if any required rule has no passing case. Deliberate deviations must be documented with a rationale and an expected-failure case; silent skips are not conformance.

## Why ambiguity is prevented

An approval cannot become actionable without its Hermes-issued `approval_id`/sequence plus the exact work, endpoint, session generation, attempt, start key, and run binding; a decision acknowledgement also needs the one persisted `decision_id`. A malformed or delayed event therefore becomes a visible correlation fault, not an approval prompt or an implicit grant.

A summary cannot be mismatched because it is generated from accepted receipt IDs, source cursor/version, and one local ledger version for one explicit binding. Similar job names, commands, timestamps, or model text cannot attach another job's progress, terminal result, or approval outcome to that summary.

## Implementation gate

The session-correlation work supplies the registry, idempotent start binding, producer-scoped inbox/reducer/outbox, and replay/snapshot semantics. The taint-hardening work supplies bounded provenance/evidence handling and prevents evidence from becoming authority. A future implementation should ship this profile only after those foundations exist, with the conformance matrix enforced in CI, version negotiation required, and generic hook intake kept outside the state-control path.
