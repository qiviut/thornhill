# Approval-resolution migration notes

Status: current runtime contract, 2026-08-22

This change hardens the existing Thornhill/Hermes Runs API boundary without
changing operator authority. It does not auto-approve, infer a decision from
elapsed time, or retry an ambiguous allow/deny call.

## Deployment order

1. Deploy or restart Hermes Agent first. Its Runs API must advertise
   `run_approval_idempotency` and `run_approval_request_correlation` in
   `/v1/capabilities`.
2. Restart Thornhill so its bridge reads the upgraded approval event and sends
   `request_id` plus `idempotency_key` on each decision.
3. Confirm a new approval event contains `request_id`, and that the board shows
   one pending local approval. Do not use a pre-upgrade browser action as the
   verification request.

The old Hermes endpoint remains backward-compatible for clients that omit the
new fields, but it cannot guarantee exact provider-request correlation or
idempotent replay. Do not claim the hardened contract while the capabilities
check is absent.

## State and data migration

No PostgreSQL schema migration is required. `provider_request_id` is an
optional field in the existing JSONB `jobs.approvals` value. Existing rows decode
with an empty provider ID and retain their existing one-use nonce and state.

The Hermes idempotency ledger is process-local and retained until the normal
terminal run-status TTL removes it. A Hermes process restart deliberately loses
that replay cache. A decision whose response was ambiguous across a restart
must remain `indeterminate`; resume the Thornhill job to inspect current state
and request fresh authority rather than reusing the old approval.

## Late or duplicate decisions

Hermes accepts an optional provider `request_id` and client `idempotency_key`.
The provider ID prevents a correlated decision from falling through to another
FIFO queue entry. The durable Thornhill approval ID is the client key. Repeating
the same choice with the same key returns the recorded acknowledgement without
touching the approval queue. Reusing the key for a different choice returns
`approval_resolution_conflict`.

A `409 approval_not_pending`, `approval_not_active`, or
`approval_request_mismatch` means the provider no longer has the requested
wait. It is not evidence of deny and not permission to resend the choice.
Thornhill reads the Hermes run status before stopping: a terminal run is not
stopped redundantly; a live run receives one bounded stop. The returned status
must carry the exact requested `run_id`; an absent or mismatched identity is
untrusted and keeps cleanup unconfirmed. Thornhill records an indeterminate
approval, preserves the operator's requested decision and redacted request
evidence, and shows recovery guidance. `resume_job` starts a fresh authority
boundary after verification; it does not replay the old ID or nonce. If status
or stop confirmation is unavailable, the durable run ID remains as a
reconciliation obligation.

A legacy stale `needs_input` run follows the same status-first cleanup proof:
terminal upstream runs are not stopped, live runs receive one bounded stop plus a
follow-up status read, and `HermesRunID` is cleared only after terminal proof. If
status or stop confirmation is unavailable, the durable run ID remains for
restart recovery instead of being discarded.

## Rollback

If the new bridge must be rolled back while upgraded Hermes remains live, old
clients can still omit the correlation fields and use the legacy FIFO endpoint,
but they lose duplicate-delivery protection. Do not roll back only one side
during an active approval incident without first parking or resolving visible
approvals through the current contract. If a Hermes restart or rollback has
already invalidated a wait, leave the Thornhill record indeterminate and resume
it with fresh authority.
