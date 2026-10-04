#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${NEON_V2_TEST_DATABASE_URL:?Dedicated disposable CI PostgreSQL DSN required}"
: "${NEON_DATA_API_TEST_POSTGREST:?Pinned PostgREST executable required for the real RLS gate}"
task_attempt="${NEON_CI_ATTEMPT:-$(date -u +%Y%m%d%H%M%S)}"
[[ "$task_attempt" =~ ^[a-z0-9_]+$ ]] || { echo 'Invalid CI attempt'; exit 1; }
export NEON_V2_TEST_SCHEMA="v2_migration_ci_$task_attempt"
export NEON_V2_TEST_AUTH_SCHEMA="v2_auth_ci_$task_attempt"
export NEON_V2_TEST_IDLE_SCHEMA="v2_migration_idle_ci_$task_attempt"
export GOTOOLCHAIN=local GOTELEMETRY=off GOMAXPROCS=2
task_evidence="artifacts/go-$task_attempt"
mkdir -p "$task_evidence"
(cd api && gofmt -l cmd internal) > "$task_evidence/gofmt.txt"
if [[ -s "$task_evidence/gofmt.txt" ]]; then cat "$task_evidence/gofmt.txt"; exit 1; fi
(cd api && go vet ./...)
(cd tools/postgrest-installer && gofmt -l .) > "$task_evidence/installer-gofmt.txt"
if [[ -s "$task_evidence/installer-gofmt.txt" ]]; then cat "$task_evidence/installer-gofmt.txt"; exit 1; fi
(cd tools/postgrest-installer && go vet ./... && go build -o ../../"$task_evidence"/postgrest-installer .)
set +e
(cd api && go test -race -count=1 -json ./...) > "$task_evidence/tests.jsonl"
task_exit=$?
set -e
node tools/check-go-tests.mjs "$task_evidence/tests.jsonl" "$task_evidence/result.json"
exit "$task_exit"
