#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${NEON_V2_TEST_DATABASE_URL:?Dedicated disposable CI PostgreSQL DSN required}"
: "${NEON_DATA_API_TEST_POSTGREST:?Pinned PostgREST executable required for the real RLS gate}"
# 六分片共享同一份可信源码清单、每片一个隔离 PostgreSQL service。
# 真实 libpq 客户端必须存在；缺少时直接使 CI 失败，不允许产生 SKIP。
NEON_GO_SHARD_INDEX=$(printenv NEON_GO_SHARD_INDEX 2>/dev/null || echo 0)
NEON_GO_SHARD_COUNT=$(printenv NEON_GO_SHARD_COUNT 2>/dev/null || echo 1)
if [[ ! "$NEON_GO_SHARD_INDEX" =~ ^[0-9]+$ || ! "$NEON_GO_SHARD_COUNT" =~ ^[1-9][0-9]*$ ]]; then
  echo 'Invalid Go CI shard index/count'; exit 1
fi
if [[ -z "$(printenv DSGA_TEST_PSQL 2>/dev/null || true)" ]]; then
  export DSGA_TEST_PSQL="$(command -v psql || true)"
fi
if [[ ! -x "$DSGA_TEST_PSQL" ]]; then
  echo 'Real libpq executable missing: DSGA_TEST_PSQL required'; exit 1
fi
"$DSGA_TEST_PSQL" --version
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
# 从当前编译的控制面测试包实时发现清单，按 SHA256 名称稳定分片，
# 不静态排除失败测试；子测试仍由 go test 的 -run 语义完整展开。
(cd api && go test -list '^Test' ./internal/control) > "$task_evidence/control-test-inventory.txt"
node tools/select-go-test-shard.mjs "$task_evidence/control-test-inventory.txt"   "$NEON_GO_SHARD_INDEX" "$NEON_GO_SHARD_COUNT" "$task_evidence/control-shard.json"
pattern=$(node -p 'JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")).pattern'   "$task_evidence/control-shard.json")
# Go 安全 Renderer 只生成本次新鲜的契约文件；Rust 消费验证仍是独立发布门禁，
# 不能仅凭 Go 导出成功声称跨语言集成已通过。
export NEON_DSGA_RENDERER_CONTRACT="$(pwd)/$task_evidence/renderer-contract.json"
test ! -e "$NEON_DSGA_RENDERER_CONTRACT"
set +e
(cd api && go test -race -count=1 -timeout=14m -json -run "$pattern" ./internal/control)   > "$task_evidence/tests.jsonl"
task_exit=$?
if [[ "$NEON_GO_SHARD_INDEX" == 0 && "$task_exit" == 0 ]]; then
  mapfile -t other_packages < <(cd api && go list ./... | grep -v '/internal/control$')
  if [[ ${#other_packages[@]} -gt 0 ]]; then
    (cd api && go test -race -count=1 -timeout=10m -json "${other_packages[@]}")       >> "$task_evidence/tests.jsonl"
    task_exit=$?
  fi
fi
set -e
contract_selected=$(node -p 'JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")).tests.includes("TestDSGARendererNativeContractExport")'   "$task_evidence/control-shard.json")
if [[ "$contract_selected" == true ]]; then
  test -s "$NEON_DSGA_RENDERER_CONTRACT"
  sha256sum "$NEON_DSGA_RENDERER_CONTRACT" > "$task_evidence/renderer-contract.sha256"
fi
node tools/check-go-tests.mjs "$task_evidence/tests.jsonl" "$task_evidence/result.json"
exit "$task_exit"
