# Voice, approval, and long-run reliability

Status: implemented in the current reliability release. The authoritative
shipment proof is the successful GitHub Actions run and the revision reported by
`/api/status`; use the validation procedure below rather than trusting this
narrative alone.

## Problem statement

Three live symptoms shared one architectural fault: conversational transport,
execution lifetime, and authorization lifetime were coupled.

- Concurrent Realtime `response.create` calls produced
  `conversation_already_has_active_response` and could interrupt a spoken/tool
  turn.
- Browser Park closed WebRTC immediately, even while remote audio was still
  draining.
- Thornhill imposed a 4m30s whole-run deadline because Hermes approval routing
  previously expired around five minutes. Long healthy jobs and approval waits
  could therefore be stopped solely because time elapsed.
- Restart reconciliation stopped stale Hermes runs but left no operator-facing
  resume path.
- Approval prompts presented every durable scope at once, while ad-hoc shell or
  interpreter scripts created avoidable approval friction.

The design now treats voice as a disposable view of durable job state, keeps
consent explicit and non-expiring, and makes recovery verification-first.

## Realtime response and tool invariants

Thornhill's Desk is the only component that emits `response.create`.

- Semantic VAD uses `create_response: false` and
  `interrupt_response: false` in the atomic WebRTC call configuration before
  microphone media can arrive, then preserves both in the sideband update.
  Speech boundaries are observability and turn signals, not implicit response
  creation or cancellation.
- A response request is marked in-flight before it is sent. A voice turn becomes
  eligible only after Realtime confirms `input_audio_buffer.committed`, so
  `response.create` cannot race ahead of the new conversation item. User turns,
  tool continuations, and announcements coalesce behind the active response,
  output audio, user speech, and tool work becoming idle.
- Response admission and Park share one Desk serialization lock, while the FSM
  atomically checks LIVE/QUIET readiness and marks the response in-flight. A
  Park request therefore either follows an already-written `response.create` or
  prevents that response from being admitted.
- An active-response error is reported as a voice transport signal but does not
  queue an audible error response, which would otherwise repeat the collision.
  Each `response.create` carries a unique client `event_id`. Only an asynchronous
  error naming that exact ID reconciles its provisional in-flight state, so an
  unrelated rejected session or conversation event cannot cause a duplicate
  response; correlated rejection cannot wedge the session or Park lifecycle.
- Function calls execute only when the containing response and the individual
  function-call item are both `completed`. Streamed or cancelled partial calls
  cannot cause side effects. Completed batches execute concurrently off the
  Realtime event loop, preserving speech/audio observability while slow tool
  work runs; outputs are still submitted serially in response order and active
  operator speech keeps the continuation pending.
- Multi-call output preserves response order. Every output retains the original
  Realtime `call_id` and uses a versioned JSON envelope containing `schema`,
  `tool`, `call_id`, and structured `result`. Desk appends all outputs before
  creating one continuation response.
- Response lifecycle and WebRTC output-audio lifecycle are tracked separately;
  `response.done` alone is not evidence that the operator has heard all audio.

Durable jobs and their text/event history are not cancelled by a voice-layer
error, false speech detection, browser Park, or WebRTC teardown. Voice offer
negotiation is bounded, and ending a disposable Desk cancels any remaining
front-desk helper context without cancelling dispatched durable jobs.

Event fanout remains immediate, while durable event-log writes use one bounded,
ordered queue so slow PostgreSQL writes do not stall ordinary producers. Session
state transitions use the same queue with an acknowledgement fence; they cannot
overtake earlier accepted events. Process shutdown first cancels and waits for
the owned Desk, then stops River, drains accepted event persistence, and closes
PostgreSQL.

## Truthful Park lifecycle

Explicit Park is a drain request, not an immediate disconnect:

1. Desk publishes `PARKING`.
2. The browser displays `DRAINING AUDIO`, disables new microphone input, and
   keeps WebRTC plus remote audio attached.
3. Desk waits for response generation, remote output audio, user speech, and
   tool output to become idle. If disposable Realtime state fails to drain
   within 30 seconds, Desk cancels that response and completes Park without
   cancelling durable jobs.
4. Desk publishes server `PARKED`.
5. The browser keeps displaying `DRAINING AUDIO` while it observes a 300 ms
   quiet window from the remote RTP receiver, with a 2.5-second safety bound.
6. Only then does the browser display `PARKED`, close the peer connection, stop
   microphone tracks, and clear the remote audio media source.

A generation check and state publication share one gateway critical section, so
a superseded Desk cannot publish stale `PARKED` over a newer live call,
and a stale browser drain completion cannot close a replacement call.
Background Hermes jobs continue throughout parking. Reopening creates a fresh
Realtime call reconstructed from durable job, event, and debrief state; it does
not attempt byte-identical restoration of an old media stream.

## Explicit approval without inferred elapsed decision

Silence is neither approval nor denial. A pending authority record remains
unresolved until the operator explicitly decides it. Execution resources do not
need to remain attached to that record indefinitely: `APPROVAL_PARK_AFTER`
(default `15m`) is a resource-reclamation threshold, not a decision deadline.

When the threshold expires, Thornhill atomically changes the exact
`needs_approval`/`pending` record to `parked_approval`/`parked`, preserving its
redacted request evidence, pattern scope, original request time, parking time,
and reason. Only after that durable transition does it request that the Hermes
run stop, close the event stream, return the River worker, and release its
in-memory run and cancellation slots. It sends no allow or deny control
request. A confirmed upstream stop clears the retained run ID. If stop cannot be
confirmed, bounded detached retries and startup/explicit-resume reconciliation
use that durable ID; no River worker or open SSE is retained for cleanup.

A decision claim and a parking claim race under the same PostgreSQL row lock and
one-use approval ID/nonce. Exactly one may win. After parking, the old ID/nonce
is stale and cannot be resolved or replayed. If the durable transition fails,
Thornhill retains the pending record and resource owner for retry/restart
reconciliation rather than pretending reclamation or a decision succeeded.

Process restart applies the same rule to a sole durable pending request: it parks
the request, stops the known upstream run, and lets stale River redelivery finish
without starting work. A request already in `sending` cannot be reconstructed as
pending; it becomes failed with an indeterminate authority outcome instead.

Thornhill no longer wraps the whole Hermes run in a 4m30s timer, and its River
worker reports no elapsed runtime timeout. Hermes CLI, gateway, API, ACP, MCP,
Matrix, Discord, and QQ approval paths likewise do not synthesize a decision
from elapsed time. Before the parking threshold, gateway heartbeats keep a
healthy pending wait active.

A decision does not make network operations unbounded. The post-decision
approval control request has a short transport deadline, and an indeterminate
authority response stops the run without retrying the grant. MCP similarly
excludes explicit elicitation-decision time from its tool transport deadline,
then resumes ordinary deadline accounting after the decision.

Exact one-use approval IDs/nonces, FIFO correlation, collision fail-closed
handling, and exact-pattern-set job/permanent policies remain intact. Platform
buttons may become unusable when their UI lifetime ends, but that only disables
the component; it does not decide the unresolved approval. Text approval and
deny commands remain available while the request is pending; a parked request
must instead be resumed and reissued with fresh authority if still needed.

## Progressive approval and safer alternatives

The primary voice question offers four understandable actions:

- allow this once;
- deny this once;
- hear details; or
- ask the agent to use a safer alternative.

Job-wide and permanent scopes are mentioned only when the operator asks for a
broader policy. Permanent allow still requires explicit confirmation of the
full reusable scope. `use_safer_alternative` denies only the correlated
mechanism; it never grants authority or denies unseen concurrent requests. The
job records that intent and sends Hermes the typed `safer_alternative` outcome.
Hermes keeps the concrete mechanism blocked while permitting only a native or
managed alternative within existing authority, never a disguised retry.

Dispatched agents prefer native Hermes file/search/patch/browser/GitHub/MCP
tools and direct, bounded executable invocations over opaque shell pipelines.
When a script is genuinely needed, creation and debugging should be delegated
when practical. The script must be either:

1. a named, documented, validated reusable asset in the target repository's
   managed scripts directory; or
2. a task-scoped temporary artifact removed before completion.

This is approval-friction reduction, not an authority bypass. Hard blocks and
exact reusable approval scope are unchanged.

## Safe failed-job and parked-approval resume

Process restart still stops known in-flight Hermes runs fail-closed. Running work
preserves the job's error and last progress evidence rather than erasing the
uncertain outcome. A job already parked in `needs_input` retains its durable
question and can consume a persisted answer after restart; a legacy stale run ID
is stopped and conditionally cleared first. A sole pending approval is parked
unresolved as described above; restart never converts it to allow or deny.

`resume_job` atomically claims either a `failed` or `parked_approval` job into
`queued`, so concurrent resume attempts cannot enqueue the same job twice. It
clears stale execution control fields. A failed job clears any stale approval
record; a parked job retains its durable evidence just long enough for `Run` to
build the recovery brief, then clears the stale ID/nonce before starting a new
Hermes run.

The new run receives:

- the newest bounded user/assistant history from the durable Hermes session;
- the original task;
- the prior interruption and progress checkpoint;
- for a parked request, quoted untrusted command/description/pattern/timestamp
  evidence without the old approval ID or nonce; and
- explicit instructions to verify current state, infer no permission from the
  parked request, and issue a fresh approval record if the action remains needed.

This is reconciliation-first continuation, not blind replay. A successful
resumed run writes a new terminal result while retaining a failed job's prior
error as historical evidence; job status remains the authoritative current
outcome. If queue submission fails, a parked job rolls back to
`parked_approval` with its evidence intact rather than becoming an opaque failed
job.

## Durable attention across parked sessions

The rolling summary is context, not an acknowledgment ledger. Every transition
to `done`, `failed`, `needs_input`, `needs_approval`, or `parked_approval` inserts
an immutable attention row in the same PostgreSQL transaction as the job update.
A unique `(job_id, state_version, kind)` key makes replay idempotent.

On call start, one Desk instance leases unspoken rows with `SKIP LOCKED`, injects
their text under an explicit quoted/untrusted-data envelope, and asks the model
to brief the operator. The `response.create` client event ID is copied into
Realtime response metadata. A row becomes spoken only when that exact response
is `completed` and its exact `response_id` has both started and fully drained
audio. Interruption, buffer clear, text-only output, disconnect, error, or stale
callbacks never acknowledge the row. While the call survives, the exact bounded
briefing is requeued behind the active response; disconnect releases the claim so
the next call can brief it again.

Optional Web Push consumes the same attention rows through a separate durable
outbox. Delivery is suppressed while the voice desk is live; a transition to
live cancels an in-flight provider request and releases its database lease.
Parked/absent sessions retry network failures, `429`, and `5xx` responses with
bounded backoff for at most six attempts. Other permanent responses are recorded
as terminal failures. Push titles and bodies are generic, subscription endpoints
are write-only bearer capabilities, and notification egress resolves and dials
only public addresses without proxies or redirects. Notifications never grant
authority or mutate jobs. Explicit unsubscribe deletes the endpoint before
removing the browser capability; provider `404`/`410` revocation retains only a
disabled audit row.

## Known limits

- There is no byte-for-byte media continuation after WebRTC teardown.
- There is no generic application-level checkpoint protocol for pausing arbitrary
  external resources; failed-job resume relies on durable history, artifacts,
  progress evidence, and verification before repeat.
- Public OpenAI documentation and model catalogs do not establish a
  `gpt-live-1` model. The documented Realtime family remains the supported
  integration target unless authenticated account visibility and full protocol
  acceptance prove otherwise.
- UI component expiry is platform behavior, not consent expiry; operators may
  need to use a text command after a button is disabled.
- Web Push delivery is platform/provider best effort, not a completion receipt.
  iOS/iPadOS requires a Home Screen-installed PWA before permission can be
  requested; voice resume remains the durable fallback.

## Validation procedure

Before shipping a revision:

1. Run `git diff --check`, `go vet ./...`, and `go test -race ./...`.
2. Run the bounded PR fuzz smoke, randomized PostgreSQL integration, and
   deterministic provider conformance tests.
3. Run `npm ci --ignore-scripts`, `npm run check`, `npm run lint`, `npm test`,
   `npm run build`, and `npm audit --audit-level=high` under `web/`.
4. Build both container images; verify the app image label and binary version
   contain the candidate source revision.
5. Push the complete code, test, documentation, and issue-ledger commit and wait
   for the exact GitHub Actions push SHA to pass.
6. Deploy only that passing SHA through `scripts/deploy-passed-main.sh`.
7. Verify local and Tailnet `/api/status`, running OCI revision label, in-container
   binary version, deployment receipt, health/readiness, and a real Hermes job.
8. Exercise a real Realtime call for serialized multi-tool continuation, audible
   Park drain, reconnect, explicit approval resolution, and durable-job survival.
9. Run the deployer in `CHECK_ONLY=1` mode and confirm the systemd timer records a
   clean already-current no-op.

Do not claim shipment from local tests alone. The green Actions SHA, deployed SHA,
status endpoint, image label, binary version, and receipt must all agree.
