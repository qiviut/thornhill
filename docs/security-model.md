# Security model

This document defines the security posture Thornhill actually supports. It is the
starting point for security reviews and deployment decisions; [`SECURITY.md`](../SECURITY.md)
explains how to report a vulnerability, while [`ci-security.md`](ci-security.md)
describes the repository and delivery pipeline.

Thornhill supports a **single-operator voice desk inside an operator-only Tailnet
boundary**. It provides neither application authentication nor public or
multi-user isolation. Network reachability counts as operator identity only while
the supported deployment assumptions below remain true.

## Claim vocabulary

Security statements use these terms deliberately:

| Status | Meaning |
| --- | --- |
| **Enforced** | Code, configuration validation, or CI rejects a violating operation. |
| **Assumed** | The deployer must establish and preserve this condition outside Thornhill. |
| **Best effort** | The mechanism improves safety or observability but is not a hard guarantee. |
| **Deferred** | The control is appropriate only after a stated deployment or threat-model change. |
| **Not provided** | Thornhill does not currently make this security or reliability promise. |

## Supported deployment and trust boundary

The supported deployment has all of the following properties:

- **Assumed:** one human operator controls the desk.
- **Assumed:** Tailnet grants admit only that operator's intended devices to the
  Thornhill listener. A compromised or mistakenly admitted Tailnet device is
  therefore operator-equivalent at the application boundary.
- **Enforced by defaults and deployment policy:** Compose publishes on loopback
  unless `THORNHILL_BIND_ADDR` is deliberately set to a specific Tailscale
  address. Public listeners and Tailscale Funnel are outside the supported model.
- **Assumed:** the Linux account, host OS, Docker control plane, PostgreSQL
  administrator, and deployment secrets are trusted. A malicious process with the
  same effective host privileges is outside Thornhill's isolation boundary.
- **Assumed:** Hermes normally runs on loopback on the same trusted host. OpenAI
  and any configured remote Hermes provider are external processors entrusted
  with the data sent to them, subject to their own account and retention controls.

Thornhill performs **no application-level user authentication**. Tailnet policy is
its operator identity boundary. Browser `Origin` checks reduce cross-site request
and WebSocket risks; they do not authenticate a person or device. Deliberate
non-browser clients commonly send no `Origin` and are accepted where the route
inventory says so.

### When this model stops being valid

Add application authentication and per-principal authorization before any of the
following changes:

- another person, service, or untrusted device can reach the Thornhill listener;
- the listener is exposed outside an operator-only Tailnet grant;
- multiple operators need distinct history, approvals, quotas, or audit identity;
- browser sessions can be used from shared or untrusted machines.

Give hook producers a separate machine credential or listener if they cross the
trusted host/Tailnet boundary. Add pinned cryptographic Hermes identity only if
Hermes becomes remote or separately administered and its private key is protected
by a genuinely independent trust boundary.

## HTTP route inventory

This block is rendered from the same records used to register gateway routes.
`TestSecurityModelRouteInventoryMatchesCode` fails if code and documentation
drift. An Origin policy in this table is browser protection, never user identity.

<!-- route-security:begin -->
| Route | Expected caller | Enforced boundary | Data exposed or accepted | Authority |
| --- | --- | --- | --- | --- |
| `GET /api/status` | Tailnet-admitted observer | Tailnet perimeter | Health and exact source revision | Read-only |
| `GET /api/push/config` | Operator browser | Tailnet perimeter | Push enablement and public VAPID key | Read-only |
| `POST /api/push/subscriptions` | Operator browser | Tailnet plus exact same browser origin | Push endpoint and capability keys | Create or replace a notification capability |
| `DELETE /api/push/subscriptions` | Operator browser | Tailnet plus exact same browser origin | Push endpoint capability | Revoke a notification capability |
| `POST /offer` | Operator browser or deliberate non-browser client | Tailnet plus browser Origin policy; no-Origin clients accepted | SDP and current spend admission state | Create a billable provider call and replace the active desk |
| `GET /ws` | Operator browser or deliberate non-browser client | Tailnet plus browser Origin policy; no-Origin clients accepted | Recent events, transcripts, jobs, and client state | Inject operator intent and control the active desk |
| `GET /events` | Tailnet-admitted observer | Tailnet perimeter | Recent and live events, including transcripts and job state | Read-only observation |
| `POST /hooks/hermes` | Server-side hook or cron producer | Tailnet plus cross-origin browser rejection; no-Origin clients accepted | Size-bounded producer JSON | Append non-authoritative hook telemetry |
| `GET /audio/prebaked/` | Operator browser | Tailnet perimeter | Prebaked public application audio | Read-only |
| `GET /` | Operator browser | Tailnet perimeter | Static application shell and assets | Read-only |
<!-- route-security:end -->

## Control boundaries and current guarantees

### Browser and event input

- **Enforced:** OpenAI credentials remain server-side; `/offer` relays SDP without
  returning the API key.
- **Enforced:** browser JSON, WebSocket messages, and event payloads are parsed at
  explicit runtime boundaries before becoming typed application state.
- **Enforced:** cross-origin browser requests are rejected on state-changing
  routes according to the route inventory, and the UI is protected by a strict
  same-origin Content Security Policy and anti-framing policy.
- **Assumed:** any network client admitted by Tailnet policy is operator-equivalent
  unless a route applies an additional producer/browser constraint.

### Hermes hooks and job control

`POST /hooks/hermes` is generic hook/cron intake despite its historical path name.
It accepts size-bounded JSON and publishes `hermes.hook` telemetry.

- **Enforced:** the route does not dispatch jobs, answer questions, resolve
  approvals, or call Hermes authority endpoints. Structured Runs events are the
  job-control path.
- **Enforced:** cross-origin browser posts are rejected.
- **Assumed:** server-side/no-Origin producers have already passed the Tailnet or
  host boundary. There is no hook token or signature.
- **Best effort:** hook telemetry is queued to the event log and is subject to the
  mechanical-event retention window.

If no producer needs this route, disabling or removing it is preferable to
carrying unused intake surface.

### Approval intent, execution, and acknowledgment

These are three different guarantees:

1. **Model-mediated intent:** the Realtime model interprets the operator's spoken
   or typed words. The UI shows the pending command and pattern scope, and the
   Desk prompt requires explicit words. This is not cryptographic proof of human
   consent.
2. **Enforced authority execution:** canonical decision enums, a one-use nonce,
   current Hermes run ownership, FIFO approval order, exact eligibility checks,
   and atomic state transitions prevent stale or replayed decisions. Ambiguous
   authority calls stop the run rather than retrying permission.
3. **Best-effort acknowledgment:** the canonical decision is durably recorded,
   but the conversational confirmation is currently model-generated. Thornhill
   does not yet provide a deterministic server-rendered visual or delivered-audio
   receipt for a successful positive decision.

Questions, uncertainty, silence, parking, and safer-alternative requests are not
positive approval. Permanent scope requires direct confirmation before the
broker records it.

### Permanent approval policy

- **Enforced:** a permanent policy matches the complete normalized pattern-key
  set exactly; it is not a prefix or glob rule.
- **Enforced:** the configured Hermes base URL is included in that pattern set, so
  changing endpoints changes the policy namespace.
- **Assumed:** replacement of Hermes behind the same host-local URL remains within
  the trusted-host boundary. Thornhill does not pin a cryptographic Hermes
  instance identity.
- **Deferred:** add an explicit operator-controlled policy namespace and semantic
  epoch before permanent policy must survive Hermes migrations with distinct
  trust or pattern semantics. Cryptographic attestation is useful only with
  independent key custody.

### Spend and provider accounting

- **Enforced:** `DAILY_BUDGET_USD` compares the current UTC day's `est_usd` ledger
  total before admitting a new Realtime call.
- **Not provided:** this is not currently an effective spend cap. Application-
  produced Realtime and summary usage rows record `est_usd = 0`; modality,
  caching, and transcription costs are not fully accounted. A configured positive
  budget therefore does not trip from normal recorded usage.
- **Not provided:** even accurate post-response accounting alone would not be a
  strict reservation or concurrency-safe provider budget. Hard spend containment
  also needs call reservation/duration/concurrency controls or an upstream account
  budget.

Do not describe or operate `DAILY_BUDGET_USD` as a working cost ceiling until both
accounting and the desired cap semantics are implemented and tested.

### Dependency and delivery posture

- **Enforced:** pull-request CI is secretless and read-only; privileged Dependabot
  approval/merge lanes do not check out PR code and independently bind actor,
  repository, branch, and exact head SHA.
- **Enforced:** branch protection and post-merge CI qualify the exact landed SHA;
  deployment independently verifies trusted CI and deploys that exact revision.
- **Enforced:** workflow Actions are immutable-SHA pinned and repository Actions
  are allow-listed.
- **Assumed risk acceptance:** every green Dependabot update is currently eligible
  for unattended promotion without a package-authority/provenance classifier.
  Tests strongly reject known behavioral and policy failures, but cannot prove an
  executable dependency or publisher is benign.
- **Deferred:** route high-authority Actions, images, toolchains, scanners, and
  sensitive runtime dependencies through provenance/human review while retaining
  the low-risk fast path.

[`ci-security.md`](ci-security.md) is authoritative for detailed workflow and
branch-protection controls.

## Durable data, retention, and recovery

The PostgreSQL volume is Thornhill's durable system of record. The application
contains the following data classes:

| Data class | Contents | Application retention or deletion |
| --- | --- | --- |
| Jobs | Task, status, questions/answers, result or error, Hermes session/run IDs, pending approval state and progress | Retained indefinitely; no application delete path |
| Permanent approval policies | Exact-set policy hashes, source job, creation time | Retained indefinitely; no application list/revoke/expiry path |
| Deployment control | Dispatch pause state and update time | Single row updated in place |
| Retained events | Transcripts, job outcomes, questions, authority requests and decisions with redacted evidence | Retained indefinitely as the decision and learning corpus |
| Mechanical events | Progress/running ticks, usage events, session state, hook mirrors, voice errors | Pruned after `EVENT_RETENTION`, 30 days by default |
| Summaries | Rolling and debrief model-generated summaries | Scope rows overwritten in place; no age deletion |
| Attention events | Pending/delivered speech and push notification content | Retained indefinitely; no age deletion |
| Push subscriptions | Push endpoint plus `p256dh` and `auth` capability material | Deleted on explicit unsubscribe; invalid capabilities may be disabled but retained |
| Push deliveries | Notification body, attempts, delivery/failure state | Deleted when its subscription is deleted; otherwise no age deletion |
| Usage ledger | Source, input/output token totals, estimated cost | Retained indefinitely; no age deletion |
| River-owned queue tables | Queue arguments and execution metadata managed by River | River lifecycle defaults; not governed by `EVENT_RETENTION` |

One-use approval nonces are stored only in the current job approval state needed
for broker validation. They are cleared from bus publications and the append-only
event corpus.

### Backup status

- **Enforced:** the named PostgreSQL volume survives ordinary container
  replacement and exact-SHA deployment rollback.
- **Not provided by this repository:** protection against host/disk loss, volume
  deletion, database corruption, or operator error. There is no checked-in backup
  job, off-host destination, RPO/RTO, rotation policy, or tested restore runbook.
- **Unknown:** a deployer may operate host-level backups outside this repository;
  repository documentation cannot claim or verify them.

If this retained corpus is an asset, use encrypted off-host backups with
independent key/access control and test restoration into an isolated compatible
PostgreSQL instance. Backups duplicate sensitive task, transcript, authority, and
push data, so their retention and deletion schedule are part of the privacy model.
If loss is intentionally acceptable, document that accepted-loss boundary instead
of calling the persistent volume a backup.

## Reviewer checklist

Before reporting a security issue, state which assumption is accepted or broken:

- Can a non-operator principal reach the Thornhill listener under actual Tailnet
  grants?
- Is the behavior an application authority path, non-authoritative telemetry, or
  browser cross-origin protection?
- Does the claim concern model interpretation, deterministic broker execution, or
  operator acknowledgment?
- Is the attacker outside the trusted host account, or already equivalent to the
  deployment/database administrator?
- Is a control enforced today, merely assumed, best effort, deferred, or explicitly
  not provided?

A finding that requires a shared/public deployment can still be useful, but should
be framed as an invalidated deployment assumption rather than an authentication
bypass in the supported single-operator model.
