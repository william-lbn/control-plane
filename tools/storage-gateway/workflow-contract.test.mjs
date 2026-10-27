import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const read = path => readFileSync(new URL(path, import.meta.url), 'utf8');
const workflow = read('../../.github/workflows/ci.yml');
const dockerfile = read('../../Dockerfile.storage-gateway');
const legacy = ['api', 'web', 'gateway', 'dataapi', 'postgrest', 'adapter', 'auth', 'ai'];
const publish = workflow.slice(workflow.indexOf('  publish:'));

test('Storage image joins the existing publication gate without changing eight image identities', () => {
  const matrix = publish.match(/matrix: \{component: \[([^\]]+)\]\}/);
  assert.ok(matrix, 'explicit image component allowlist required');
  assert.deepEqual(matrix[1].split(',').map(v => v.trim()), [...legacy, 'storage-gateway']);
  assert.match(publish, /needs: \[go, web, helm, workflows, auth, functions, clients\]/);
  assert.match(publish, /github\.event_name != 'pull_request'/);
  assert.match(publish, /github\.ref == 'refs\/heads\/main'/);
  assert.match(publish, /startsWith\(github\.ref, 'refs\/tags\/v'\)/);
  assert.match(publish, /max-parallel: 2/);
  assert.match(publish, /file: Dockerfile\.\$\{\{ matrix\.component \}\}/);
  for (const [, action] of workflow.matchAll(/- uses: ([^\s#]+)/g)) {
    assert.match(action, /@[a-f0-9]{40}$/, 'all workflow actions must remain pinned');
  }
});

test('Storage builds retain locked inputs isolated export cache and immutable publication receipts', () => {
  assert.match(publish, /GO_BUILDER_IMAGE=\$\{\{ steps\.names\.outputs\.GO_BUILDER_IMAGE \}\}/);
  assert.match(publish, /node tools\/build-args\.mjs >> "\$GITHUB_OUTPUT"/);
  const builders = JSON.parse(read('../../containers/builders.lock.json'));
  assert.match(builders.images.go, /@sha256:[a-f0-9]{64}$/);
  assert.match(publish, /cache-from: \|\s+type=gha,scope=\$\{\{ matrix\.component \}\}\s+\$\{\{ matrix\.component == 'storage-gateway' && 'type=gha,scope=api' \|\| '' \}\}/);
  assert.match(publish, /cache-to: type=gha,mode=max,scope=\$\{\{ matrix\.component \}\}/);
  assert.match(publish, /task_name="control-\$COMPONENT"/);
  assert.match(publish, /task_tag="sha-\$GITHUB_SHA-r\$GITHUB_RUN_ID-a\$GITHUB_RUN_ATTEMPT"/);
  assert.match(publish, /platforms: linux\/amd64/);
  assert.match(publish, /sbom: true/);
  assert.match(publish, /provenance: mode=max/);
  assert.match(publish, /DIGEST: \$\{\{ steps\.image\.outputs\.digest \}\}/);
  assert.match(publish, /name: 'image-\$\{\{ matrix\.component \}\}'/);
});

test('Storage runtime contains only its own command and CI distributes its matching Helm chart', () => {
  assert.match(dockerfile, /FROM \$\{GO_BUILDER_IMAGE\} AS build/);
  assert.match(dockerfile, /go build[^\n]+\.\/cmd\/storage-gateway/);
  assert.match(dockerfile, /FROM scratch/);
  assert.match(dockerfile, /USER 65532:65532/);
  assert.match(dockerfile, /ENTRYPOINT \["\/opt\/neon-control\/storage-gateway"\]/);
  assert.doesNotMatch(dockerfile, /cmd\/control-api|cmd\/control-worker|\/control-api \/opt|\/control-worker \/opt|hostPath/);
  const helm = read('../ci-helm.sh');
  assert.match(helm, /helm lint charts\/storage-gateway --strict/);
  assert.match(helm, /helm template neon-storage charts\/storage-gateway --namespace neon/);
  assert.ok(helm.indexOf('helm package charts/storage-gateway') < helm.indexOf('sha256sum artifacts/helm/*.tgz'));
  assert.match(workflow.slice(0, workflow.indexOf('  go:')), /node --test tools\/storage-gateway\/workflow-contract\.test\.mjs/);
});
