# CI trust and publication policy

## Required public-repository baseline

The checked-in policy in `.github/branch-protection.json` is the day-one public-repository baseline for `main`:

- require the stable `Go, web, and image build` check against an up-to-date branch;
- enforce the rule for administrators;
- require pull-request conversation resolution and linear history;
- prohibit force pushes and branch deletion;
- require a pull request, but use zero mandatory approvals while Thornhill is a single-maintainer project.

`./scripts/check-ci-policy.sh` prevents the workflow job name and policy from silently diverging. `./scripts/apply-branch-protection.sh` fails closed while the repository is private, because a successful pre-publication no-op could leave a newly public `main` unprotected. The current private-plan limitation can be acknowledged explicitly without changing policy:

```bash
./scripts/apply-branch-protection.sh --defer-private
```

Run the fail-closed form immediately after publication and require its verified protection summary before accepting public contributions:

```bash
./scripts/apply-branch-protection.sh
```

Increase `required_approving_review_count` when a reliable second reviewer exists; do not install a permanently unfulfillable review rule.

## Trust lanes

### 1. Pull-request qualification: untrusted and secretless

`CI` executes contributor code and dependency build scripts with only `contents: read`. It must never reference repository, environment, Dependabot, cloud, tailnet, model-provider, signing, or deployment secrets. It establishes whether one immutable source revision is eligible for promotion; it does not deploy or sign.

The required check exercises:

- a Gitleaks scan over the fetched Git history before builds execute;
- Go formatting, vetting, race tests, and short invariant-driven fuzz campaigns;
- an ephemeral deterministic OpenAI-compatible provider process;
- web dependency installation, checks, and production build;
- application and PostgreSQL image builds;
- migrations and concurrent approval claims against an ephemeral PostgreSQL container whose username, database, password, container name, host port, and test data are freshly generated from cryptographic randomness;
- the Compose delivery model and this policy itself.

### 2. Promotion: branch protection decides trust

Only the exact revision that passed the required check may merge to `main`. Dependabot approval automation validates the source repository, actor, base branch, head branch, and exact CI-tested SHA and never executes PR code in its write-capable workflow.

### 3. Secret-bearing canaries or deployment: trusted revision only

If a live OpenAI/Hermes canary or deployment workflow is added, trigger it from a completed successful `CI` run and fail closed unless all of these are true:

1. the source run event was `push`, not `pull_request`;
2. the source repository is exactly this repository;
3. the branch is exactly `main`;
4. the source run SHA still equals the protected `main` SHA selected for promotion;
5. the privileged workflow definition comes from the protected default branch;
6. credentials come from a narrowly scoped GitHub environment, with the smallest token permissions and concurrency limits.

Check out and rebuild the exact trusted SHA in the privileged lane. Do not execute mutable PR artifacts, restore contributor-controlled caches, or inherit a PR workspace. If an artifact crosses the boundary, first bind it to the source SHA and verified digest/provenance and treat it as data until revalidated.

No secret-bearing GitHub lane exists today. Promotion remains host-local, so GitHub never receives Docker, host, Tailnet, or application credentials. This is preferable to adding a privileged deployment workflow before its credential and runner boundaries exist.

### Local trusted deployment correspondence

The host-side `thornhill-ci-deploy.timer` is the promotion boundary. It has no
PR trigger and never executes a pull-request checkout. It selects the newest
successful `push` CI run for `main`, verifies that SHA is an ancestor of current
`origin/main`, builds from a detached worktree at that exact revision, and
injects the full SHA into the binary and OCI image label. Once the build is
ready, a PostgreSQL row lock atomically pauses job creation; the deployer then
rechecks that no queued or active work exists before replacing the app.

Both local and Tailnet UI/status probes, the running OCI label, and the in-container
binary must agree. Failure restores the prior image using the prior revision's
Compose model and verifies the rollback. A revision that fails host verification
is recorded in `failed.json` and suppressed until a newer passing SHA arrives or
an operator explicitly sets `RETRY_FAILED=1`.

This preserves a directly inspectable chain:

```text
GitHub push CI run → head SHA → detached source worktree → OCI revision label
  → linked binary commit → live /api/status → deployed.json receipt
```

The timer deliberately polls from the host rather than giving GitHub a Tailnet
or Docker credential. The externally reachable UI and status URLs are required
host-local service environment values (`PUBLIC_APP_URL` and
`PUBLIC_STATUS_URL`); they are never committed as deployment defaults.
`CHECK_ONLY=1 scripts/deploy-passed-main.sh` fails whenever the live revision
differs from the latest passing CI revision.

This is **source-revision correspondence**, not binary artifact promotion: CI and
the host independently rebuild the same commit. OCI labels, linker metadata, and
runtime checks prevent source-SHA ambiguity, but mutable upstream base-image tags
can still produce different image digests. Digest-pinned bases plus a signed CI
artifact/provenance lane would be required for byte-identical artifact assurance.

## Deterministic dummy provider

`cmd/dummy-openai` is a standalone process implementing the subset of OpenAI-compatible contracts Thornhill consumes:

- Realtime call creation and authenticated sideband WebSocket events;
- deterministic response lifecycle/transcript/usage events;
- speech and chat-completion HTTP response shapes;
- health and graceful SIGTERM shutdown.

It runs no model, performs no inference, and makes no external requests. The integration test builds it, starts it on an operating-system-selected loopback port with a cryptographically random bearer token and call/test values, exercises the actual Thornhill Realtime client, then verifies process termination.

It intentionally does not emulate WebRTC media/RTP or validate AI answer quality. Those remain live-canary concerns. The provider is kept behind a small separable package and command so it can become an independent conformance-provider project once more than Thornhill consumes it.

### Existing-provider evaluation

The preferred reuse candidate is [CopilotKit/aimock](https://github.com/CopilotKit/aimock): it is active, MIT-licensed, and advertises OpenAI Realtime GA, chat, and speech coverage. We tested v1.35.1 rather than assuming README compatibility. Its Realtime server failed the RFC 6455 handshake with Thornhill's strict Go WebSocket client because that release uses a non-standard WebSocket GUID when computing `Sec-WebSocket-Accept`; it also does not implement the WebRTC call-creation endpoint `/v1/realtime/calls`. Weakening Thornhill's handshake validation or carrying a hidden `node_modules` patch would make the test less meaningful, so aimock is not currently in the dependency graph.

[bauerDOTuzh/openai-realtimeapi-mock](https://github.com/bauerDOTuzh/openai-realtimeapi-mock) provides a useful scenario-oriented Realtime WebSocket mock, but its documented protocol is deliberately simplified/legacy, requires a WAV fixture, and does not cover Thornhill's call-creation, chat-summary, and speech contracts. Re-evaluate upstream providers before extracting the local implementation; retire local code when an independently maintained provider passes this conformance test without compatibility exceptions.

## Randomized tests and fuzzing

Randomized integration tests use operating-system cryptographic randomness, not timestamps, fixed names, or a pseudo-random seed. Infrastructure is isolated and destroyed after each run. On failure, report enough generated identity to reproduce the state only when it is non-secret; ephemeral passwords are never logged.

Native Go fuzzing complements that entropy with minimized, replayable corpora. PRs run short campaigns; `.github/workflows/fuzz.yml` runs a weekly two-minute campaign per authority/protocol target and preserves minimized failure corpora as artifacts. Fuzz inputs never reach live providers, execute commands, or carry credentials.
