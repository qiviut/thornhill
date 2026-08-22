# Hermes–Thornhill session correlation contract

Status: design-only proposal, 2026-08-13

## Outcome

Make every Thornhill job history entry, Hermes execution attempt, operator approval, and durable decision traceable to the same explicit identity chain. The contract must tolerate at-least-once delivery, retries, reconnects, stale browser/voice callbacks, partial persistence, and restarts without granting authority, duplicating an approval, or attaching evidence to the wrong work.

This is an authority and history contract, not a transcript-mirroring scheme. It complements the taint work: provenance and taint metadata travel with correlated evidence, but no content, model output, status description, command-looking string, title, or pattern text is an identity or authority signal.

## Why this is needed

Today Thornhill persists a job's Hermes session and run IDs, but its event bus has only a local sequence and job ID. The bridge's structured approval event does not carry a Hermes approval identity; Thornhill currently creates its own approval ID and decision nonce after receipt. That leaves no shared durable key for an approval request, invalidation, decision acknowledgement, or replay.

The result is an avoidable ambiguity boundary: a duplicate, delayed, restored, or cross-run approval event can look like a new actionable request even when its Hermes wait is gone. The design below removes that ambiguity rather than trying to infer linkage from timestamps, titles, command text, descriptions, or event order observed by one transport.

## Scope and ownership

Thornhill owns the durable operator-facing work ledger, job lifecycle, approval presentation, decision-attempt record, and reconciliation queue. Hermes owns its session, run execution, live approval wait, and the authoritative acknowledgement that a decision reached that wait.

Neither system may infer an authority outcome from the other system's UI state, elapsed time, free-form text, or a missing callback. Correlation identifiers route and fence state; they are not credentials, consent, or proof that a person owns a browser.

For v1, one active session binding belongs to one Thornhill work item. A future deliberate shared-session feature needs its own `shared_session_group_id` and authorization/replay design; accidental many-to-one attachment is rejected.

## Correlation vocabulary

Do not overload one vague `correlation_id`. The following opaque IDs have distinct scopes and must be carried as one `correlation` object.

| Field | Issuer and lifetime | Rule |
| --- | --- | --- |
| `work_id` | Thornhill; one durable logical job | Root history/board identity. Never derived from a display name. |
| `hermes_endpoint_id` | deployment configuration; stable namespace | Separates same-shaped IDs from different Hermes deployments. It is not cryptographic peer authentication. |
| `session_id` | Hermes session, bound by Thornhill | Stable conversational runtime identity; stored separately even where legacy mode happens to equal `work_id`. |
| `session_generation` | Thornhill; increment only on an intentional session rebind | Makes a stale session binding fail closed. |
| `attempt_id` | Thornhill ULID, persisted before `POST /v1/runs` | One execution attempt and the primary cross-system fence. A retry or resume gets a new value. |
| `start_idempotency_key` | Thornhill; one attempt-start admission | Sent only by Thornhill's trusted bridge, never derived from task content; resolves an ambiguous start without creating a replacement run. |
| `run_id` | Hermes; bound once start is definitively acknowledged | One live Hermes run. Unique only with `hermes_endpoint_id`; never guessed after an ambiguous start. |
| `approval_id` | Hermes; one live approval wait | Required on every approval request, heartbeat, invalidation, and decision acknowledgement. Never minted by Thornhill as a substitute. |
| `approval_seq` | Hermes; monotonic within a run | Preserves FIFO identity and detects an unsupported concurrent approval. |
| `decision_id` | Thornhill; one operator decision attempt | Persisted before crossing the Hermes authority boundary and echoed by Hermes only in the matching acknowledgement. |
| `decision_nonce` | Thornhill; one-use secret/control token | Bound to the current durable row and exact decision attempt. Never included in the event log, bus, UI replay, metrics, or summaries. |
| `event_id` | event producer; immutable across replay | Idempotency key for exactly one semantic event, unique within `producer_id`. |
| `producer_id` | durable producer namespace | Stable producer identity for deduplication; it survives a process restart, unlike `boot_id`. |
| `source_seq` and `source_state_version` | source producer, per `{producer, endpoint, attempt}` | `source_seq` detects event gaps; `source_state_version` orders source-authoritative state transitions. They are never compared across producers. |
| `ledger_version` | Thornhill transaction, per work item | Monotonic local projection version assigned with the inbox/reducer/outbox commit; it orders Thornhill's canonical history and summaries. |
| `voice_call_id` and `voice_turn_id` | Thornhill Desk, optional | Bind a disposable voice delivery to a job/attempt. They are never approval authority. |

The required binding is:

```text
work_id
  -> (hermes_endpoint_id, session_id, session_generation)
  -> attempt_id
  -> run_id
  -> approval_id / approval_seq
  -> decision_id
```

A terminal or historical event may omit approval and voice fields, but it may never omit `work_id`, endpoint, session binding, `attempt_id`, `run_id`, `event_id`, and the applicable ordering fields. An event that cannot meet that rule is evidence of an integration fault, not a candidate for best-effort correlation.

## State and session-boundary semantics

A durable work item can outlive a browser, a Desk call, an SSE connection, or a River worker. A browser/voice activation is a disposable view of durable state, not a replacement session or authority owner.

1. Thornhill creates and durably records `attempt_id` plus a start idempotency key before requesting a Hermes run.
2. Hermes accepts the supplied correlation object and `start_idempotency_key`, binds them to the newly created `run_id`, and returns/streams the complete echoed binding plus start key in `run.accepted`.
3. Thornhill binds `run_id` only after the echoed context and start key exactly match the starting record. An ambiguous start result is `start_indeterminate`; it is not retried as a new run. Reconciliation may use a Hermes idempotency lookup for the same start key, but not a fresh start.
4. A client activating a job captures `{work_id, session_id, session_generation, view_epoch}`. It checks that context after every await and immediately before render, speech, control enablement, cursor update, or cleanup. A stale callback may clean up its captured resource but cannot mutate the newer view.
5. A Desk call additionally requires its exact `voice_call_id` and `voice_turn_id` for spoken output. These values may be absent from durable execution events and cannot authorize a decision.
6. A run resume or reissue creates a new `attempt_id`, `run_id`, and approval identity. It preserves the durable history and prior evidence as quoted untrusted data, but never reuses old approval authority.

`parked_approval` is a resource state, not an allow, deny, or expiry decision. Thornhill commits the parked state before releasing the stream/worker, keeps redacted evidence, and invalidates the old approval identity for action. A later resume verifies current state and requests a fresh approval if still needed.

## Wire envelope

All cross-system control events use a versioned, schema-validated envelope. Hermes emits run, approval, and snapshot events; Thornhill emits its locally durable decision/parking events with the same correlation object and `producer.component="thornhill"`. Fields not declared by the schema are retained only as bounded, quarantined evidence and cannot influence a reducer.

```json
{
  "schema": "thornhill.hermes-event.v1",
  "event_id": "evt_…",
  "producer": {
    "producer_id": "hermes:deployment-or-instance-namespace",
    "component": "hermes",
    "instance_id": "…",
    "boot_id": "…"
  },
  "event_type": "approval.requested",
  "occurred_at": "2026-08-13T00:00:00Z",
  "causation_id": "evt_…",
  "correlation": {
    "work_id": "…",
    "hermes_endpoint_id": "…",
    "session_id": "…",
    "session_generation": 1,
    "attempt_id": "…",
    "start_idempotency_key": "…",
    "run_id": "…",
    "approval_id": "…",
    "approval_seq": 1
  },
  "ordering": {
    "source_seq": 42,
    "source_state_version": 7
  },
  "payload": {
    "kind": "typed-and-bounded-payload",
    "approval_snapshot_ref": "…",
    "action_digest": "…",
    "pattern_set_digest": "…",
    "policy_version": "…"
  },
  "provenance": {
    "trace_ids": ["…"],
    "source_classes": ["tool_output"],
    "taint_bits": ["untrusted_content"]
  }
}
```

`payload` contains typed fields only. Commands, descriptions, pattern labels/keys, and model-authored content are untrusted evidence even when they arrive inside a schema-valid event; they cannot select identity, construct a control object, or create authority. Approval display material is a redacted, escaped snapshot referenced by a stable record and bound by `action_digest`/`pattern_set_digest`; it remains data even when it contains command syntax. Those digests bind the exact rendered evidence/policy snapshot after deterministic canonicalization; they are not an origin claim, a signature, or a substitute for schema, ownership, taint, and policy validation. The deterministic broker re-normalizes and validates typed action/pattern fields before each decision. Raw source content, secrets, decision nonces, and model-authored instructions do not enter metrics, control messages, or implicit system context.

The envelope has producer-specific ordering: Hermes-origin events require `source_seq` and, when reducer-changing, `source_state_version`; Thornhill-origin outbox events require `ledger_version`. A Thornhill event may quote the causal Hermes cursor for diagnostics, but reducers never compare a local ledger version with a Hermes source version.

A remote or independently administered Hermes needs an authenticated transport and separately protected peer identity. `hermes_endpoint_id`, `instance_id`, and `boot_id` prevent accidental namespace mixing but do not claim to solve that separate trust problem.

## Required event types and transitions

The protocol distinguishes durable control events from best-effort presentation events.

| Event | Required effect |
| --- | --- |
| `run.accepted` | Binds the pre-created attempt to the echoed `run_id`; only matching data may leave `starting`. |
| `run.state` | Applies a typed, versioned non-terminal state transition. |
| `run.heartbeat` | Proves a live stream/wait for the exact attempt; it never refreshes or decides authority by itself. |
| `tool.progress` / `output.delta` | Presentation-only. Loss is tolerable and cannot change approval/run authority. |
| `approval.requested` | Creates exactly one durable pending approval only after all correlation fields, digests, provenance summary, and FIFO position validate. |
| `approval.heartbeat` | Refreshes observability for the exact pending wait; it does not extend a decision token or create a second approval. |
| `approval.decision_applied` | Confirms the exact `{approval_id, approval_seq, decision_id}` reached Hermes. It is not proof that a later tool side effect succeeded. |
| `approval.invalidated` / `approval.expired` | Closes only the exact pending approval and its linked run state with a typed reason. |
| `approval.parked` | Records resource reclamation with no decision. Old approval authority becomes stale. |
| `run.terminal` | Applies one terminal outcome for the exact attempt, including terminal reason and final `source_state_version`. |
| `run.snapshot` | Reconciliation-only authoritative state at a version/cursor; used to close a detected gap. |

For Hermes-origin reducer-changing events, `source_state_version` increases strictly by one within the attempt. Thornhill assigns its own `ledger_version` while applying the event and emitting its local outbox record; it never treats a Thornhill decision/parking version as a Hermes source version. A terminal state is monotonic: lower-version progress, waiting, or completion events cannot revive, overwrite, or attach a new approval to it.

Approval lifecycle is:

```text
absent -> pending -> deciding -> decision_applied
                  \-> parked | invalidated | expired
                  \-> indeterminate
```

`pending -> deciding` is Thornhill's atomic compare-and-set and writes `decision_id` before Hermes I/O. `deciding -> decision_applied` requires the matching Hermes acknowledgement. A transport loss or exception after the authority boundary is `indeterminate`, stops/reconciles the run, and is never retried automatically. `parked`, `invalidated`, `expired`, `indeterminate`, and `decision_applied` are terminal for that `approval_id`; a subsequent attempt requires a new approval.

If Hermes emits more than one outstanding approval for a run, or an approval whose FIFO sequence cannot be reconciled, Thornhill does not create an actionable modal or use a broad resolve-all shortcut. It quarantines the fault and stops/reconciles the run; only a documented per-item, exactly correlated denial is eligible for a safe automatic deny.

## Ordering, durability, and delivery

The contract provides per-attempt causal ordering, not a false global order across jobs or transports.

- Hermes gives each event a stable `event_id` and increasing `source_seq`; replays preserve both. The Thornhill bridge advances its receipt cursor for every schema-valid Hermes event before it elects not to forward a presentation-only event to a browser. Thus presentation delivery may be lossy, but ingestion of the authoritative source stream is not. Thornhill-minted control events use the same envelope and a Thornhill event ID namespace; they are ordered by their durable transaction/`ledger_version`, not by a shared wall clock.
- Hermes state-changing events also carry `source_state_version`. Thornhill applies source version `n+1` only after it has applied `n`; a higher version creates a gap rather than a speculative state transition. A matching authoritative `run.snapshot` may atomically advance the source cursor/version and local projection after full-binding validation; it is the only allowed jump. Its resulting durable projection receives the next local `ledger_version`.
- Thornhill inserts every accepted control event into a deduplicating inbox and applies the job/approval reducer plus its outbound event in one PostgreSQL transaction. The unique key is `(producer_id, event_id)`; `hermes_endpoint_id` remains part of the correlation binding and cross-check.
- A duplicate is an auditable no-op. A lower/equal `source_state_version` cannot mutate Hermes-derived state, and a lower/equal `ledger_version` cannot overwrite Thornhill's projection. A payload-only change for the same producer-scoped event ID is a protocol violation, not an update.
- On a source-sequence or source-state-version gap, Thornhill persists the observation, requests replay after the last contiguous cursor, and then requests a matching `run.snapshot` if needed. It does not make an approval actionable while the gap is unresolved.
- Presentation events may be dropped or reordered. They render only when their client view epoch and exact attempt/run remain current; they never update durable authority state.
- Authoritative control events use a durable Hermes outbox/replay source and must not be silently discarded by a bounded in-process fanout queue. A slow browser may miss deltas, but it can always rehydrate from the Thornhill durable ledger.

## Durable correlation ledger

The protocol's authoritative history is a small additive PostgreSQL ledger; the existing operator board remains a projection, not the only evidence source.

| Record | Required fields and constraints |
| --- | --- |
| `work_correlation` | `work_id` primary key; endpoint, session ID/generation, current state, and monotonic `ledger_version`. |
| `run_attempt` | `attempt_id` primary key; `work_id` foreign key; `(hermes_endpoint_id, start_idempotency_key)` unique; `(hermes_endpoint_id, run_id)` unique once bound; last contiguous source cursor/version and reconciliation state. |
| `event_inbox` | `(producer_id, event_id)` primary key; canonical envelope hash, complete binding, source cursor/version, receipt time, disposition, and applied `ledger_version`. A same-key/different-hash event is a protocol fault. |
| `approval` | `(hermes_endpoint_id, approval_id)` unique; `attempt_id` foreign key; `(attempt_id, approval_seq)` unique; lifecycle state, redacted evidence reference, digests, provenance summary, and current `decision_id`. |
| `approval_decision` | `decision_id` primary key; exact approval/attempt/run binding, choice, protected one-use control nonce, decision state, acknowledgement event ID, and typed failure reason. The nonce never projects into the event ledger or UI. |
| `correlation_outbox` / `reconciliation_obligation` | `ledger_version`, typed state event or fault, retry cursor/backoff, and terminal resolution. These make publication/replay and repair inspectable after a crash. |

The inbox, projection reducer, approval/decision mutation, and Thornhill outbox entry commit in one transaction. Bounded redacted evidence stays in its existing quarantined store; no raw command, transcript, or secret is duplicated merely to make correlation work.

## Approval decision protocol

1. Hermes persists its wait and emits `approval.requested` with the shared approval identity, exact run binding, action/pattern digests, typed risk/category fields, and provenance summary.
2. Thornhill validates the full binding against the active attempt, inserts the event and pending row atomically, and only then displays/speaks it. Missing, malformed, unknown, stale, or mismatched binding creates a correlation-fault record, never an actionable approval.
3. Thornhill atomically claims `pending -> deciding`, creates `decision_id`, and retains the private one-use nonce. A second decision loses without making a Hermes call.
4. Thornhill sends the exact approval, run, attempt, decision, digest, and choice to a per-item Hermes approval endpoint. Hermes rejects any mismatch, stale generation, or missing current wait.
5. Hermes persists/applies the decision once and emits `approval.decision_applied` carrying the same `decision_id`; Thornhill then finalizes the local decision. A deny or safer-alternative choice remains an explicit decision record, not an inferred failed approval.
6. If either side cannot prove the outcome after the authority boundary, Thornhill marks the decision `indeterminate`, stops/reconciles the run, and requires a fresh run/approval rather than generating approval churn through retries.

No approval can be correlated by a command preview, description, model summary, title, timestamp proximity, current browser tab, or a mutable `currentJob` value.

## Reconciliation strategy

A startup and periodic reconciler compares the Thornhill correlation registry with Hermes run snapshots and event cursors.

- `starting`: use only the original `start_idempotency_key` and full expected correlation object to discover whether Hermes accepted the attempt. If not provable, mark `start_indeterminate`; never create a replacement run automatically.
- `active` with a gap or lost stream: replay from the last contiguous cursor and compare the matching `run.snapshot` before reducing further state.
- `pending`: confirm the same run/attempt/approval wait. Under the existing resource policy, a restart may park it without deciding, then stop/release execution resources only after durable parking commits.
- `deciding`: mark `indeterminate` unless the matching decision acknowledgement is durable. Do not replay the authority call.
- `parked`: preserve evidence, retry only bounded upstream cleanup, and reissue fresh authority on explicit resume.
- `terminal`: accept only a matching terminal snapshot/event; record late lower-version events as no-ops.
- unknown run, session, endpoint, or identifier mismatch: enter `correlation_fault`, preserve bounded redacted diagnostics, fail closed for execution/approval, and create operator-visible reconciliation work.

The reconciler never repairs rows by matching text or importing an uncorrelated Hermes history. It can repair only from an authenticated, schema-valid snapshot whose full correlation object matches the persisted registry.

## Threat model and failure handling

| Failure or threat | Required safe behavior |
| --- | --- |
| Model/tool/browser text supplies IDs, commands, or a claimed origin | Treat it as data. Thornhill and Hermes mint/validate IDs server-side; no text-derived linkage. |
| Duplicate SSE delivery or reconnect replay | Deduplicate by producer `event_id`; one inbox row, one reducer transition, one approval presentation. |
| Out-of-order event or missing stream segment | Detect with sequence/version, buffer/replay/snapshot, and withhold approval actionability until coherent. |
| Stale UI, voice callback, or previous session activation | Capture view epoch and exact resource identity; discard delivery without cancelling/altering the durable origin. |
| Ambiguous run-start result | Do not retry start; query by original `start_idempotency_key` plus full binding or mark indeterminate and reconcile. |
| Approval call timed out after possible delivery | Mark `indeterminate`, stop/reconcile, never resend allow/deny. |
| Hermes wait disappears or approval callback lacks identity | Invalidate/park the exact local record if possible; otherwise no actionable record and fail closed. |
| Concurrent decisions, parking, cancellation, or restart | Conditional row claims keyed by exact attempt/approval/nonce; exactly one transition wins. |
| Late terminal/progress event from an old run | Binding and state version reject it; it cannot overwrite a newer attempt or terminal state. |
| Taint/provenance metadata missing or malformed at a high-risk boundary | Treat as unknown/high exposure; allow bounded storage/display but do not create executable authority. |
| Endpoint replacement or cross-instance ID collision | Require endpoint namespace plus binding; remote deployments additionally need authenticated peer identity. |

## Migration plan

1. Publish this v1 contract and a schema fixture corpus before runtime changes. Version negotiation is mandatory; legacy peers do not silently claim v1 behavior.
2. Add additive correlation-registry, deduplicating event-inbox, reconciliation, and approval-decision fields. Backfill existing `work_id`, session link, and run link as `legacy_unverified`; never invent historical attempt, approval, event, or causal IDs.
3. Add a `start_idempotency_key` and echoed correlation object to the Hermes Runs API, including a lookup/replay surface keyed by that exact pair. Run in observe mode first: record missing/mismatched echoes and reconciliation gaps without changing ordinary non-authority presentation.
4. Require v1 correlation for every newly created approval. At cutover, a legacy pending approval is parked or marked indeterminate according to its known boundary state; it is not upgraded in place or reinterpreted from text. Explicit resume creates a fresh v1 attempt and approval.
5. Move approval decisions and terminal state changes behind the inbox/reducer/outbox transaction, then make malformed or unknown approval events hard-deny for actionability.
6. Remove legacy fallback only after active legacy work has reached a durable terminal/parked state and the reconciliation metrics remain clean for an agreed observation window. Rollback disables new starts; it never downgrades a live v1 decision attempt into an ambiguous legacy approval.

## Minimal telemetry

Emit compact structured metadata only, with no raw command, transcript, secret, decision nonce, or model-generated instruction.

- Counters: events received/deduplicated/rejected, schema failures, sequence gaps, snapshot/replay attempts, correlation faults, active approvals without complete binding, decision outcomes, indeterminate outcomes, stale UI/voice drops, and parked/reissued approvals.
- Histograms: ingest lag, replay/reconciliation duration, approval pending duration, decision round-trip duration, and time to close a correlation fault.
- Gauges: active attempts by state, unresolved gaps, pending/parked/indeterminate approvals, and oldest reconciliation obligation.
- Immutable audit fields: `work_id`, endpoint namespace, session binding generation, `attempt_id`, `run_id`, `approval_id` when non-secret, `decision_id`, `event_id`, versions/cursors, choice category, action/pattern digests, policy version, taint summary, and terminal/reconciliation reason.

The primary service-level invariant is zero actionable approvals lacking a complete matching `{work, session binding, attempt, run, approval}` tuple. A separate approval-churn metric counts re-presentations per approval ID and fresh reissues per work item; duplicates/replays must not increase either count.

## Conformance test matrix

| Scenario | Required oracle |
| --- | --- |
| Two jobs, similar titles/content, concurrent runs | No history/event/approval crosses the explicit bindings. |
| Same start request retried before and after Hermes acceptance | One attempt/run is bound through the idempotency key, or the result is indeterminate; no duplicate run. |
| Duplicate `approval.requested`, `decision_applied`, and terminal events | One inbox record/reducer effect/presentation; duplicates are visible no-ops. |
| Reordered progress, approval, terminal, and reconnect replay | State-version gap triggers replay/snapshot; no approval becomes actionable before repair. |
| Lost or malformed correlation field / unknown endpoint/run | Quarantine plus correlation fault; no modal, decision, or inferred history attachment. |
| Two concurrent decisions | One conditional claim and one Hermes control call; loser is stale/no-op. |
| Decision acknowledgement lost after Hermes may have applied it | Local approval becomes indeterminate, run is stopped/reconciled, and no retry happens. |
| Crash or durable-inbox/reducer failure after Hermes applies a decision | No false local success or retry; reconciliation finalizes only from a matching authoritative acknowledgement/snapshot with the same `decision_id`, otherwise remains indeterminate. |
| Approval timeout, resource parking, and explicit resume | Parking makes no decision; old IDs/nonces fail; resume gets a new attempt and approval. |
| Restart in starting, active, pending, deciding, parked, and terminal states | Reconciliation follows the state rules above; no terminal row coexists with an actionable stale approval. |
| Late callback from old browser/voice call after a new activation | It cannot render, speak, alter controls, acknowledge attention, or touch the new view. |
| Tainted tool/model/voice payload with valid-looking IDs or command text | Metadata persists as data; no ID/control object is accepted from content and no execution authority is created. |
| Endpoint/instance switch with colliding run ID | Namespace/binding mismatch is rejected and surfaced for reconciliation. |
| Fuzzed envelopes, oversized fields, duplicate IDs with differing payloads | Parser bounds input; inconsistent duplicates are protocol faults and cause no state mutation. |

Use deterministic fake Hermes streams, barriers, and a fake authority endpoint rather than sleeps or live model calls. The suite should fail if a reducer ignores an attempt ID, accepts a stale decision, turns a sequence gap into a current approval, permits a terminal-state resurrection, or derives correlation from text.

## Expected operational benefit

Approval churn falls because one logical approval has one Hermes-issued identity, one Thornhill row, one presentation, and one decision attempt even across reconnects. Uncorrelatable approvals are no longer shown as pending work: they are quarantined and reconciled or parked/reissued with fresh authority, so a missing or stale event cannot block a run indefinitely or trick an operator into deciding the wrong request.
