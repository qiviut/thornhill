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

## Action allowlist

A repository-level Actions policy restricts which actions may run at all: those
owned by this account, those authored by GitHub, and explicitly vetted exceptions —
currently `gitleaks/gitleaks-action@*` and `ossf/scorecard-action@*`. It
**additionally** requires every reference to be a full-length commit SHA. Those two
conditions are independent, which is what makes a wildcard entry narrower than it
first appears: it admits any *pinned commit* of that one repository, never a mutable
tag or branch.

The policy is enforced before a workflow starts, so a violation surfaces as a
`startup_failure` run with **no jobs and no logs** — a uniquely unhelpful signal, and
one that a workflow which does not run on pull requests will only produce after
merge. `cipolicy.checkPinnedWorkflowActions` therefore asserts the SHA-pinning half
locally, so a mutable reference fails the required check on the pull request next to
its reason. The *which actions* half lives in repository settings and cannot be
asserted from here; adding a workflow that uses anything else requires widening the
allowlist first, and a lane added without that step will silently never run.

The wildcard form is a deliberate balance for this project rather than an oversight.
An exact-SHA entry would be tighter, but it has to be paired with a Dependabot
`ignore` for that action: otherwise an auto-merged bump introduces a SHA the
allowlist does not admit, and the lane reverts to a logless startup failure while
the bump's own CI stays green. That trade buys little for an advisory check and costs
a two-place manual update on every version.

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

Before source evidence runs, `scripts/test-high-risk-review.py` verifies the
classifier's own coverage for deployment installers, scanner configuration, and
review-gate helpers. `scripts/high-risk-review.py` then binds its report to the
actual contributor head (`pull_request.head.sha`) and base (`pull_request.base.sha`),
not GitHub's synthetic merge ref; push and protected-main dispatch contexts use
their event-specific revisions. A normal base/head pair uses the three-dot
range; when GitHub supplies an all-zero initial-push base, the reviewer diffs the
empty tree against the submitted head so root commits and every commit in a
multi-commit initial push are covered. Changes
classified as lifecycle, stateful-deployment, or pipeline-container run their
corresponding evidence matrix. The resulting `automated-evidence-complete`
disposition is deterministic CI evidence, not a human-review approval.

### 2. Promotion: branch protection decides trust

Only the exact revision that passed the required check may merge to `main`. Dependabot pull requests use the same secretless qualification lane. After CI succeeds, the review `workflow_run` job may act on only an open same-repository PR whose actor and author are `dependabot[bot]`, base is `main`, branch is `dependabot/*`, and head SHA exactly matches the successful CI run. It never checks out or executes PR code and has only read access plus `pull-requests: write`. A separate merge lane re-derives the same guards and holds the branch-write grants described below. Repository-wide workflow permissions remain read-only; only those isolated jobs receive write access. This bot approval is an automation signal, not independent human review.

**Reviewing and merging are two lanes, and only the second can write to the branch.** That separation is the point: promotion is unattended, so routine dependency currency does not wait on a human noticing a green PR, and a stale queue of open Dependabot PRs is a breakage signal rather than a maintenance backlog — but the lane that grants review authority never also holds the power to land code.

Delegating the merge to Dependabot's own credentials was tried first, because it would have meant no Actions lane ever holding `contents: write`. **It does not work.** A `@dependabot squash and merge` comment authored by `github-actions[bot]` is silently ignored: that actor has no write access to the repository (`author_association: NONE`), so Dependabot never acknowledges the command. Observed on three pull requests whose required check had gone green — approval was created, the comment was posted, and the PRs sat open and unacknowledged. The failure mode was inert rather than unsafe, which is why it was worth attempting, but it does not deliver unattended promotion.

`.github/workflows/dependabot-auto-merge.yml` therefore holds the grant, and its containment is the point:

- `contents: write`, `pull-requests: write`, and `actions: write` exist on one job; the workflow default stays read-only.
- It **runs no actions at all**. Every step is a `run:` step against the pre-installed `gh` CLI, so nothing third-party executes with a token that can write to `main`.
- It re-derives every guard — event, actor, source repository, branch prefix, base branch, and head SHA — from the `workflow_run` metadata rather than trusting the review lane, so neither lane can widen the other.
- The merge request names the CI-tested SHA. A rebase landing between qualification and the request changes the SHA, and GitHub refuses rather than merging untested code.
- A refusal exits zero. Under a strict required check every merge leaves the other open branches out of date, so Dependabot rebases them and the replacement revision earns its own run and its own attempt; treating that convergence as failure would manufacture noise.
- A successful squash reads the landed `main` SHA back from GitHub and dispatches full `CI` using the required `expected_sha` input. GitHub's API accepts a branch or tag ref rather than a raw commit, so CI preflight rejects the run before checkout unless `main`, the resolved run SHA, and `expected_sha` all match. The merge lane also reads back the created run ID and cancels a race that resolved to a newer revision. CI concurrency separates push runs from SHA-keyed dispatch runs, so cancelling that stale dispatch cannot cancel qualification already running for newer `main`. This explicit dispatch is required because GitHub suppresses recursive `push` workflows for commits created with `GITHUB_TOKEN`.
- The merge/dispatch transition is restart-safe. A rerun revalidates the original pull-request number and exact tested head, recognizes an already merged squash through its immutable `merge_commit_sha`, and reuses queued, running, or successful qualification for that landed SHA. Only a missing, failed, or cancelled qualification is dispatched again.

The host deployer treats only normal `push` CI and exact-SHA protected-main
`workflow_dispatch` CI as promotion evidence. The merge lane normally emits the
dispatch, but an authorized manual dispatch is deliberately equivalent because
the required preflight binds it to the current protected-main SHA before
checkout. Pull-request and non-main runs remain ineligible, and exact-SHA/current-main
checks remain load-bearing.

Branch protection remains the decider in both cases: the required check must have passed and the branch must be up to date, and squash is the only method linear history permits.

`cipolicy` pins both lanes. `checkDependabotApproval` keeps the review lane free of branch write access, `checkDependabotMerge` pins the merge lane's triggers, both permission sets, its single job, its absence of external actions, its independently derived guards, the SHA-bound squash, and the exact landed-SHA CI dispatch. `TestApprovalLaneCannotMerge` additionally fails if merge capability ever returns to the reviewing lane, since that would put the write grant and the review authority back in one file.

Unattended merging does not relax the promotion invariant, because the bot approval was never the gate. `required_approving_review_count` is `0`, so `required_status_checks.strict` on `Go, web, and image build` is what decides. `required_linear_history` means the merge must be a squash — a plain merge commit is rejected. If Dependabot force-pushes a rebase, the new SHA earns its own CI run, approval, and merge attempt; the request is SHA-bound so it cannot carry over to untested code.

Because promotion is unattended, two settings outside this repository become load-bearing and must stay enabled:

- **Dependabot security updates** and **Dependabot alerts**. `dependabot.yml` configures *version* updates only. Without the security-update toggle, a published advisory produces no pull request until a routine version bump happens to carry the fix — which is how `postcss` sat on GHSA-r28c-9q8g-f849 while the `npm audit` gate went red.
- **Allow squash merging**, the only method linear history permits and the one the merge lane requests.

There is deliberately no cooldown on version updates: the operator's stated preference is fastest possible patching, with supply-chain risk absorbed by the secretless qualification lane, `--ignore-scripts` installs, digest-pinned bases, and the image/vulnerability gates rather than by delaying releases. Updates are grouped per ecosystem because `strict` invalidates every other open branch on each merge, so batching converges in far fewer full CI cycles.

If unattended promotion ever needs to be withdrawn, delete `dependabot-auto-merge.yml` along with `checkDependabotMerge` and its test. The review lane keeps working on its own and the repository returns to approve-only, where a stale queue of open Dependabot PRs means a human has not looked yet rather than that something is broken.

### 3. Measurement: advisory, never a gate

`.github/workflows/scorecard.yml` scores this repository's own supply-chain posture
weekly, on pushes to `main`, and on demand, filing regressions as code-scanning
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
both permission sets, the single job, and the absence of publication.

Read two sub-scores with context rather than at face value:

- **Branch-Protection** scores low because `GITHUB_TOKEN` cannot read protection
  settings; that needs an admin-scoped PAT this lane deliberately does not hold.
  The real contract is `.github/branch-protection.json`, applied and verified by
  `./scripts/apply-branch-protection.sh` and asserted by `cipolicy`.
- Signed-Releases and provenance sub-scores reflect that detached artifact signing
  is not part of the near-term trust model. The local bundle still binds the host
  to CI-built image IDs and the exact protected-main SHA; signing/attestation
  verification remains an additive future control.

### 4. Operator-installed release bundles

The qualification workflow remains secretless and read-only. After the image,
runtime, security, and SBOM gates pass, the image lane assembles an artifact named
`thornhill-release-<full-SHA>`. That bundle contains the exact OCI archives,
source/image manifest, checksums, SBOMs, Compose models, and local installer. It
is produced from the already-qualified images; it does not rebuild them.

A registry publisher is not the production promotion authority for this model. The
host receives a bundle through an operator-approved transfer path. GitHub Actions
has no SSH, Tailnet, Docker, database, server, or deployment credentials. The
operator selects the exact successful protected-main run, downloads the matching
bundle, verifies the full source SHA and artifact checksums, and invokes the
installer locally. Detached signing/attestation can be added as a stronger
artifact-authenticity control; the current required binding is the operator's
exact protected-main CI evidence plus the bundle's source/image/checksum checks.

`.github/workflows/canary.yml` remains separately opt-in through
`workflow_dispatch`. It runs only from `main` in the protected
`production-canary` environment and rechecks the supplied SHA before checkout.
The canary is lower-priority evidence and never substitutes for protected-main
CI, image qualification, or local package installation read-back.

### Local release installation correspondence

The local `scripts/install-local-release.sh` is the promotion boundary. It accepts
only an operator-supplied bundle and exact expected SHA. It does not invoke `gh`,
GitHub APIs, registry pulls, SSH, or remote deployment. It uses `docker load` for
the two bundle archives and `docker compose up --no-build` against the existing
Compose project name and host `.env`.

Before any container mutation it verifies:

- release metadata and the exact expected full source SHA;
- the app/PostgreSQL image IDs, image revision labels, and all `SHA256SUMS` entries;
- host `.env` ownership and mode without printing its contents;
- the currently running local/Tailnet revision, image labels, binary version,
  database health, runtime UID, and database hardening;
- the configured database password through the disposable client path.

The installer then creates a bounded host-local PostgreSQL recovery snapshot,
atomically pauses new dispatches, refuses to proceed while queued or active work
exists, stops the application before PostgreSQL, and recreates both services
without a build. It verifies both status paths, the in-container binary, database
health, and hardening after startup. It records a local receipt and transition
journal. A failed replacement attempts to restore locally tagged previous images;
if rollback cannot be verified, dispatch remains paused for operator recovery.

This preserves the inspectable chain:

```text
Protected-main CI run → exact head SHA → qualified image archives →
  bundle manifest/checksums → docker load image IDs → OCI labels → binary commit
  → live status endpoints and PostgreSQL hardening → local deployed receipt
```

The former `thornhill-ci-deploy.timer`/registry controller is not part of this
promotion path and must remain disabled. Its source files are retained only until
a separate cleanup/removal change is reviewed. `docs/rollback-compatibility.json`
continues to document migration compatibility for any future package that changes
schema; incompatible migrations require an explicit operator backup/restore path
rather than an automatic downgrade.

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
