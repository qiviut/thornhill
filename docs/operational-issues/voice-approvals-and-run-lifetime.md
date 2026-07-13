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

## Explicit approval with no elapsed decision

Silence is neither approval nor denial. A healthy approval stays pending until
one of these distinct outcomes occurs:

- the operator explicitly allows or denies it;
- the operator explicitly cancels or stops the session;
- the owning execution is interrupted or shuts down; or
- the control/transport operation fails.

Thornhill no longer wraps the whole Hermes run in a 4m30s timer, and its River
worker reports no elapsed runtime timeout. Hermes CLI, gateway, API, ACP, MCP,
Matrix, Discord, and QQ approval paths likewise do not synthesize a decision
from elapsed time. Gateway heartbeats keep a healthy pending wait active.

A decision does not make network operations unbounded. The post-decision
approval control request has a short transport deadline, and an indeterminate
authority response stops the run without retrying the grant. MCP similarly
excludes explicit elicitation-decision time from its tool transport deadline,
then resumes ordinary deadline accounting after the decision.

Exact one-use approval IDs/nonces, FIFO correlation, collision fail-closed
handling, and exact-pattern-set job/permanent policies remain intact. Platform
buttons may become unusable when their UI lifetime ends, but that only disables
the component; it does not decide the still-pending approval. Text approval and
deny commands remain available.

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

## Safe failed-job resume

Process restart still stops known in-flight Hermes runs fail-closed. Thornhill
preserves the job's error and last progress evidence rather than erasing the
uncertain outcome.

`resume_job` is allowed only for a failed job. It atomically claims the
failed-to-queued transition, so concurrent resume attempts cannot enqueue the
same job twice. It clears stale execution control fields and approvals while
retaining the durable job identity and Hermes session ID. The new run receives:

- the newest bounded user/assistant history from the durable Hermes session;
- the original task;
- the prior interruption and progress checkpoint; and
- an explicit instruction to verify current workspace/service state before
  repeating any potentially side-effecting operation.

This is reconciliation-first continuation, not blind replay. A successful
resumed run writes a new terminal result while retaining the prior error as
historical evidence; job status remains the authoritative current outcome.

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

## Validation procedure

Before shipping a revision:

1. Run `git diff --check`, `go vet ./...`, and `go test -race ./...`.
2. Run the bounded PR fuzz smoke, randomized PostgreSQL integration, and
   deterministic provider conformance tests.
3. Run `npm ci`, `npm run check`, and `npm run build` under `web/`.
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
