# Container design, security, and maintenance

Reviewed against primary documentation on **2026-07-14**.

## Release baseline

Thornhill treats the container image as a tested release artifact, not merely a packaging step:

- build the web client and Go service in separate builder stages;
- copy only the linked service binary, compiled web assets, license, and writable-data directory into the runtime image;
- pin every base and scanner image with a readable tag plus immutable manifest digest;
- run the application as numeric UID/GID `65532:65532` on Chainguard's minimal `static` image;
- keep the root filesystem read-only, provide only explicit `/tmp` and `/data` writable mounts, drop every Linux capability, retain Docker's default seccomp profile, set `no-new-privileges`, and impose a PID limit;
- run PostgreSQL with a read-only root filesystem, writable storage only at the named database volume plus bounded `/run/postgresql` and `/tmp` tmpfs mounts, all capabilities dropped except the five required by the official first-start ownership/drop-privilege path, `no-new-privileges`, a PID limit, and no published host port;
- expose an image-native health check that verifies the status endpoint is ready and, for release builds, versioned and serving the image's linked Git revision; unversioned development images can be healthy but remain ineligible for deployment;
- build and test the actual final application and PostgreSQL images on every pull request without repository or deployment secrets.

PostgreSQL remains a separate container and the application persists no state in its root filesystem. PostgreSQL's root entrypoint is an initialization boundary rather than the database runtime identity: after preparing the persistent volume, the database process directly beneath Docker's init is verified to run as upstream UID `70`. Node and Go toolchains never enter the application runtime layer.

## Build and dependency discipline

`Dockerfile` uses deterministic lockfile installs (`npm ci --ignore-scripts` and committed Go checksums) and an explicit multi-stage build. `.dockerignore` excludes Git metadata, local environment files, dependency trees, coverage, and developer artifacts from the build context. Build arguments are metadata only; credentials must never be passed through `ARG`, `ENV`, copied files, labels, or provenance-visible inputs.

BuildKit is mandatory. CI runs Dockerfile build checks before building, and the host deployer uses `docker buildx build --pull --load`. The pinned digest fixes the starting filesystem while the readable tag keeps Dependabot updates reviewable. `--pull` ensures the pinned reference is resolved rather than accidentally using an unrelated local tag.

The PostgreSQL wrapper starts from a pinned official image and applies current Alpine repository security fixes. This is an explicit trade-off: the base is reproducible, while the final OS package set tracks security fixes available at build time. PostgreSQL builds use `--no-cache` so a previously cached `apk upgrade` layer cannot hide newly published fixes. The resulting image, rather than only its source manifest, is scanned and recorded in an SBOM.

## Qualification pipeline

The required `Go, web, and image build` job is one secretless, read-only qualification lane. It runs:

- Gitleaks over fetched Git history;
- `gofmt`, `go vet`, Staticcheck, and `govulncheck`;
- race-enabled Go tests, boundary fuzzing, and the deterministic provider integration test;
- TypeScript checking, Biome's maintained static-analysis rules, production web build, and `npm audit`;
- Actionlint over every workflow;
- BuildKit Dockerfile checks and Hadolint;
- ShellCheck over every tracked shell script;
- Trivy repository dependency and Dockerfile-misconfiguration scans;
- Trivy scans of the built application and PostgreSQL images;
- a runtime harness that first renders and asserts the checked-in Compose security model, then verifies application and PostgreSQL runtime identities, image health, exact source-revision reporting, read-only roots, least capabilities, `no-new-privileges`, both PID limits, and graceful application `SIGTERM` shutdown;
- real PostgreSQL migration/concurrency integration tests and Compose-model validation;
- CycloneDX SBOM generation and 30-day artifact retention for both images.

The vulnerability gate rejects fixable `HIGH` or `CRITICAL` findings. Unfixed findings remain visible but do not block an otherwise unremediable build. New suppressions must be narrow, justified, and time-bounded; there is currently no vulnerability ignore file.

### PostgreSQL scanner scope

Trivy's Dockerfile non-root rule is skipped only for `Dockerfile.postgres`: the official entrypoint must initially have enough authority to initialize and fix ownership on a fresh named volume before dropping to the `postgres` user. Compose drops every ambient capability, adds back only `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETGID`, and `SETUID` for that transition, and the runtime harness verifies the resulting database process is UID `70`. The database is not published to a host port in the production Compose model.

The third-party `gosu` launcher is excluded from language-package gating because signature-only scanning reports every Go standard-library advisory compiled into that helper without reachability analysis. The PostgreSQL image is still gated on all OS packages, including current Alpine fixes, and receives a complete CycloneDX inventory. Thornhill's own Go binary receives full OS and language-aware image scanning plus call-aware `govulncheck`.

## Scanner and ruleset maintenance

No scanner is installed from an unversioned network command in CI.

- Staticcheck, `govulncheck`, and Actionlint are Go `tool` dependencies in `go.mod`; the root `gomod` Dependabot entry updates their engines and embedded checks.
- Biome and its rules are locked in `web/package-lock.json`; the `/web` npm entry updates them. `biome.json` references the package-local schema rather than a separately pinned remote schema URL.
- Hadolint, ShellCheck, and Trivy run from tag-plus-digest-pinned images in `.github/scanners/compose.yml`; a dedicated `docker-compose` Dependabot entry groups updates to the scanners and their embedded rule engines.
- Trivy refreshes its vulnerability database and misconfiguration checks on each clean CI run. The scanner version remains reviewable and reproducible.
- GitHub Actions, including Gitleaks and SBOM artifact upload, are full-SHA pinned and updated by the `github-actions` Dependabot ecosystem.
- Application build bases are updated by the Docker ecosystem; production and scanner Compose references are covered separately by the Docker Compose ecosystem.

`cmd/ci-policy-check` fails CI if scanner steps disappear, images lose digest pins, or any required Dependabot ecosystem/directory coverage is removed.

## Operations and patch cadence

1. Review daily Dependabot PRs; do not merge a digest or scanner update unless the entire qualification lane passes.
2. Rebuild and redeploy promptly after base, Go, Node, npm, PostgreSQL, scanner, or vulnerability-database changes. Do not assume a previously clean image remains clean.
3. Retain source SHA, OCI revision label, linked binary version, status response, SBOM, CI run, and deployment receipt as the correspondence chain.
4. Investigate scanner findings in reachability and runtime context; do not dismiss a raw package-name match, but do not present it as an exploitable incident without evidence.
5. When images are eventually published to a registry, add registry-native SBOM and provenance attestations, bind promotion to the immutable image digest, verify signatures/attestations before deployment, and retain rollback artifacts under a documented cleanup policy.
6. Periodically test clean builds, health transitions, stop behavior, rollback, and restoration from persistent data—not only warm-cache happy paths.

## Primary sources

- Docker, [Building best practices](https://docs.docker.com/build/building/best-practices/)
- Docker, [Build secrets](https://docs.docker.com/build/building/secrets/)
- Docker, [Engine security](https://docs.docker.com/engine/security/) and [seccomp](https://docs.docker.com/engine/security/seccomp/)
- Docker, [Test before push in GitHub Actions](https://docs.docker.com/build/ci/github-actions/test-before-push/)
- Docker, [SBOM and provenance attestations](https://docs.docker.com/build/ci/github-actions/attestations/)
- Google Cloud, [Building leaner containers](https://cloud.google.com/build/docs/optimize-builds/building-leaner-containers)
- Google Cloud, [Container scanning overview](https://cloud.google.com/artifact-analysis/docs/container-scanning-overview)
- Google Cloud, [Generate and validate build provenance](https://cloud.google.com/build/docs/securing-builds/generate-validate-build-provenance)
- GoogleContainerTools, [Distroless images](https://github.com/GoogleContainerTools/distroless)
- Chainguard, [Using the static image](https://edu.chainguard.dev/chainguard/chainguard-images/how-to-use/static-base-image/)
- Chainguard, [Considerations for image updates](https://edu.chainguard.dev/chainguard/chainguard-images/staying-secure/updating-images/considerations-for-image-updates/)
- GitHub, [Dependabot-supported ecosystems and repositories](https://docs.github.com/en/code-security/dependabot/ecosystems-supported-by-dependabot/supported-ecosystems-and-repositories)
- Trivy, [Container image scanning](https://trivy.dev/latest/docs/target/container_image/)
