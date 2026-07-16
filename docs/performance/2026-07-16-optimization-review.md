# Performance optimization review — 2026-07-16

## Scope and stopping rule

This review optimizes avoidable work in Thornhill's application and GitHub
Actions pipeline. It does **not** trade away correctness, approval authority
safety, dependency scanning, fuzzing, reproducibility, or untrusted-PR trust
boundaries merely to shorten a number.

The baseline application revision was `231fc8bdbae1bf6e395f2a46fa9a2857ed9fc0f7`.

## Evidence collected

### Running application

A bounded, read-only local probe sent 160 requests per endpoint with eight
workers:

- `GET /api/status`: p50 2.64 ms, p95 5.26 ms, p99 6.58 ms, max 9.57 ms.
- `GET /`: p50 2.42 ms, p95 4.50 ms, p99 4.84 ms, max 5.29 ms.

The database is small and not a current hot path: `jobs` and `usage_ledger`
were 72 kB each and `event_log` 552 kB. `EXPLAIN (ANALYZE, BUFFERS)` showed
0.17 ms for active-job listing and 0.99 ms for today's usage aggregation.
Adding speculative indexes now would impose write/maintenance cost without a
measured benefit.

The production UI's content-hashed JavaScript asset was 202,396 bytes and its
CSS was 5,048 bytes. Go's `http.FileServer` sent both identity encoded with no
explicit cache policy. The Go default gzip encoder used by the new handler
produces a 63,541-byte JavaScript response: a 138,855-byte (68.6%) reduction.

A handler benchmark on a comparable 190 kB text asset measured:

- identity: 0.18–0.21 ms/request;
- gzip: 0.97–1.18 ms/request.

That sub-millisecond CPU cost is justified for a content-hashed asset whose
transfer then drops by about 69% and is cached for the asset generation.

### GitHub Actions

The three most recent successful CI runs accounted for 263, 265, and 276
seconds (median 265 seconds). Dominant stages were intentionally thorough:

- Go analysis and race tests: 74–75 seconds;
- authority/protocol fuzzing: 61–69 seconds;
- application image build: 29–30 seconds;
- source/configuration/final-image scan: 20–24 seconds.

The web verification is only 3–5 seconds. Removing it, shortening fuzzing,
or allowing mutable caches from an untrusted pull request would not be a
sound performance improvement.

## Changes made

### Static frontend delivery

`/assets/` text assets (`.js`, `.css`, `.json`, `.svg`) now:

- use gzip only for `GET` requests that explicitly accept it;
- retain identity responses for range requests and gzip declines;
- set `Cache-Control: public, max-age=31536000, immutable` only for successful
  or revalidated responses;
- emit `Vary: Accept-Encoding` so identity and gzip cache variants cannot be
  confused.

The HTML document remains `Cache-Control: no-cache`, ensuring the next deploy
can point a browser at new content-hashed asset names. Missing assets are
neither compressed nor cached immutably. Prebaked audio remains outside this
path because it may already be compressed and can require byte ranges.

### CI critical path

The single serial qualification job is now a fail-closed DAG:

1. **Preflight** performs the full-history Gitleaks scan and CI policy check
   before either lane evaluates checkout content.
2. **Source analysis and fuzzing** retains formatting, Actionlint, vet,
   Staticcheck, Govulncheck, race tests, fuzzing, provider conformance, and
   full web verification/audit.
3. **Image build and security** retains BuildKit validation, both image builds,
   hardening and PostgreSQL integration tests, Compose validation, full scans,
   and SBOM preservation.
4. **Go, web, and image build** remains the exact branch-protection and deploy
   check name. It runs with `always()` and fails unless both qualification
   lanes succeed.

This preserves the existing no-mutable-cache policy for public pull requests,
the no-cache PostgreSQL security refresh, and every prior quality/security
step. This prediction was validated by [PR #8 CI run 29471282705](https://github.com/qiviut/thornhill/actions/runs/29471282705): every preserved check passed, preflight took 14 seconds, source/fuzz took 181 seconds, image/security took 105 seconds in parallel, and the fail-closed aggregator took 3 seconds. End-to-end elapsed time was **207 seconds**, saving **58 seconds (21.9%)** against the prior 265-second median. Runner scheduling and variable upstream scan/download timing make this a measured sample, not a guaranteed SLA.

The CI policy harness now verifies this topology explicitly. It rejects a
missing preflight dependency, omitted image security scan, or a non-fail-closed
aggregator.

## Deliberately not changed

- **Database indexes/query rewrites:** current plans and table sizes show no
  bottleneck. Revisit with production measurements after meaningful data
  growth.
- **Fuzz duration and full image scans:** they account for real assurance and
  stay in PR qualification; scheduled fuzzing remains longer at two minutes.
- **Untrusted PR dependency/build caches:** they remain disabled intentionally.
  A trusted-only cache design requires a separate supply-chain review, not a
  casual speed tweak.
- **Docker PostgreSQL `--no-cache`:** it intentionally refreshes Alpine
  security packages before scanning.

## Validation

Before shipping, run the full Go race suite, PostgreSQL integration,
provider-conformance test, fuzz suite, web checks/build/audit, Actionlint,
CI-policy validation, Compose/scanner configuration, Docker builds and
hardening/scanner checks. The PR CI timing is recorded against the prior
265-second median after merge.
