import assert from 'node:assert/strict';
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';
import { parseAllDocuments } from 'yaml';

export function documents(source) {
  return parseAllDocuments(source, { uniqueKeys: true }).map(document => {
    assert.equal(document.errors.length, 0, 'RBAC input must be valid unambiguous YAML');
    return document.toJSON();
  }).filter(Boolean);
}

function permissions(objects, account) {
  const roles = new Map(objects.filter(o => o.kind === 'Role').map(o => [o.metadata.name, o]));
  const verbs = new Set();
  for (const binding of objects.filter(o => o.kind === 'RoleBinding')) {
    if (!binding.subjects.some(s => s.kind === 'ServiceAccount' && s.name === account)) continue;
    assert.equal(binding.roleRef.kind, 'Role', 'These namespace accounts must not acquire external cluster grants');
    const role = roles.get(binding.roleRef.name);
    assert.ok(role, 'Every bound role must be in the rendered contract');
    for (const rule of role.rules) {
      if (!rule.apiGroups.some(g => g === '' || g === '*')) continue;
      if (!rule.resources.some(r => r === 'pods' || r === '*' || r.startsWith('pods/'))) continue;
      assert.deepEqual(rule.apiGroups, [''], 'Pod grants cannot use a wildcard group');
      assert.deepEqual(rule.resources, ['pods'], 'Observer cannot acquire wildcard or Pod-subresource permissions');
      for (const verb of rule.verbs) {
        assert.ok(['get', 'list'].includes(verb), 'No Pod mutation, delete, exec or wildcard grants');
        verbs.add(verb);
      }
    }
  }
  return [...verbs].sort();
}

export function assertRbacProfiles(split, combined) {
  const account = objects => {
    const api = objects.find(o => o.kind === 'Deployment' && o.spec.template.metadata.labels['app.kubernetes.io/component'] === 'api');
    assert.ok(api, 'Rendered API Deployment required');
    const name = api.spec.template.spec.serviceAccountName;
    assert.match(name, /-api$/);
    return name;
  };
  const apiAccount = account(split), workerAccount = apiAccount.slice(0, -4) + '-worker';
  const observerName = workerAccount + '-observer';
  const observer = split.find(o => o.kind === 'Role' && o.metadata.name === observerName);
  assert.ok(observer, 'Independent Endpoint retirement requires Worker-only observation');
  assert.deepEqual(observer.rules, [{ apiGroups: [''], resources: ['pods'], verbs: ['get', 'list'] }]);
  const binding = split.find(o => o.kind === 'RoleBinding' && o.metadata.name === observer.metadata.name);
  assert.ok(binding, 'The observer must actually be bound');
  assert.deepEqual(binding.roleRef, { apiGroup: 'rbac.authorization.k8s.io', kind: 'Role', name: observer.metadata.name });
  assert.deepEqual(binding.subjects, [{ kind: 'ServiceAccount', name: workerAccount, namespace: 'neon' }]);
  const worker = split.find(o => o.kind === 'Deployment' && o.spec.template.metadata.labels['app.kubernetes.io/component'] === 'worker');
  assert.equal(worker?.spec.template.spec.serviceAccountName, workerAccount);
  assert.deepEqual(permissions(split, apiAccount), ['get']);
  assert.deepEqual(permissions(split, workerAccount), ['get', 'list']);
  const combinedAccount = account(combined), combinedWorker = combinedAccount.slice(0, -4) + '-worker';
  assert.ok(!combined.some(o => o.metadata.name === combinedWorker + '-observer'), 'Disabled Worker retains no observer');
  assert.ok(!combined.some(o => o.kind === 'ServiceAccount' && o.metadata.name === combinedWorker));
  assert.deepEqual(permissions(combined, combinedAccount), ['get', 'list']);
  assert.ok(!split.concat(combined).some(o => o.kind === 'ClusterRoleBinding'), 'No cluster-scoped application grants');
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const [split, combined] = process.argv.slice(2);
  assert.ok(split && combined, 'Provide split and combined rendered Helm manifests');
  assertRbacProfiles(documents(fs.readFileSync(split, 'utf8')), documents(fs.readFileSync(combined, 'utf8')));
  console.log('Helm RBAC: Worker observation, API boundary and no Pod mutation passed');
}
