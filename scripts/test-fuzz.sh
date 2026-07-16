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
  go test "${package}" -run='^$' -fuzz="^${target}$" -fuzztime="${fuzztime}"
done
