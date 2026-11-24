import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

function render(extra = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dsga-rbac-site-'));
  try {
    const file = path.join(dir, 'values.json');
    fs.writeFileSync(file, JSON.stringify({fullnameOverride: 'other-control', api: {creationEnabled: true}, dsga: {createEnabled: true, runtimeCatalogSecret: 'site-catalog', runtimeSelectionNamespace: 'neon-control-other', agentNamespace: 'system-other', runtimeAccessRolePrefix: 'other-runtime-access', nodeReadClusterRole: 'other-worker-nodes', ...extra}}));
    return spawnSync(process.env.HELM_BIN || 'helm', ['template', 'other-control', 'charts/neon-control-plane', '-n', 'tenant-other', '-f', 'charts/neon-control-plane/ci/render-values.yaml', '-f', file], {encoding: 'utf8', timeout: 30000});
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
}

test('DSGA 跨命名空间权限跟随显式站点引用，API 不获得 Node 写权限', () => {
  const result = render();
  assert.equal(result.status, 0, result.stderr);
  const docs = YAML.parseAllDocuments(result.stdout).map(x => x.toJSON()).filter(Boolean);
  const bindings = docs.filter(x => x.kind === 'RoleBinding' && x.metadata.name.startsWith('other-control-dsga-'));
  assert.ok(bindings.length >= 6);
  assert.deepEqual([...new Set(bindings.map(x => x.metadata.namespace))].sort(), ['neon-control-other', 'system-other', 'tenant-other']);
  for (const binding of bindings) for (const subject of binding.subjects) assert.equal(subject.namespace, 'tenant-other');
  const delegation = docs.find(x => x.kind === 'ClusterRoleBinding' && x.metadata.name === 'other-control-dsga-node-read');
  assert.equal(delegation.roleRef.name, 'other-worker-nodes');
  const policy = docs.find(x => x.kind === 'Role' && x.metadata.name === 'other-control-dsga-platform-policy');
  assert.equal(policy.metadata.namespace, 'neon-control-other');
  assert.deepEqual(policy.rules, [{apiGroups: [''], resources: ['configmaps'], verbs: ['get', 'update']}]);
  assert.ok(bindings.filter(x => x.roleRef.name.includes('runtime-access')).every(x => x.roleRef.name.startsWith('other-runtime-access-')));
});

test('DSGA 拒绝通配命名空间与超出当前原生合同的平台空间', () => {
  for (const extra of [{runtimeSelectionNamespace: '*'}, {runtimeSelectionNamespace: 'ordinary-space'}, {agentNamespace: '*'}, {runtimeAccessRolePrefix: '*'}]) {
    const result = render(extra);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /schema|pattern|dsga/i);
  }
});
