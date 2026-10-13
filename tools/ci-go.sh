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
# Export the exact Linux S3 SDK acceptance client as CI evidence. It is not in
# API/Worker runtime images; live tests read their own protected credential file.
(cd api && go build -trimpath -o ../"$task_evidence"/storage-sdk-probe ./cmd/storage-sdk-probe)
TASK_S3_PROBE="$task_evidence/storage-sdk-probe" node --input-type=module <<'JS'
import fs from 'node:fs';
import crypto from 'node:crypto';
import {execFileSync} from 'node:child_process';
const file=process.env.TASK_S3_PROBE;
const commit=process.env.GITHUB_SHA || execFileSync('git',['rev-parse','HEAD'],{encoding:'utf8'}).trim();
if(!/^[a-f0-9]{40}$/.test(commit))throw new Error('Exact committed S3 probe source required');
fs.writeFileSync(file+'.json',JSON.stringify({source_commit:commit,platform:'linux/amd64',sha256:crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex'),runtime_service:false})+'\n');
JS
# Compile the opt-in real backing tests without executing them against public
# CI credentials. Live Kubernetes/MinIO acceptance has a separate Linux gate.
(cd api && go test -mod=readonly -tags=live -run '^$' ./internal/control) > "$task_evidence/live-compile.log"
(cd tools/postgrest-installer && gofmt -l .) > "$task_evidence/installer-gofmt.txt"
if [[ -s "$task_evidence/installer-gofmt.txt" ]]; then cat "$task_evidence/installer-gofmt.txt"; exit 1; fi
(cd tools/postgrest-installer && go vet ./... && go build -o ../../"$task_evidence"/postgrest-installer .)
set +e
(cd api && go test -race -count=1 -json ./...) > "$task_evidence/tests.jsonl"
task_exit=$?
set -e
node tools/check-go-tests.mjs "$task_evidence/tests.jsonl" "$task_evidence/result.json"
exit "$task_exit"
