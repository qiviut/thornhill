# Thornhill

Single-operator voice desk for a Hermes Agent fleet. A `gpt-realtime-2.1`
front desk takes voice, dispatches work to Hermes Agent instances (backed
by OpenAI models), relays their questions, and announces results — the
GPT-Live delegation pattern, self-hosted, with an open agent harness in
the back office.

```
Browser ══ WebRTC audio ═══════════► OpenAI Realtime (gpt-realtime-2.1)
   │                                          ▲
   └─ WS control → Gateway ══ sideband WS (call_id)
                      │  ▲
                 Event Bus ◄──── webhooks (Hermes hooks/cron)
                      │  │
                     UI  Dispatcher (Postgres + River)
                          │
                    Agent Bridge ── Runs API + SSE ───────► Hermes Agent
                                                          (fleet VM → OpenAI)
```

One OpenAI API key, held server-side only. The browser posts its SDP
offer to `/offer`; the gateway relays it to `POST /v1/realtime/calls`,
captures the `call_id` from the `Location` header, and attaches a
sideband WebSocket to run the session. No ephemeral key minting, no key
in the browser. Audio flows browser↔OpenAI directly.

## Run it

```sh
cp .env.example .env   # set OPENAI_API_KEY
# Default host exposure is loopback. For an intentional tailnet deployment:
# THORNHILL_BIND_ADDR=100.x.y.z docker compose up --build -d
docker compose up --build -d
# open http://127.0.0.1:8787 — tap the ring
```

With `HERMES_BASE_URL` unset, a stub worker fakes a 90-second job
(including one clarifying question halfway) so the entire voice loop —
dispatch, needs_input relay, completion announcement, park/resume — can
be exercised with only an OpenAI key. Point `HERMES_BASE_URL` at a
Hermes Agent API server ([docs](https://hermes-agent.nousresearch.com/docs))
to go live.

Microphone capture requires a browser secure context. For a tailnet deployment,
keep the app on loopback and terminate HTTPS with Tailscale Serve rather than
opening it directly over plain HTTP:

```sh
# If Hermes also remains loopback-only on this Linux host, set:
# HERMES_BASE_URL=http://127.0.0.1:8642
# Then use the host-integration override so the app can reach Hermes while
# Postgres remains containerized on a high loopback port.
docker compose -f docker-compose.yml -f docker-compose.host.yml up --build -d
tailscale serve --https=8787 --bg http://127.0.0.1:8787
# open https://your-host.your-tailnet.ts.net:8787
```

Dev mode: `make run` (Go on :8787) plus `make web` (Vite with HMR).
The production UI is same-origin by default. For Vite development, set
`ALLOWED_ORIGINS=localhost:5173` (or a similarly narrow explicit origin) in
`.env`; cross-origin WebSockets and SDP offers are rejected otherwise.

### CI-proven live revision

Production images embed the full Git commit and expose it at `GET /api/status`.
A normal local build deliberately reports `unversioned`; it must not be confused
with a CI-corresponding deployment.

On the Thornhill host, install the user timer once:

```sh
PUBLIC_APP_URL=https://your-host.your-tailnet.ts.net:8787/ \
PUBLIC_STATUS_URL=https://your-host.your-tailnet.ts.net:8787/api/status \
  ./scripts/install-ci-autodeploy.sh
```

The timer polls GitHub for the newest successful **push-to-main** CI run and builds
that exact commit from a detached temporary worktree. Immediately before
replacement it atomically pauses new dispatches in PostgreSQL and rechecks that
no work is active. It then recreates only the app service and verifies the local
and Tailnet UI, status endpoint, OCI label, and binary revision. Failed
verification restores the prior revision's image **and Compose model**; the bad
SHA is quarantined instead of being retried every two minutes. The current
correspondence is independently checked with:

```sh
CHECK_ONLY=1 ./scripts/deploy-passed-main.sh
curl -fsS https://your-host.your-tailnet.ts.net:8787/api/status | jq .
```

The durable receipt is stored at
`~/.local/state/thornhill-ci-deploy/deployed.json`; it includes the source SHA and
GitHub Actions run URL.

## Verification and maintenance

```sh
gofmt -w .
go vet ./...
go test -race ./...
go tool staticcheck ./...
go tool govulncheck ./...
go tool actionlint .github/workflows/*.yml
FUZZTIME=5s scripts/test-fuzz.sh
go test -tags=integration -run '^TestProviderProcessConformance$' ./internal/dummyopenai
(cd web && npm ci --ignore-scripts && npm run check && npm run lint && npm run build && npm audit --audit-level=high)
docker buildx build --check .
docker buildx build --pull --load --build-arg THORNHILL_REVISION=0123456789abcdef0123456789abcdef01234567 --tag thornhill:local .
docker buildx build --pull --load --file Dockerfile.postgres --tag thornhill-postgres:ci .
scripts/test-container-hardening.sh thornhill:local thornhill-postgres:ci
scripts/test-postgres-integration.sh
scripts/run-security-scans.sh thornhill:local thornhill-postgres:ci
scripts/check-ci-policy.sh
```

GitHub Actions runs formatting, Go and web static analysis, known-vulnerability
and secret scans, race-enabled tests, short authority/protocol fuzz campaigns,
an ephemeral deterministic model-provider process, both final container builds,
runtime-hardening checks, randomized PostgreSQL migration/concurrency tests,
Compose validation, image scans, and CycloneDX SBOM generation on every push and
pull request. Its Go and Node setup versions are derived from the Dockerfile, so
Docker updates are tested with the same toolchains they change. A weekly workflow
gives every fuzz target a longer campaign and preserves minimized failures.
Dependabot checks Go tools/modules, npm packages and Biome rules, Dockerfiles,
production and scanner Compose images/rule engines, and GitHub Actions daily. A
privileged `workflow_run` lane may approve only an open, same-repository
`dependabot[bot]` PR to `main` at the exact SHA that read-only CI passed; it
neither checks out PR code nor merges it.

PR CI is intentionally secretless. The checked-in branch-protection policy,
publication procedure, future trusted-promotion rules, randomized-test policy,
and dummy-provider scope are documented in [docs/ci-security.md](docs/ci-security.md).
Container design, scanner scope, update coverage, and the primary-source review
are documented in [docs/container-security.md](docs/container-security.md).

## Session lifecycle

PARKED is the canonical state; a live call is a disposable
materialization of it.

```
LIVE ──30s no speech──► QUIET ──park request / idle──► PARKING ──drained──► PARKED
  ▲                       │ speech                    │ response/audio/tool      │
  └───────────────────────┘                           └──────── waits ───────────┘
                                                        tap → new call ─────────▲
```

`PARKING` is visible in the browser as `DRAINING AUDIO`. The browser disables
new microphone input but keeps the peer connection and remote audio attached
until the server confirms that response generation, output audio, user speech,
and tool work are idle. A 30-second safety bound cancels only the disposable
Realtime response and completes Park if a lifecycle event or front-desk tool
wedges; durable Hermes jobs continue. After server confirmation, the browser
additionally observes the remote RTP audio level for a quiet window (bounded to
2.5 seconds) before it displays `PARKED` and tears down WebRTC; a stale
completion cannot close a replacement call.
Guards include a 2-minute grace
after a job question is voiced. Mute alone is announce-only mode (the
desk can still speak); mute+hidden or 10 minutes of mute parks. The
57-minute rollover (60-minute API cap) parks and auto-redials —
invisible in practice. A rolling "since you left" digest is folded
continuously by a cheap model, so resume seeding is one Postgres read.

## Realtime tool protocol

Thornhill owns response creation: the atomic call configuration disables the
provider defaults with `create_response: false` and `interrupt_response: false`
before microphone media can arrive, and the sideband update preserves that
policy. Semantic VAD only reports speech boundaries. User turns and
tool continuations are queued until the active response and output audio are
idle, so only one `response.create` is in flight. Output-audio started/stopped
events are tracked separately from response completion for interruption and
parking observability. Response admission and Park are serialized, and every
`response.create` carries a unique `event_id`; only an error correlated to that
ID can reconcile the provisional in-flight state.

Function calls execute only when both their containing response and output item
are `completed`; streamed partial arguments never cause side effects. Multiple
calls preserve response-output order. Each `function_call_output` uses the
original Realtime `call_id` and contains a JSON envelope with `schema`, `tool`,
`call_id`, and structured `result`, followed by one serialized continuation.
Tool batches execute concurrently off the Realtime event loop, while their
outputs are submitted serially in response order. Speech/audio lifecycle events
remain observable during slow durable operations; a user turn that starts
during tool work holds the continuation until its committed item is available.

## Conversational approvals

Live Hermes jobs use `/v1/runs` and subscribe to structured tool and approval
SSE events. When a run asks for authority, Thornhill persists the redacted
request, highlights it on the board, and proactively injects it into the live
Realtime conversation. If voice is parked, a prebaked spoken alert asks the
operator to resume; the pending request is announced immediately on resume.

The operator may ask questions before deciding. The desk model translates
natural speech into one typed choice at the authority boundary. Its primary
prompt is **allow once**, **deny once**, **details**, or **use a safer
alternative**; broader job/permanent allow or deny scopes are offered only when
the operator asks for them. Questions, silence, ambiguous scope, and transport
errors never grant or deny authority. A healthy pending request has no decision
deadline; only an explicit typed decision, explicit cancellation/session
interruption, or process failure ends the wait. Session choices apply to the
exact approval pattern set for the current Thornhill job. Reusable allows and
denies are exact-pattern-set policies owned by Thornhill and scoped to the
configured Hermes endpoint; Hermes receives only a one-time grant for each
concrete request. Permanent allow still requires explicit confirmation of its
pattern-wide scope.

The broker accepts a decision only for the sole pending request and validates a
one-use approval ID/nonce. A second concurrent request triggers deny-all and a
fail-closed stop. An allow POST is sent exactly once; an indeterminate response
is never retried and stops the run. Approval heartbeats keep healthy Hermes and
gateway activity leases alive without turning elapsed time into a decision.
The River worker likewise has no elapsed runtime deadline; explicit cancel,
shutdown, and execution failure still stop work.

## Job recovery and script lifecycle

Browser parking never cancels durable Hermes jobs. After a Thornhill restart,
known upstream runs are stopped fail-closed and their jobs are marked failed.
`resume_job` reuses the durable job identity and Hermes session history, while
preserving the prior error and last tool-progress checkpoint in a verification
brief. Resume is an atomic failed-to-queued claim, so duplicate or concurrent
resume attempts cannot enqueue the same job twice. The resumed agent must
inspect current state before repeating any potentially side-effecting operation;
stale approval requests are not replayed. Recovery retains the newest bounded
session history rather than the oldest messages that happen to fit, and the
prior error remains durable even after a successful resumed completion (job
status remains the authoritative current outcome).

Dispatched agents prefer native Hermes tools over opaque shell pipelines. If a
script is necessary, creation and debugging should be delegated when practical.
The script must either become a named, documented, validated asset in the target
repository's managed scripts directory or remain task-scoped and be removed
before completion. Unexplained ad-hoc scripts are not an acceptable residue.

## Audible errors (no screen required)

- **L0** session alive: errors injected as conversation items; the model
  voices them.
- **L1** realtime dead, backend alive: prebaked OpenAI TTS phrases
  (generated at startup into `PREBAKE_DIR`) played via the control WS.
- **L2** backend dead: browser-local earcons (descending = link lost,
  ascending = restored, triple blip = job done, low buzz = failed) driven
  by a heartbeat watchdog.

## Layout

```
cmd/thornhill        wiring
internal/gateway     /offer SDP relay, control WS, SSE, static
internal/desk        session FSM + tools + announcement queue (prompt.md)
internal/openairt    realtime sideband client (GA event names)
internal/dispatch    job lifecycle, River queue, stub runner
internal/bridge      Hermes Runs API + approval broker + hooks receiver
internal/summarize   rolling debrief
internal/audio       TTS prebake + dynamic speech
internal/events      in-process bus with replay
internal/store       Postgres: jobs, event log, summaries, usage
web/                 Vite + React UI (state ring, board, ticker, earcons)
docs/vendor/         the exact API docs this code was written against
```

## Verify on first contact (marked TODO(verify) in code)

1. Hermes hook payload shape (`/hooks/hermes` ingests anything, tags it
   `hermes.hook`).
2. `TRANSCRIBE_MODEL` and `TTS_MODEL` names against current docs.
3. Truncation field path in `session.update`
   (`truncation.retention_ratio`, per realtime-costs.md).
4. Needs-input intent remains a trailing-question heuristic; approvals and
   tool progress are structured Runs events.

## Notes

- Tailnet-only by deployment: bind `LISTEN_ADDR` (or the compose port
  mapping) to the host's tailscale address. The gateway itself does no
  auth — the tailnet is the perimeter, and there is exactly one user.
- Budget breaker: `DAILY_BUDGET_USD` gates new calls on the day's
  estimated spend from the usage ledger (estimates currently logged at
  zero cost; wire rates from docs/vendor/openai/pricing.md when it
  matters).
- A process restart fail-closes in-flight Hermes work: Thornhill stops known
  run IDs, marks running/input/approval jobs failed, and cancels their River
  redelivery. Queued jobs remain eligible to start normally; failed jobs can be
  resumed through the verification-first recovery path described above.

## Contributing and security

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the required local checks and
secretless contribution rules. Report vulnerabilities privately as described in
[`SECURITY.md`](SECURITY.md).

## License

Thornhill is licensed under the [GNU Affero General Public License v3.0
only](LICENSE) (`AGPL-3.0-only`). The browser interface links to this public
source so network users can obtain the corresponding source. Modified network
deployments must keep that offer pointed at the source for the version they run.
