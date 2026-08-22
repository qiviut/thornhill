# Reliability and authority boundaries

Read this before changing job state, Hermes run ownership, approvals, browser
protocol parsing, or deployment. These decisions are deliberately load-bearing;
local simplifications can otherwise create duplicate work, implied consent, or a
deployment that cannot safely roll back. Read
[`security-model.md`](../security-model.md) alongside it for caller identity,
route authority, exposure assumptions, retained-data inventory, and security claim
status. The proposed future cross-system correlation contract is in
[`hermes-session-correlation.md`](hermes-session-correlation.md); it is design-only
and does not describe current runtime behavior.

## Durable job and run ownership

- PostgreSQL is authoritative. A job mutation and its River enqueue commit in
  one transaction; never reintroduce a database-then-enqueue crash window.
- A River delivery must durably claim a job and re-check cancellation before
  starting a Hermes run. Subsequent writes require the same run identity and an
  active status so an old stream cannot overwrite cancellation or a newer turn.
  The in-process cancellation callback is installed only after that durable
  claim; duplicate deliveries must return before installing or deleting the
  active owner's callback, and deferred cleanup must be owner-scoped so a newer
  resumed delivery cannot lose its cancellation handle.
- Hermes run-start requests are not retried after an ambiguous transport result.
  Repeating a non-idempotent start can duplicate work.
- The HTTP client bounds the wait for response headers, not the lifetime of the
  response body. Established event streams are governed by cancellation and a
  stream-idle threshold. This prevents a pre-header stall from consuming every
  River worker without imposing a deadline on a valid long conversation.

## Approval is a conversation, then a one-use authority decision

- Approval authority calls serialize per Hermes run, never process-wide. Work
  and conversations for unrelated runs must remain independent.
- The operator may ask for details, risks, or a safer alternative before making
  a decision. Questions and silence are not approval or denial.
- The control-call timeout starts only after an explicit decision is sent. The
  separate resource threshold parks an unresolved approval and releases its run;
  it never manufactures a decision. Resuming requires a fresh authority request.
- Provider request IDs (with FIFO order retained only as legacy evidence), nonces,
  current run ownership, and canonical choices are broker-enforced. Operator
  intent is still conveyed through the model, so the UI must show the pending
  command/scope and the Desk prompt must continue to require explicit words.
  Do not describe this as cryptographic proof of consent.

## Browser input is untrusted until parsed

`unknown` is the correct TypeScript boundary type for JSON, WebSocket payloads,
and other untrusted values: it means the shape has not yet been proven. Runtime
parsers narrow it into the explicit `Job`, `BusEvent`, and status unions before
application code can use it. Do not replace that quarantine type with `any` or a
type assertion, and do not leave validated application state broadly typed.

## Query growth and operator visibility

- Spoken lookup reads at most two candidate rows because that is sufficient to
  distinguish a unique match from ambiguity.
- The active-job query is intentionally not capped. It feeds the authoritative
  operator board; silently hiding queued, input-blocked, or approval-bearing work
  would be a correctness failure. Keep its partial status/time index and address
  presentation growth separately if the deployment model expands.
- Startup reconciliation separately queries failed jobs that retain a non-empty
  `HermesRunID`; those rows are cleanup obligations, not active user work, and
  their handles are cleared only after terminal upstream status is proven.
- Time-range ledgers need timestamp indexes. Additive indexes are rollback-safe:
  older application images ignore them.

## The event log is a retained decision corpus

`event_log` is not only a forensic trail. An operator authority decision paired
with the exact command and pattern scope it applied to is the record used to
improve the behaviour of the agents Thornhill dispatches, so that content is a
product asset and is retained indefinitely.

- Both automatic and operator authority decisions must publish the decision plus
  its approval evidence. Publishing a post-resolution job snapshot alone loses the
  decision, because resolution clears `Approvals`.
- Retention prunes only mechanical telemetry — progress ticks, usage, session
  state, hook mirrors, voice-transport errors. Never widen the sweep to job
  outcomes, questions, authority requests, or decisions, and never replace it with
  a blanket age cutoff.
- Nothing published on the bus or written to `event_log` may carry the one-use
  decision nonce. This includes running, failed, cancelled, orphan-reconciliation,
  collision, and approval-resolution snapshots. `store.Approval` keeps the `json`
  tag because that tag is also the `jobs.approvals` JSONB persistence format the
  broker re-reads to validate authority; redact per publication with `Redacted()`
  instead of dropping the tag.
- Analysis reads are kind-scoped and per-job, which is what the `(kind, ts)` and
  partial `(job_id, ts)` indexes exist for.

## Deployment and rollback

- The deploy drain and all transitions into `queued` or `running` use one
  PostgreSQL advisory-lock protocol. After acquiring it, deployment rechecks
  active work before stopping services.
- App and database containers are recreated together from the tested exact SHA.
  The persistent PostgreSQL volume survives container replacement.
- Every embedded schema edit must update `docs/rollback-compatibility.json`.
  Automatic promotion accepts additive backward-compatible changes only;
  incompatible migrations require an operator-controlled forward/restore plan.
- Production database credentials belong in the untracked deployment environment,
  not the Compose files. The environment file is deployment-user-owned and denies
  group/other access; the application independently validates the 64-character
  lowercase-hex transport form at startup.
- Existing-volume credential rotation is a durable maintenance transaction. Before
  stopping either service, the deployer atomically journals the old/new revisions,
  images, CI run, phase, and a password fingerprint (never the password), fsyncs
  the file, renames it, and fsyncs the state directory. Every destructive phase
  advances that journal. A later run reconciles it before the ordinary
  already-current fast path, conservatively treating the role as possibly rotated
  and converging forward or using the retained rollback artifacts. A changed
  secret, malformed phase, or missing artifact fails closed.
- Dispatch is unpaused only after the target or rollback public/local endpoints,
  application revision, database image, and configured database authentication all
  verify. Endpoint and container checks propagate each failure explicitly even when
  called from conditional retry loops. Recovery first verifies the durable dispatch
  pause and zero active jobs, then stops the application. A missing application
  container or stable exited candidate is an idempotent stop success; PostgreSQL
  retains the stricter stable exit-zero checkpoint requirement.
- Target completion durably removes the journal while dispatch is still paused; a
  crash in the final gap therefore leaves the latest healthy revision safely paused
  for the ordinary already-current path to heal. Rollback instead persists a
  `rollback_verified` marker with the previous/journaled credential model, unpauses,
  and only then removes the marker. A crash in that gap re-verifies the previous
  runtime, durably restores the failed-target quarantine, and consumes the marker
  without retrying the quarantined target. An unverified rollback deliberately
  leaves dispatch paused for operator recovery rather than reopening admission into
  an unknown runtime state.
- Before `ALTER ROLE`, the credential helper proves that the same disposable
  PostgreSQL client image can launch, resolve, and reach the database network.
  Runtime completion also compares the running application's password fingerprint
  with the host environment and independently authenticates from that client.
