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

`CI` executes contributor code and dependency build scripts with only `contents: read` and `pull-requests: read`. Pull-request metadata access lets Gitleaks resolve and scan the exact PR commit range; the workflow has no pull-request write permission. It must never reference repository, environment, Dependabot, cloud, tailnet, model-provider, signing, or deployment secrets. It establishes whether one immutable source revision is eligible for promotion; it does not deploy or sign.

The required check exercises:

- a Gitleaks scan over the fetched Git history before builds execute;
- Go formatting, vetting, Staticcheck, `govulncheck`, race tests, and short invariant-driven fuzz campaigns;
- an ephemeral deterministic OpenAI-compatible provider process;
- web dependency installation, TypeScript and Biome analysis, checks, and production build;
- version-or-digest-pinned Actionlint, Hadolint, ShellCheck, Gitleaks, and Trivy tooling with Dependabot-covered engines and embedded rule sets;
- BuildKit validation plus application and PostgreSQL image builds from tag-and-digest-pinned bases;
- built-image vulnerability gates, CycloneDX SBOMs, and runtime checks for health, non-root execution, read-only root, dropped capabilities, `no-new-privileges`, and graceful shutdown;
- migrations and concurrent approval claims against an ephemeral PostgreSQL container whose username, database, password, container name, host port, and test data are freshly generated from cryptographic randomness;
- the Compose delivery model and this policy itself.

### 2. Promotion: branch protection decides trust

Only the exact revision that passed the required check may merge to `main`. Dependabot pull requests use the same secretless qualification lane. After CI succeeds, a separate `workflow_run` job may act on only an open same-repository PR whose actor and author are `dependabot[bot]`, base is `main`, branch is `dependabot/*`, and head SHA exactly matches the successful CI run. It never checks out or executes PR code, and has only read access plus `pull-requests: write`. Repository-wide workflow permissions remain read-only; only the narrowly required ability for Actions to submit PR reviews and comments is enabled. This bot approval is an automation signal, not independent human review.

That job also requests unattended promotion, so routine dependency currency does not depend on a human noticing a green PR. A stale queue of open Dependabot PRs is a breakage signal, not a maintenance backlog.

Reviewing and merging are two lanes, and only the second can write to the branch.

Delegating the merge to Dependabot's own credentials was tried first, because it would have meant no Actions lane ever holding `contents: write`. **It does not work.** A `@dependabot squash and merge` comment authored by `github-actions[bot]` is silently ignored: that actor has no write access to the repository (`author_association: NONE`), so Dependabot never acknowledges the command. Observed on three pull requests whose required check had gone green — approval was created, the comment was posted, and the PRs sat open and unacknowledged. The failure mode was inert rather than unsafe, which is why it was worth attempting, but it does not deliver unattended promotion.

`.github/workflows/dependabot-auto-merge.yml` therefore holds the grant, and its containment is the point:

- `contents: write` and `pull-requests: write` exist on one job; the workflow default stays read-only.
- It **runs no actions at all**. Every step is a `run:` step against the pre-installed `gh` CLI, so nothing third-party executes with a token that can write to `main`.
- It re-derives every guard — event, actor, source repository, branch prefix, base branch, and head SHA — from the `workflow_run` metadata rather than trusting the review lane, so neither lane can widen the other.
- The merge request names the CI-tested SHA. A rebase landing between qualification and the request changes the SHA, and GitHub refuses rather than merging untested code.
- A refusal exits zero. Under a strict required check every merge leaves the other open branches out of date, so Dependabot rebases them and the replacement revision earns its own run and its own attempt; treating that convergence as failure would manufacture noise.

Branch protection remains the decider in both cases: the required check must have passed and the branch must be up to date, and squash is the only method linear history permits.

`cipolicy` pins both lanes. `checkDependabotApproval` keeps the review lane free of branch write access, `checkDependabotMerge` pins the merge lane's triggers, both permission sets, its single job, its absence of external actions, its independently derived guards, and the SHA-bound squash. `TestApprovalLaneCannotMerge` additionally fails if merge capability ever returns to the reviewing lane, since that would put the write grant and the review authority back in one file.

Unattended merging does not relax the promotion invariant, because the bot approval was never the gate. `required_approving_review_count` is `0`, so `required_status_checks.strict` on `Go, web, and image build` is what decides. `required_linear_history` means the merge must be a squash — a plain merge commit is rejected. If Dependabot force-pushes a rebase, the new SHA earns its own CI run, approval, and merge attempt; the request is SHA-bound so it cannot carry over to untested code.

Because promotion is unattended, two settings outside this repository become load-bearing and must stay enabled:

- **Dependabot security updates** and **Dependabot alerts**. `dependabot.yml` configures *version* updates only. Without the security-update toggle, a published advisory produces no pull request until a routine version bump happens to carry the fix — which is how `postcss` sat on GHSA-r28c-9q8g-f849 while the `npm audit` gate went red.
- **Allow squash merging**, the only method linear history permits and the one the merge lane requests.

There is deliberately no cooldown on version updates: the operator's stated preference is fastest possible patching, with supply-chain risk absorbed by the secretless qualification lane, `--ignore-scripts` installs, digest-pinned bases, and the image/vulnerability gates rather than by delaying releases. Updates are grouped per ecosystem because `strict` invalidates every other open branch on each merge, so batching converges in far fewer full CI cycles.

If unattended promotion ever needs to be withdrawn, delete `dependabot-auto-merge.yml` along with `checkDependabotMerge` and its test. The review lane keeps working on its own and the repository returns to approve-only, where a stale queue of open Dependabot PRs means a human has not looked yet rather than that something is broken.

### 2b. Measurement: advisory, never a gate

`.github/workflows/scorecard.yml` scores this repository's own supply-chain posture
weekly, on pushes to `main`, and on demand, and files regressions as code-scanning
alerts beside the CodeQL analyses. It never runs on `pull_request`: Scorecard
evaluates the default branch, and a contributor-triggered run must not reach a lane
holding a write grant.

It is deliberately advisory. Branch protection requires exactly one check, and that
stays the qualification lane; `cipolicy` enforces the single required context, so a
drifted third-party heuristic opens a conversation instead of blocking a merge.

This is the only workflow holding `security-events: write`, so the grant is confined
to one job while the workflow default stays `contents: read`. Result publication is
disabled on purpose: `publish_results: true` requires `id-token: write`, and an OIDC
token is a materially larger grant than this measurement justifies. The cost is only
the public badge and the OpenSSF dataset entry. `checkScorecard` pins the triggers,
both permission sets, the single job, the absence of publication, and that every
action is referenced by a full commit SHA rather than a mutable tag.

Read two sub-scores with context rather than at face value:

- **Branch-Protection** scores low because `GITHUB_TOKEN` cannot read protection
  settings; that needs an admin-scoped PAT this lane deliberately does not hold.
  The real contract is `.github/branch-protection.json`, applied and verified by
  `./scripts/apply-branch-protection.sh` and asserted by `cipolicy`.
- **Signed-Releases** and provenance sub-scores reflect the documented absence of a
  signed artifact lane. Promotion here is source-revision correspondence, not binary
  artifact promotion, as described below.

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
ready, a PostgreSQL transaction atomically sets the dispatch pause. A database
trigger then rejects inserts and transitions into `queued` or `running`, while
already-running work may still complete or park safely. The deployer rechecks
that no queued or active work exists before replacing the services.

Both local and Tailnet UI/status probes, the running OCI label, and the in-container
binary must agree. Failure restores the prior application **and PostgreSQL**
images using the prior revision's Compose model, force-recreates both services,
and verifies the rollback. A revision that fails host verification
is recorded in `failed.json` and suppressed until a newer passing SHA arrives or
an operator explicitly sets `RETRY_FAILED=1`.

This preserves a directly inspectable chain:

```text
GitHub push CI run → head SHA → detached source worktree → revision-tagged app
  and PostgreSQL images → OCI app revision label → linked binary commit
  → live /api/status and PostgreSQL hardening checks → deployed.json receipt
```

The timer deliberately polls from the host rather than giving GitHub a Tailnet
or Docker credential. The externally reachable UI and status URLs are required
host-local service environment values (`PUBLIC_APP_URL` and
`PUBLIC_STATUS_URL`); they are never committed as deployment defaults.
Polling is a convergence mechanism, not a release-time requirement: the timer
runs every 15 minutes and may remain disabled during active development. If the
shared checkout has a modified deployment controller, or that controller does
not match the selected passing revision, poll mode records the reason in
`~/.local/state/thornhill-ci-deploy/deferred.json`, exits successfully without
deploying, and stays quiet. Direct script execution remains fail-closed for the
same conditions. After a merge, update the checkout, run the service once, and
enable the timer only after the deployed receipt and live revision agree.
`CHECK_ONLY=1 scripts/deploy-passed-main.sh` fails whenever the live revision
differs from the latest passing CI revision. It also fails if another deployment
holds the lock; lock contention is never reported as successful correspondence.

Automatic rollback reuses the persistent database after candidate startup has
applied its schema. `docs/rollback-compatibility.json` therefore binds an
explicit compatibility mode and rationale to the SHA-256 of the embedded schema
SQL. CI rejects an uncovered schema edit. The host deployer accepts only
`backward-compatible-additive`; `manual-forward-only` documents an incompatible
migration but blocks automatic promotion and rollback. A breaking migration
requires an operator-controlled backup/restore or forward-recovery runbook before
deployment—not a best-effort image downgrade.

This is **source-revision correspondence**, not binary artifact promotion: CI and
the host independently rebuild the same commit. OCI labels, linker metadata,
runtime checks, and digest-pinned bases eliminate source and base-image ambiguity,
but the PostgreSQL wrapper deliberately applies the current Alpine security
repository at build time. A signed CI artifact/provenance lane would still be
required for byte-identical artifact assurance.

The detailed container design, scanner policy, exceptions, maintenance cadence,
and primary-source research are in [container-security.md](container-security.md).

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
