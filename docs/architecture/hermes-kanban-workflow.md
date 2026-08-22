# Hermes Kanban workflow in Thornhill

Status: design-only proposal, 2026-08-13. This document changes no runtime code,
routes, database schema, or dispatch behavior.

Depends on [Hermes–Thornhill session correlation](hermes-session-correlation.md),
the [operator-status contract](hermes-status-event-contract.md),
[reliability boundaries](reliability-boundaries.md), and the concurrent taint
hardening work. If this document conflicts with the correlation contract, the
correlation contract wins.

## Outcome and ownership

The operator gets one board-style pipeline for planning, execution, approval
waits, review, and historical work without creating a second mutable state
machine for the same work.

The proposed composition is deliberately asymmetric:

| Concern | Canonical owner | Rule |
| --- | --- | --- |
| Card identity, graph, assignee, planning/review lifecycle | Hermes Kanban | A card is addressed by `{board_id, card_id}`, never title or body text. |
| Work record, execution-attempt/session/run binding, durable approval/decision, attention, and reconciliation | Thornhill PostgreSQL | Thornhill is authority for execution admission and consent history; it does not impersonate Hermes' live run or wait. |
| Board columns and operator summary | Thornhill deterministic projection | Rebuildable read model; it does not independently advance either source state. |
| Hermes execution and live approval wait | Hermes | Hermes acknowledges only an exact correlated run/decision. |
| Generic hooks, comments, summaries, attachments, task text, commands, descriptions, and patterns | Evidence only | They are untrusted content, not workflow control or authority. |

A Thornhill-specific external Kanban lane is the only component allowed to
bridge a card to a Thornhill work item. The ordinary Kanban dispatcher and the
Thornhill lane must never both claim/launch the same card. The lane uses Kanban's
normal claimed-run fence and ends its board claim when work enters a human wait;
it must not retain a Kanban worker lease merely because an approval remains open.

One implementation card binds to one Thornhill `work_id`. That work may have
several nonterminal attempts (`attempt_id`) for an answer or explicit parked
resume. Once it is terminal, a requested change creates a new remediation child
card and a new `work_id`; a terminal work record is never silently reopened.
This selects downstream review/remediation cards over same-card rework for
Thornhill-bound execution.

## Columns and states

A visible lane is a deterministic projection of a canonical Kanban card state,
a Thornhill work projection, and any integrity overlay. It is not a third state
that drag-and-drop may write directly.

| Board lane | Canonical card state | Thornhill condition | Entry and allowed exit |
| --- | --- | --- | --- |
| Intake | `triage` | no work binding | Explicit specification/decomposition moves it to `todo`. |
| Planned | `todo` | no active work | Parent completion promotes it to `ready`; a dependency wait returns here. |
| Ready | `ready` | no live attempt | The sole lane controller may claim and start it. |
| Starting | `running` | binding exists; start is not yet proved | Only matching `run.accepted` enters Executing. An ambiguous start enters Reconciliation, never a fresh retry. |
| Executing | `running` | exact current attempt is `running` | May wait for input/approval, block for a typed non-authority reason, finish, fail, or cancel. |
| Needs input | `blocked(kind=needs_input)` | `needs_input` | Only an explicitly accepted answer starts a fresh attempt. Generic unblock is rejected. |
| Awaiting approval | `blocked(kind=awaiting_approval)` | `needs_approval` | Only a matching Thornhill broker decision acknowledgement may return it to Executing; a matching typed terminal/invalidation outcome takes its normal non-running transition. It is not Review. |
| Parked approval | `blocked(kind=parked_approval)` | `parked_approval` | Explicit resume verifies state and creates a fresh attempt/approval. Generic unblock, reclaim, and retry are rejected. |
| Blocked | `todo` or `blocked(kind in {dependency, capability, external})` | no authority wait | Resolve the named typed condition; only dependency completion may auto-promote. |
| Reconciliation | integrity overlay; base state is retained | sequence gap, stale binding, start indeterminate, or conflict | No dispatch, approval, or terminal claim until replay/snapshot reconciliation succeeds. |
| Review | `review` on a separate review card | no active implementation work | Reviewer completes it or creates a remediation child card. It never resolves a runtime approval. |
| Failed | `blocked(kind in {execution_failed, start_indeterminate, correlation_fault})` | `failed` or integrity-failed | Preserve evidence and create a remediation child with a new work binding after a terminal failure. Only pre-terminal start reconciliation may query the original start key to discover an admitted run; there is no generic or same-work retry. |
| Done | `done` | exact work terminal outcome is `done` | Immutable implementation handoff. A dependent review card may now promote. |
| Cancelled | `archived` plus immutable `terminal_disposition=cancelled` | exact work terminal outcome is `cancelled` | Retained history; archive is not itself cancellation. |
| Archived | `archived` without the cancelled disposition | completed historical card | Never dispatchable. |

`kind` is a fixed, schema-validated enum and a redacted evidence reference, not
a free-form string that a task body or worker can choose. The initial integration
needs adapter support for `awaiting_approval`, `parked_approval`,
`execution_failed`, `start_indeterminate`, and `correlation_fault`; treating all
of them as an ordinary generic Kanban block would make unsafe unblocking too
easy.

The Reconciliation lane is an overlay and visually takes precedence over its
base lane. It must show the last proven state and a bounded reason code rather
than claiming a guessed completion or failure. Presentation may paginate, but
operator counts and the active-work query must not hide queued, input-blocked,
or approval-bearing cards.

## Allowed transitions and policy

```text
triage -> todo -> ready -> starting -> executing
                  ^                     |  |  |  \
                  |                     |  |  |   -> done -> review child / archive
                  |                     |  |   -> failed -> remediation child
                  |                     |  |   -> cancelled -> archived(cancelled)
                  |                     |  -> needs_input -> fresh attempt -> ready
                  |                     -> awaiting_approval -> (executing | done | failed | cancelled)
                  |                                      \-> parked_approval -> explicit resume -> ready
                  \---------------- dependency/capability resolution ----------------/
```

1. `ready -> starting` requires: one valid Kanban claim, the expected card
   revision/run ID, a Thornhill work binding, a server-minted start idempotency
   key, an allowlisted execution policy, and capacity in both schedulers.
2. `starting -> executing` requires the exact echoed correlation binding and
   `run.accepted`. A timeout after the start boundary is `start_indeterminate`;
   reconciliation may use the original idempotency key but may not create a
   replacement run.
3. `executing -> needs_input`, `awaiting_approval`, `parked_approval`, `done`,
   `failed`, or `cancelled` requires a current correlated Thornhill state/event
   and its expected version. A board comment, worker exit code, model summary,
   or notification cannot make these transitions.
4. `needs_input -> ready` occurs only after Thornhill durably accepts a scoped
   answer. `parked_approval -> ready` occurs only after an explicit resume
   command has verified the old state and invalidated its old authority. Both
   create a new `attempt_id`; the old `run_id` and approval are not reused.
5. `awaiting_approval -> executing` occurs only after
   `approval.decision_applied` matches `{work_id, session_generation,
   attempt_id, run_id, approval_id, approval_seq, decision_id}`. The exact
   acknowledgement, not the choice alone, may return the run to execution;
   that return is not a grant marker or proof of a later tool side effect.
6. `awaiting_approval -> failed` requires a matching `approval.invalidated`,
   `approval.expired`, or indeterminate authority outcome. A matching
   `run.terminal(done)` or `run.terminal(cancelled)` may instead take it to
   Done or Cancelled. Neither path can be inferred from silence, a decision
   choice, delivery failure, or a missing callback.
7. `executing -> done` completes the implementation card. If review is needed,
   a pre-linked review card becomes ready. A reviewer's requested changes create
   a remediation child with its own work binding; they do not reopen a completed
   correlated work item.
8. Direct UI moves to Running, Done, Cancelled, Awaiting approval, Parked
   approval, or Reconciliation are forbidden. The UI calls a typed control
   action, and the relevant source-of-truth reducer decides the resulting lane.
9. A cancellation becomes visible only after the exact active attempt confirms
   it. Archiving an arbitrary card neither cancels a live run nor suppresses the
   reconciliation obligation.

## Card-to-work interface and correlation

`work_id` is the correlation contract's canonical name for the Thornhill durable
job/work record (the current Thornhill job ID). A card binds once to that
server-issued opaque identity; it does not carry a second, mutable job alias or
derive a join from a display name.

The lane stores a durable, versioned binding. It carries identifiers; it never
uses a display name, timestamp proximity, task description, command preview, or
pattern text to join records.

| Binding field | Owner and use |
| --- | --- |
| `board_id`, `card_id`, `card_revision`, `kanban_run_id` | Hermes Kanban card and the current claimed-run fence. A Kanban run is not proof that a remote Hermes run is live. |
| `binding_generation`, `work_id` | Thornhill's immutable card-to-logical-work link. One active binding per card. |
| `hermes_endpoint_id`, `session_id`, `session_generation` | The session namespace and stale-session fence. |
| `attempt_id`, `start_idempotency_key`, `run_id` | One start admission and one Hermes execution attempt. |
| `approval_id`, `approval_seq`, `decision_id` | Optional exact approval/decision linkage. `decision_nonce` is deliberately absent. |
| `producer_id`, `event_id`, `source_seq`, `source_state_version`, `ledger_version` | Delivery deduplication, causal ordering, and Thornhill projection ordering. |
| `content_ref`, `content_digest`, provenance/taint summary | Bounded evidence reference only; never a control-plane field. |

At ingress, the status profile's `stream_seq`, `state_version`, and replay
`cursor` normalize respectively to the correlation contract's `source_seq`,
`source_state_version`, and cursor field. They are boundary aliases, not two
parallel orderings: persisted board bindings use the correlation names only, and
a dual/mismatched value is quarantined as a correlation fault.

The control surface is intentionally small:

- `dispatch_card`: accepts a card reference, expected card revision/run fence,
  execution-policy ID, and bounded card-content reference. It returns or
  replays the existing work binding by idempotency key.
- `submit_input`: accepts a current binding and an input-envelope reference.
  Thornhill validates the target wait before it admits a fresh attempt.
- `resolve_approval`: is a Thornhill broker action, not a Kanban action. The
  board supplies an opaque approval action reference and canonical choice;
  Thornhill mints/claims `decision_id` and keeps the nonce private.
- `resume_parked`, `cancel_work`, and `reconcile_binding`: all require the
  expected current binding/version. They are narrow control operations, not
  generic card edits.
- lane events are delivered through durable inbox/outbox records. A process-local
  Kanban event, the in-memory Thornhill bus, SSE replay, voice delivery, or push
  notification is never the sole control record.

Every command and callback must compare its full binding before a mutation. A
late board worker may release its own resource, but cannot modify a newer card
run, work attempt, session view, approval control, summary cursor, or media
session.

## Approval and blocked-work rules

Approval remains a conversation followed by a one-use brokered authority
decision. The board may show a redacted request, stable action/pattern digests,
risk category, and current wait state. It must not show a nonce, bearer token,
raw secret, or a command/pattern as executable text.

- `awaiting_approval` is a human authority wait, not a reviewer queue.
- Silence, a comment, an emoji, a drag operation, parking, a timeout, or a
  generic unblock is neither allow nor deny.
- Parking commits durable evidence before releasing scarce execution resources.
  It makes no decision and causes the old approval identity to become stale.
- A decision transport ambiguity is `indeterminate`: stop/reconcile the exact
  run and require fresh authority rather than retrying an allow or deny.
- Approval serialization is per current Hermes run. It must not become a global
  board lock that stalls unrelated cards.
- A normal `blocked` card has an explicit reason enum and resolution owner.
  `dependency` may auto-promote only after the graph gate clears; `capability`,
  `external`, `execution_failed`, and correlation faults require an explicit
  action or remediation card.

## Taint, access, and concurrency boundaries

### Taint and input handling

Card titles, bodies, comments, parent handoffs, attachments, URLs, worker
summaries, status prose, tool outputs, command descriptions, and approval
patterns enter the lane as `ContentEnvelope`-style evidence with source and
taint metadata. Missing or malformed provenance is high-exposure evidence.

Only static, server-owned configuration may choose a lane, profile, toolset,
workspace, execution policy, model/provider override, or action template. A
card may reference an allowlisted option, but its content cannot manufacture an
option. The adapter supplies bounded, quoted task evidence to an appropriately
restricted worker; it never feeds task text into a shell, interpreter, route,
policy field, approval scope, or correlation ID.

All UI/voice/log/summary rendering escapes control and bidi characters. Taint
propagates through handoffs, summaries, replay, and attachments. A sanitizer or
model classification can support routing but cannot wash content into authority.

### Access control

The first release remains within Thornhill's supported single-operator,
operator-only Tailnet model. It must not expose Hermes' local Kanban database or
an unauthenticated Kanban plugin endpoint through Thornhill.

- Every new Thornhill route is registered in the code-backed route inventory;
  its caller, data class, and authority are tested with the route table.
- Read-only board observation is separate from mutation. The lane controller has
  dispatch/reconciliation capability only; approval decisions remain broker-only.
- Workers are card-scoped and can terminate only their expected Kanban run.
  Reviewers can complete/reject only their review card. The reconciler cannot
  invent a decision or a remote run.
- `/hooks/hermes` remains non-authoritative telemetry and is not an alternative
  board-control or approval endpoint.
- If another person, service, or untrusted device can access the board, this
  deployment assumption is invalid. Add application identity, per-principal
  authorization, and per-board data isolation before that expansion; a tenant
  label is not a security boundary.

### Concurrency and recovery

There is exactly one dispatch owner per Thornhill-bound board. It holds the
Kanban claim before it asks Thornhill to start work, and it respects both the
board-wide/per-profile Kanban caps and Thornhill's active-run admission cap.
Claiming, start admission, and outbox publication are individually transactional
in their owning store; cross-store work is at-least-once and reconciled, never
claimed to be a distributed transaction.

A controller retry can replay idempotent binding/projection requests. It cannot
retry an ambiguous run start, an approval decision delivery, or a side effect
whose absence was not proved. Capacity leases are released for input and parked
approval waits, while their durable obligations remain visible. Restart recovery
uses the correlation registry, original start key, inbox/outbox state, and Hermes
snapshots; host-local PID death never proves a remotely administered Hermes run
has stopped.

## Status mapping and operator summaries

The board projects the current durable snapshot, not the latest text or event
arrival. The primary mapping is:

| Thornhill execution state | Board lane / canonical card treatment | Summary treatment |
| --- | --- | --- |
| no work; `triage`, `todo`, `ready` card | Intake, Planned, or Ready | planning/backlog count only |
| start pending / `queued` | Starting while a matching claim/start key exists; otherwise Ready | "starting" only when the binding is proved |
| `running` | Executing | active count and bounded progress |
| `needs_input` | Needs input / `blocked(kind=needs_input)` | operator action required; cite the durable question reference |
| `needs_approval` | Awaiting approval / `blocked(kind=awaiting_approval)` | operator decision required; no implied choice or scope |
| `parked_approval` | Parked approval / `blocked(kind=parked_approval)` | unresolved, no live authority; explicit resume required |
| `done` | Done implementation card; dependent review card may become Ready | completion only after matching terminal receipt |
| `failed` or authority indeterminate | Failed / typed blocked reason | remediation required; never silently requeued |
| `cancelled` | Cancelled terminal disposition, then optional archive | cancellation only after matching terminal receipt |
| source gap, stale binding, terminal conflict, or unknown identity | Reconciliation overlay | say "reconciliation pending" and retain the last proven state |

`review` is a Kanban workflow state for a review card; it has no equivalence to
`needs_approval`. `archived` is retention/visibility management; it has no
equivalence to `cancelled`.

Every board card, spoken line, push item, and aggregate status carries an
inspectable basis:

```text
{board_id, card_id, card_revision, work_id, attempt_id, run_id,
 ledger_version, accepted_event_ids_or_cursor_range, reconciliation_state}
```

A multi-card digest keeps one basis per card. It is computed from committed
card/work projections with this priority: integrity/reconciliation fault,
operator approval/input action, failed/cancelled outcome, parked wait, active
work, review/backlog, then terminal history. Delivery of voice or push attention
is downstream of the state transition; a delivery success or failure never
changes a card's state. Model-written summaries may explain an already committed
basis, but cannot create, downgrade, or resolve a status.

## Migration and rollback

1. Publish this contract and fixture corpus first. Normalize terminology to the
   correlation contract (`source_seq`, `source_state_version`, and
   `ledger_version`) rather than creating another status identity scheme.
2. Add only additive card-binding, typed-block, inbox/outbox, and projection
   records. Existing jobs/cards appear as `legacy_unverified` observation rows;
   do not invent historical attempts, approval IDs, causal links, or terminal
   causes from names or text.
3. Run a read-only projector against one private board. Compare lane counts and
   summary bases with the current Thornhill board; record mismatches, gaps, and
   taint/provenance failures without dispatching work or exposing approval
   controls.
4. Enable the dedicated lane for newly created cards only, initially with manual
   dispatch, small explicit capacity limits, no auto-decomposition, and no
   generic unblock for authority/failure states. Require the correlation,
   status, taint, and route-classification conformance gates before enabling
   control mutations.
5. Cut over only after observe-mode metrics show no unresolved binding mismatch
   for an agreed window. Legacy active approval work reaches a durable terminal,
   parked, or indeterminate state; it is never upgraded in place.
6. Rollback stops new card claims and preserves both stores' durable state. It
   does not cancel unknown remote runs, replay decisions, downgrade a v1
   approval, or delete evidence. Reconciliation remains available until all
   bindings reach a known state.

## Conformance and telemetry matrix

Use deterministic fake Kanban/Hermes endpoints, a fake clock, barriers, and
fault-injecting stores. Do not use live models, sleeps, or model-written prose as
the test oracle.

| Case | Fixture / injected fault | Required invariant | Required telemetry |
| --- | --- | --- | --- |
| Normal path | triage → ready → start → running → done | one card binding, work, attempt, terminal receipt, and summary basis | transition counts; dispatch-to-accept latency |
| Competing controllers | two controllers claim one Ready card | one Kanban claim and one work-start request win | claim-conflict and duplicate-dispatch counters |
| Ambiguous start | Thornhill writes start record; Hermes response is lost | query only the original start key or mark `start_indeterminate`; no new run | start-indeterminate count and reconciliation age |
| Duplicate delivery | replay same event ID after lost acknowledgement | one inbox receipt/reducer effect/attention/projection; duplicate is an auditable no-op | received/applied/duplicate event counters |
| Ordering gap | terminal/approval arrives before a missing predecessor | defer state, request replay/snapshot, and show Reconciliation; no approval/action summary | source-gap counter, cursor/snapshot outcome, oldest gap |
| Stale callback | old attempt completes after an explicit resume | old `{attempt_id, run_id, session_generation}` cannot mutate the new view/card | stale-event and stale-view drop counters |
| Approval race | concurrent decision, park, cancel, and restart | one conditional transition wins; no duplicate broker call or authority resurrection | decision/park conflict and indeterminate-decision counters |
| Park and resume | unresolved approval is parked, delayed old terminal arrives, then resume | parked remains non-decision; old ID/nonce fail; fresh attempt/approval required | parked/reissued approval counts and cleanup age |
| Cross-store partial failure | Kanban claim commits but work creation fails; or Thornhill commits and board projection fails | reconcile by idempotency/binding; do not duplicate work or silently lose the card | outbox retry, projection lag, binding-mismatch counters |
| Controller/worker restart | process dies while active, blocked, or deciding | no host-PID inference for remote work; restore/reconcile exact current binding | recovery outcome and oldest obligation gauges |
| Tainted evidence | task/comment/attachment/event contains role-like text, commands, IDs, or secrets | fixed schema fields alone drive state; evidence is escaped/redacted and creates no capability | taint-source/unknown-provenance and rejected-control counters |
| Summary/attention split | push or voice delivery fails after committed state | board state/basis remains correct; delivery retries do not create a second transition | attention retry/dedup counters |
| Access boundary | cross-origin, wrong lane, or observer attempts a mutation | route/capability denial; no card/work/approval mutation | authorization/route-policy denial counters |
| Legacy migration | imported old jobs and pending approvals | visible as `legacy_unverified`; no inferred link, auto-dispatch, or authority action | legacy population and unresolved-legacy gauges |

Keep metric labels low-cardinality: state, typed reason, producer class, policy
version, and outcome are appropriate. Put opaque card/work/attempt/event IDs in
the redacted audit ledger and summary basis, not metric labels. Minimum gauges
are active cards by lane, active runs, pending/parked/indeterminate approvals,
unresolved gaps, controller capacity, and oldest reconciliation obligation;
minimum histograms are queue age, dispatch-to-accept, event ingest lag, approval
wait/decision duration, and reconciliation duration. Raw task text, commands,
descriptions, patterns, nonces, tokens, and secrets never enter telemetry.

## Implementation gate

No runtime implementation should begin until the correlation registry and
idempotent start/replay semantics, status inbox/reducer/outbox, taint envelope
and sink restrictions, typed blocked-state support, route classification, and
this conformance matrix have agreed schemas and focused CI coverage. The desired
first implementation is a private read-only projection; control mutation is a
later, separately reviewed step.
