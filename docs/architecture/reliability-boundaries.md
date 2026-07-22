# Reliability and authority boundaries

Read this before changing job state, Hermes run ownership, approvals, browser
protocol parsing, or deployment. These decisions are deliberately load-bearing;
local simplifications can otherwise create duplicate work, implied consent, or a
deployment that cannot safely roll back.

## Durable job and run ownership

- PostgreSQL is authoritative. A job mutation and its River enqueue commit in
  one transaction; never reintroduce a database-then-enqueue crash window.
- A River delivery must durably claim a job and re-check cancellation before
  starting a Hermes run. Subsequent writes require the same run identity and an
  active status so an old stream cannot overwrite cancellation or a newer turn.
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
- Nonces, current run ownership, canonical choices, and FIFO approval order are
  broker-enforced. Operator intent is still conveyed through the model, so the
  UI must show the pending command/scope and the Desk prompt must continue to
  require explicit words. Do not describe this as cryptographic proof of consent.

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
- Time-range ledgers need timestamp indexes. Additive indexes are rollback-safe:
  older application images ignore them.

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
  not the Compose files. Credential rotation must update the persisted PostgreSQL
  role and the environment as one maintenance operation.
