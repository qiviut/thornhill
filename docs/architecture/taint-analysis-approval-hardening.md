# Taint-analysis hardening for approval workflows

Status: design-only addendum, 2026-08-22. No runtime behavior, route, schema, or policy is changed by this document.

## Outcome and contract boundary

This addendum makes provenance and taint an explicit admission contract so tainted evidence cannot create approval churn or acquire authority. Structural validation stays fail-closed, while semantic taint analysis is bounded and non-blocking for valid approval workflows. It reduces clerical approval work—duplicate prompts, stale prompts, and policy confusion—without reducing the operator decision required for a new high-authority action.

[Hermes–Thornhill session correlation](hermes-session-correlation.md) owns identities, ordering, durable inbox/outbox, and reconciliation. The [operator-status contract](hermes-status-event-contract.md) owns projections and summaries. [Reliability boundaries](reliability-boundaries.md) owns durable approval parking, exact decision state, and resource release. The [Kanban workflow](hermes-kanban-workflow.md) owns card/work projections, and [voice approval/run lifetime](../operational-issues/voice-approvals-and-run-lifetime.md) owns voice attention and resume behavior. This document owns the missing evidence/taint rules. If there is a conflict, the correlation contract wins.

The supported deployment remains the single-operator Tailnet model in [the security model](../security-model.md). That boundary does not make model output, tool output, history, hook payloads, or browser content authoritative. An endpoint/producer ID names a namespace but is not peer authentication; a remote or separately administered Hermes requires authenticated transport and independently protected peer identity before v1 control events may be accepted.

The code evidence below was verified at runtime revision `fcc3755d855594e1a1a4deaf0fd32f31dc0e6644`; the companion correlation, status, and Kanban documents are design proposals, not current runtime behavior.

## Current starting point

The current release already has important safeguards, but it is not the proposed v1 taint/correlation protocol:

- `internal/bridge/hermes.go:174-188` unmarshals a compact, unversioned Runs event directly into `runEvent`; it has no event ID, producer, provenance, attempt, or Hermes-issued approval identity.
- `internal/bridge/hermes.go:903-1013` turns the event's command, description, and pattern keys into a Thornhill-local approval ID and nonce. This is the ambiguity that v1 must remove; a local ID cannot correlate a delayed Hermes wait.
- `internal/bridge/hermes.go:921-939` responds to a second uncorrelatable approval with `resolve_all=true`. V1 must instead use an exact per-item control identity or quarantine/stop/reconcile the run; it must never bulk-resolve a live wait from ambiguous evidence.
- `internal/desk/desk.go:908-919` accepts a raw approval ID and decision nonce from a model tool call, while `internal/desk/desk.go:1060-1072` returns the pending approval through that model-visible tool path. V1 must replace raw nonce transport with a server-minted, current-approval action reference; the nonce stays in protected broker state.
- `internal/store/store.go:504-554` atomically races decision claim against parking, and `internal/bridge/hermes.go:821-900` parks before releasing upstream resources. These safeguards remain required.
- `internal/bridge/hermes.go:480-515`, `internal/desk/desk.go:689-700`, and `internal/summarize/summarize.go:159-190` already quote/bound some resumed evidence, attention, and summaries as untrusted. Quoting is a rendering/prompt discipline, not a provenance or authority proof.
- `internal/events/bus.go:157-208` persists an in-process publication separately from job mutation. The proposed inbox/reducer/outbox transaction is therefore a future prerequisite for authoritative v1 control events.

Do not label the current event shape as v1-compatible, backfill synthetic Hermes approval IDs, or infer an old event's provenance from text, time, or local sequence.

## Terms and non-negotiable invariants

An `EvidenceEnvelope` and `ControlEnvelope` are conceptual internal roles, not a second wire protocol. The existing `thornhill.hermes-event.v1` envelope remains the outer status/correlation contract: its validated typed payload is the control portion, while bounded evidence is persisted/rendered as a referenced internal record. Any future wire-visible `ApprovalSubject` fields must be added through that existing contract owner; this addendum does not define a parallel schema.

An approval instance binds one exact action snapshot. A reusable policy matches one canonical `ApprovalSubject`, not command prose or a model classification. The subject contains the policy namespace/version, Hermes endpoint namespace, static execution-profile ID, validated tool capability and operation class, risk tier, reusable-scope eligibility, and exact canonical pattern set. `ApprovalSubject` is an internal server-owned canonical value; the wire contract need only carry its versioned digest unless the status-contract owner explicitly extends the schema. The instance additionally binds the action/evidence digest. Digests bind bytes after deterministic canonicalization; they do not authenticate a producer, prove consent, or make content safe.

An `ApprovalActionRef` is a server-minted, current-approval control reference for the Desk resolver. It is neither an approval ID nor a decision nonce, is bound to the full correlation tuple and (for voice delivery) the current call/turn as a presentation fence, carries no display evidence, travels only in the typed resolver control path, and becomes invalid on decision, parking, invalidation, session-generation change, or resume. A call/turn fence prevents stale delivery but is not proof of consent. The reference lets a model convey a canonical operator choice to the broker without exposing a one-use authority secret; it does not turn model interpretation into cryptographic proof of consent.

1. Evidence is data, never an identity, capability, policy selector, tool name, route, work ID, session/run/approval ID, decision, or executable argument.
2. Every ingress records a bounded source class and ancestry. A caller-supplied source label is evidence only; the ingress component assigns the persisted provenance state.
3. Taint is monotonic. Parsing, escaping, redaction, summarization, storage, replay, hashing, or model classification may create a safer representation, but may not clear `untrusted_content` or promote it into authority. An independent server-side validation can derive a separate typed field without laundering the source bytes.
4. Unknown, malformed, over-budget, or conflicting provenance is high exposure. It may be retained as bounded redacted evidence and surfaced as a durable reconciliation fault, but cannot create an actionable approval, an automatic allow, a reusable policy, or an execution request; it must not retain a worker, stream, or approval capability indefinitely.
5. Only a closed-schema, server-validated control event may create an approval record. It must include the full current correlation tuple, producer/event identity, causal ordering, Hermes-issued approval ID/sequence, canonical action and policy-subject digests, and a provenance summary.
6. A policy may only authorize an action already exposed by static server-owned execution configuration. Tainted task text may request work, but cannot choose a toolset, workspace, model/provider override, route, policy namespace, capability, or approval scope.
7. Taint never makes an allow safer. A taint/provenance result may narrow execution, require fresh authority, quarantine an event, or suppress reusable-policy matching; it may never add an allow, broaden scope, or silently convert a deny/park/indeterminate result.
8. One logical approval has one Hermes-issued approval identity, one exact approval subject, one Thornhill decision attempt, and at most one concurrent operator presentation. Explicit retry/re-presentation is allowed for unacknowledged delivery and must not create a new approval, attempt, or authority grant. Duplicate/replayed evidence is an auditable no-op; a collision is quarantined/reconciled, never sent through a bulk, first-match, or `resolve_all` authority path.
9. `pending -> deciding`, parking, cancellation, invalidation, and terminalization are conditional, full-binding transitions. No lock, database transaction, or decision nonce survives remote authority I/O as a substitute for an attempt fence.
10. A resume starts a fresh attempt and approval identity. Prior command/pattern/history data remains quoted evidence; it cannot revive a nonce, policy match, decision, or permission.
11. Browser/voice/session views use a separate `{view_epoch, resource_id}` fence. A stale callback may clean up its own resource but cannot render, speak, enable approval controls, or mutate a newer view.
12. The Desk/model resolver receives only the current `ApprovalActionRef` and a canonical choice; it never receives a decision nonce or accepts an approval identity parsed from free-form evidence. The broker resolves the reference to protected state and rechecks the full binding before I/O.
13. Raw content, secrets, decision nonces, start idempotency keys, and high-cardinality identifiers never enter metrics, summaries, ordinary logs, or model-visible evidence. Redaction/escaping protects a sink; it does not satisfy any of the preceding authority checks.

### Structural admission versus semantic analysis availability

Structural provenance validation is on the admission path and is synchronous, bounded, and fail-closed. A structurally valid, fully correlated approval may proceed to the normal operator workflow even when semantic analysis is unavailable. Semantic analysis is an asynchronous advisory sidecar, never a worker, SSE, approval-lock, or voice-resource prerequisite.

Use a bounded assessment state set such as `not_requested`, `pending`, `clean`, `tainted`, `failed`, `timeout`, and `stale`. Each assessment binds to the evidence digest, full work/session/run/attempt/approval tuple, analyzer identity/version, and analysis-policy version. Assessment ordering is local to that assessment stream; it must not be compared as if it were Hermes `source_state_version` or Thornhill `ledger_version`. A result with the wrong digest, tuple, analyzer generation, or current attempt is evidence-only and produces no reducer mutation.

`pending`, `failed`, `timeout`, `stale`, and analyzer-backpressure states have one safe effect: suppress reusable-policy matching and retain the existing explicit one-time decision or safer-alternative path when the structural approval binding is valid. They must not delay or retain the approval workflow, synthesize allow/deny, broaden scope, or create a second prompt. `clean` is not authority and may only participate in an exact existing `ApprovalSubject` policy match. Queue depth, assessment age, retries, and obligations are bounded and observable; analyzer recovery never replays an operator decision.

## Explicit data-flow boundaries

| Source and representation | Permitted transformation | Required gate before the next boundary | Safe disposition on failure |
| --- | --- | --- | --- |
| Browser/voice task, answer, or model function arguments | Parse to a bounded input envelope; preserve task text as evidence | Static Desk/dispatcher schema and the server-owned operation selected by the route/tool definition | Reject malformed input; never let text select execution profile or approval scope |
| Hermes Runs SSE, including tool output and `approval.requested` | Retain a bounded raw digest/evidence reference; parse a versioned envelope | Producer/transport check, closed schema, duplicate-key rejection, field budgets, complete correlation, event ID/hash, sequence/version, and provenance validation | Quarantine into a durable reconciliation obligation with no reducer mutation/modal or resource-owning approval wait |
| Semantic taint analyzer/assessment worker | Consume only a bounded evidence reference and emit an assessment receipt | Evidence digest, full correlation tuple, analyzer/policy version, bounded queue/deadline, and assessment-generation check; no control fields | Persist a stale/failed/timeout observation and release the sidecar obligation; valid structural approval remains on the normal explicit path |
| Tool/MCP output, files, URLs, transcript/history, worker result, summary, or hook telemetry | Create or extend an `EvidenceEnvelope`; quote, redact, and escape per sink | Evidence-size/depth limits and sink-specific renderer/prompt wrapper | Bound or reject; never reinterpret as control input or prompt authority |
| Canonical action/pattern candidate from an approval event | Deterministically validate known capability/operation enums and canonicalize the exact pattern set into an `ApprovalSubject` | Static capability catalog, policy semantics version, endpoint namespace, risk tier, and exact-subject digest | Treat as unmatched/high exposure; require a fresh human decision only if a valid live approval exists |
| Thornhill operator decision | Map a canonical choice plus current `ApprovalActionRef` to one decision attempt | Reference resolves to the current pending row; full correlation, protected decision nonce, attempt token, per-run serialization, and matching Hermes acknowledgement all validate | `indeterminate`, stop/reconcile the exact run, and never retry allow/deny automatically |
| UI, voice, push, logs, event corpus, and summary prompt | Render redacted evidence with an explicit quoted-data boundary | Exact work/attempt receipt basis plus view/resource fence; renderer escapes control and bidi characters | Drop stale delivery or show reconciliation pending; never mutate state or acknowledge approval |
| Resume/replay/snapshot | Rehydrate bounded history/evidence and reconcile from a matching snapshot | Original start idempotency key, full correlation binding, producer cursor/version, and fresh attempt creation | Park/indeterminate/correlation fault; never synthesize linkage from text or local timestamps |

The two control-plane transitions remain deliberately separate:

```text
EvidenceEnvelope -> model context -> model-emitted request -> server validation -> ControlEnvelope -> reducer/approval broker
operator words -> canonical decision enum -> conditional local claim -> exact Hermes authority call -> matching acknowledgement
```

Neither arrow may be collapsed into `content -> shell`, `summary -> policy`, `taint label -> allow`, or `missing callback -> decision`.

## Reducing approval requirements safely

The target reduces approval friction by eliminating false work, not by lowering the authority threshold.

| Situation | Required behavior |
| --- | --- |
| Parse, redaction, storage, status projection, replay, and presentation-only progress | Never require or manufacture an approval; these are non-authority operations. |
| Semantic analysis pending, failed, timed out, stale, or backpressured with structurally valid correlation | Do not hold workflow resources or create a second prompt. Suppress reusable-policy matching and retain one existing explicit one-time decision or safer-alternative path. |
| Duplicate/replayed event for one valid approval identity | One persisted receipt and one existing concurrent presentation; explicit re-presentation may reuse that approval only. Increment duplicate telemetry only. |
| Stale, missing, malformed, or conflicting correlation/provenance | No actionable prompt and no automatic allow. Quarantine/reconcile known work or park/reissue it with fresh authority. |
| Exact existing policy match | An automatic current decision is permitted only after the complete validated `ApprovalSubject` and current live approval binding match. A digest/text match alone is insufficient. |
| New, unmatched, or high-risk action | Show one correlated approval with redacted evidence and canonical choices. Taint classification cannot make it auto-allowed. |
| Explicit safer alternative | Resolve only the exact proposed mechanism through the existing typed choice. It is neither a grant for an alternative nor a broad denial. |
| Parking, silence, timeout, disconnect, or restart | Preserve/reclaim state without a decision. Explicit resume verifies current state and creates a fresh attempt/approval if still needed. |

For reusable policies, preserve automatic denial whenever its exact legacy semantics can be deterministically represented: a denial cannot grant capability. Preserve an automatic allow only when the migration can derive a complete v1 `ApprovalSubject` with the same endpoint, capability, risk, canonical pattern semantics, and policy version. Otherwise mark the policy `legacy_review_required` and ask once only when it is next encountered; do not mass-reprompt or silently broaden it.

## Implementation checklist and dependency order

1. Publish a schema/fixture package before editing the bridge: the existing `thornhill.hermes-event.v1` envelope plus typed payloads, conceptual `EvidenceEnvelope`/`ControlEnvelope` mappings, `ApprovalSubject`, provenance/taint enums, semantic-assessment states, field budgets, canonicalization rules, and a reason-code registry. Include adversarial fixtures for duplicate JSON keys, unknown fields, oversized/deep content, conflicting event IDs, bidi/control characters, and role-like text.
2. Implement the correlation contract's additive registry, start idempotency lookup, deduplicating inbox, authoritative event cursor, reducer, outbox, and reconciliation obligations. Do not make taint metadata authoritative before this identity/ordering foundation exists.
3. Add a narrow Hermes-event ingress adapter at the current `scanRunEvents`/approval boundary. It must preserve an evidence digest before parsing, validate the envelope once, separate evidence from typed control fields, and pass only validated values to the reducer. It must not retrofit the old `runEvent` struct with optional fields and call that validation.
4. Define semantic analysis as a bounded sidecar contract, not an approval dependency: assessment receipt, evidence digest, full correlation tuple, analyzer/policy version, local assessment generation, deadline, retry budget, and terminal stale/failed/timeout dispositions. Prove that analyzer queueing, failure, restart, or rollback releases its own resources and cannot retain workers, SSE streams, approval locks, or voice resources.
5. Add additive evidence and approval records: immutable evidence reference/digest, source class, ancestry, taint/provenance summary, schema/policy versions, canonical action/policy-subject digests, producer event/hash, exact correlation references, and a protected hash of the current action reference. Keep raw action references, secrets/nonces, and start keys out of evidence, telemetry, and ordinary-log rows.
6. Move approval creation, decision claim, projected work state, attention record, accepted event receipt, and outbound status receipt into the same transaction. Replace Desk/model raw approval-ID/nonce arguments with a scoped `ApprovalActionRef`; the broker alone resolves it to the protected nonce and exact Hermes approval ID/sequence plus Thornhill decision ID. Remove `resolve_all`/first-match control paths, keep external Hermes decision I/O after the local conditional claim, and finalize only from the matching acknowledgement or snapshot.
7. Replace raw pattern-policy matching with a deterministic `ApprovalSubject` builder backed by a server-owned capability catalog. The builder must reject unknown capability/operation/risk combinations and must prove exact canonical-set equality for reusable policy matching. Persist the full internal subject or its protected digest according to the status/correlation contract; do not invent a second wire schema.
8. Route all Desk announcements, status summaries, session history, tool/MCP output, and resume briefs through the evidence renderer. Keep their quote wrapper, but add taint/provenance references and exact receipt basis rather than relying on prompt prose alone.
9. Add view-epoch/resource fencing wherever a reconnect, attention response, approval control, or voice output can outlive a job/session activation. Durable work continues; stale delivery is dropped. Allow explicit re-presentation only through the existing approval identity and current view/turn fence.
10. Add deterministic conformance tests and a generated requirement-to-case report. The CI gate must reject an untested normative invariant; accepted divergence requires an explicit expected-failure record rather than a skip. Include the taint-specific cases below and reference the correlation/status/Kanban cases they exercise.
11. Only after the preceding gates pass, enable v1 for newly admitted approval-bearing runs. Legacy active work remains in its current state and is never upgraded in place.

## Threat-model updates

| Threat or failure | Required containment |
| --- | --- |
| Tool/model/MCP/history content impersonates an ID, policy, or instruction | Content remains an `EvidenceEnvelope`; server-minted IDs and static capability catalog decide control. |
| Validly transported but malicious/compromised Hermes event | Schema, producer/transport validation, full binding, provenance, event hash, ordering, and reducer checks all apply; transport success is not content trust. |
| Taint laundering through redaction, summary, digest, or LLM classification | Preserve taint ancestry; mark a new rendering/classification property separately; never use it to expand authority. |
| Analyzer outage, stale/forged assessment, rollback, or result replay | Bind assessments to evidence digest, full attempt tuple, analyzer/policy generation, and local assessment ordering; treat mismatch as evidence-only and pending/failure as no reusable-policy match. |
| Analyzer backpressure or malicious oversized input | Apply bounded evidence, queue, deadline, retry, and obligation budgets. Drop or quarantine excess assessment work without retaining approval, worker, stream, or voice resources. |
| Same event ID with altered payload, replay, reordered event, or stale browser callback | Immutable event-hash comparison, per-attempt cursor/version, inbox deduplication, and view/resource fences make it a no-op/fault rather than a new prompt or state update. |
| Policy confusion from similar command text or reordered patterns | Match only the complete canonical `ApprovalSubject`; action/evidence digest binds an individual request, not a policy namespace. |
| Decision transport loss or provider restart | Leave the exact decision indeterminate, stop/reconcile, and require fresh authority instead of retrying a potentially delivered choice. |
| Prompt injection or control-reference leakage through Desk/model context | Evidence never carries a nonce; only a current scoped action reference reaches the broker, which rechecks it against protected state and full binding. |
| Oversized/secret-bearing evidence or active markup | Per-field/depth budgets, redacted evidence references, inert rendering, and secret-free telemetry/logging limit denial-of-service and disclosure. |
| Session switch, Park, or concurrent resume/decision/cancel | Separate durable work identity from disposable view identity; exact conditional transitions allow one winner and leave late work auditable only. |

## Verification matrix and telemetry

Use a deterministic fake Hermes stream/authority endpoint, fake clock, barriers, and fault-injecting store; do not use a live model or sleeps as the oracle.

| ID | Fixture or interleaving | Required oracle | Minimum telemetry |
| --- | --- | --- | --- |
| TA-01 | Event with unknown field, duplicate JSON key, invalid enum, excessive depth, or over-budget field | Quarantined before any reducer/policy/renderer use | `taint_envelope_rejected_total{reason}` |
| TA-02 | Command-like/role-like browser, tool, MCP, history, or summary text claims an ID/policy/tool | It remains evidence; static fields alone drive control | `taint_control_confusion_rejected_total{source_class}` |
| TA-03 | Valid approval event has missing/malformed structural provenance or lacks a complete binding | No pending row, modal, authority call, auto-policy result, or resource-owning approval wait | `approval_nonactionable_total{reason=provenance_or_binding}` |
| TA-NB-01 | Structurally valid approval with semantic analysis pending, timeout, analyzer restart, rollback, or queue saturation | No worker/SSE/approval-lock/voice-resource retention; one explicit approval path remains available; no reusable-policy match or synthesized decision | `taint_analysis_state_total{state,reason}`; queue depth/age and obligation gauges |
| TA-SCOPE-03 | Assessment is `tainted`, `failed`, `timeout`, `stale`, or `unknown` versus a clean exact subject | Only scope reduction to the existing one-time decision or safer alternative; never automatic allow, broadened scope, or inferred deny | `approval_policy_match_total{outcome,policy_version}` and disposition counters |
| TA-04 | Same producer/event ID replayed; then same ID with another canonical hash | One receipt/presentation for replay; altered payload is a critical fault | received/applied/duplicate/hash-conflict counters |
| TA-05 | Reordered approval/terminal/progress or a source gap | No approval actionability until replay/snapshot repairs the exact attempt | gap/replay/snapshot counters and oldest-gap gauge |
| TA-06 | Exact policy subject versus one changed capability, endpoint, risk, pattern, version, or provenance constraint | Only exact complete match can auto-resolve; every near match requires normal live handling | `approval_policy_match_total{outcome,policy_version}` |
| TA-07 | Rendering/redacting/persisting untrusted evidence and progress | No broker call and no approval row; output is inert and bounded | non-authority operation counter; zero broker calls |
| TA-08 | Two concurrent decisions, parking, cancel, and resume against one approval; then a second outstanding approval event | One conditional winner; the second wait is quarantined/reconciled, with no duplicate, first-match, or bulk authority call and no state resurrection/leaked nonce | decision/park/cancel conflict, approval-collision, and indeterminate counters |
| TA-09 | Decision call times out after possible delivery | Exact row becomes indeterminate; run is stopped/reconciled; no retry | `approval_indeterminate_total{reason}` and reconciliation age |
| TA-10 | Parked request, delayed old terminal, then explicit resume | Old authority cannot resolve; new attempt gets fresh IDs and at most one fresh approval | parked/reissued/presentation-per-approval counters |
| TA-CORR-05 | Assessment completion is duplicate, stale, reordered, cross-attempt, or attached to another session | Deduplicated/audited evidence-only no-op; no state resurrection, cross-session attachment, or decision retry | assessment stale/duplicate/correlation-fault counters |
| TA-11 | Old view/voice/resource completion after a newer activation | It cannot render, speak, enable controls, update cursor, or acknowledge attention | `stale_view_drop_total{surface}` |
| TA-OPS-09 | Analyzer queue saturation, crash, retry exhaustion, or policy-version rollback | Assessment obligations are bounded and observable; normal approval workflow remains resource-bounded and no decision is replayed | queue depth/age, timeout, retry-exhausted, and obligation gauges |
| TA-12 | Resume history, attention, and debrief contain instruction-shaped text | Bounded quoted evidence reaches context only; any model-emitted request still needs normal server validation and cannot use evidence-derived identity or scope | evidence-render and prompt-boundary counters |
| TA-13 | Redacted event, summary, log, metric export, or model-visible tool result contains a nonce/start key/secret fixture | No sensitive value is present outside protected control storage | telemetry/redaction test failures are release-blocking |
| TA-14 | Model call supplies a current or stale raw approval ID/nonce in free-form arguments, then tries a stale action reference | Broker rejects it; only the current scoped `ApprovalActionRef` plus canonical choice can begin a decision attempt | action-reference rejection/expiry counters |
| TA-15 | Legacy pending/parked/policy rows during observe and controlled cutover | Observe mode makes no decisions or prompt changes and fabricates no identity; cutover promotes only complete exact-subject matches after its gate, while other legacy policies receive one-use review on next encounter | legacy counts, policy-review-required count, approval-churn baseline |
| TA-16 | Restart while starting, pending, deciding, parked, and terminal | Reconciler follows the exact binding; no terminal work has an actionable stale approval | reconciliation outcome and oldest-obligation gauges |

Use low-cardinality metric labels only: event family, source class, provenance state, taint disposition, analysis state, policy version, reason code, and outcome. Store opaque IDs, event hashes, exact tuple, assessment generation, and redacted evidence references only in the audit ledger. Track analyzer queue depth, oldest assessment age, timeout/retry exhaustion, stale-result drops, and released sidecar obligations separately from approval-state metrics. The rollout gate is: zero actionable approvals without complete structural v1 binding/provenance; zero authority calls without a validated control payload and matching durable claim; zero concurrent duplicate presentations per Hermes approval ID (explicit re-presentation is counted separately); and no unbounded growth in analyzer, parked, indeterminate, or reconciliation obligations over the agreed observation window.

## Phased low-churn rollout and rollback

1. Contract and fixtures: review this addendum with the correlation/status contracts, publish schemas and requirement IDs, and make no runtime change.
2. Passive observation: parse/validate v1-shaped envelopes beside current handling, run semantic assessment only as a bounded observer, record redacted mismatch/taint/analysis-state metrics, and never create, suppress, park, or resolve an approval because of the observer. Analyzer timeout or outage must be visible without holding workflow resources.
3. Shadow ledger: add additive evidence/correlation/approval-subject storage and compare the shadow reducer with the existing job projection. Record assessment receipts and stale-result dispositions separately; new data must be reconcilable without changing the operator surface.
4. New-run v1 admission: require complete v1 identity/provenance for newly created approval-bearing runs. Initially retain the existing operator-facing decision policy, but deduplicate presentations and quarantine uncorrelatable input.
5. Controlled policy cutover: enable exact canonical reusable-policy matches by risk tier after the conformance and observation gates are clean. Preserve compatible denials; promote compatible allows only when the full subject is proven. Mark the rest for one-use review on next encounter.
6. Legacy retirement: after all legacy active work is terminal, parked, or indeterminate and metrics remain clean for an agreed window, remove legacy fallback. Retain redacted historical evidence; never fabricate an upgrade path.

Rollback stops new v1 starts/policy promotion and leaves the durable ledger, inbox, evidence, parked work, and reconciliation paths intact. It never downgrades a live v1 decision attempt to legacy, replays an allow/deny, deletes evidence, or turns a missing acknowledgement into a decision.
