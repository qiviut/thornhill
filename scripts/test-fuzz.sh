#!/usr/bin/env bash
set -euo pipefail

fuzztime=${FUZZTIME:-5s}
targets=(
  './internal/store:FuzzApprovalPatternHash'
  './internal/bridge:FuzzApprovalDecisionIsSingleUse'
  './internal/bridge:FuzzParkedApprovalNeverInvokesAuthority'
  './internal/openairt:FuzzRealtimeEventExtractors'
  './internal/gateway:FuzzValidCallID'
  './internal/gateway:FuzzOriginPolicy'
)

for entry in "${targets[@]}"; do
  package=${entry%%:*}
  target=${entry#*:}
  echo "==> ${target} (${package}, ${fuzztime})"
  # One worker keeps the short per-target budget deterministic on shared CI
  # runners; the targets still run sequentially with independent processes.
  go test "${package}" -run='^$' -fuzz="^${target}$" -fuzztime="${fuzztime}" -parallel=1
done
