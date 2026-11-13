import assert from 'node:assert/strict';
import fs from 'node:fs';
import { parseAllDocuments } from 'yaml';

function documents(file) {
  return parseAllDocuments(fs.readFileSync(file, 'utf8'), { uniqueKeys: true }).map(doc => {
    assert.equal(doc.errors.length, 0, `${file}: RBAC manifest must be valid unambiguous YAML`);
    return doc.toJSON();
  }).filter(Boolean);
}

function find(objects, kind, name, namespace) {
  const object = objects.find(item => item.kind === kind && item.metadata?.name === name && (namespace === undefined || item.metadata?.namespace === namespace));
  assert.ok(object, `missing ${kind} ${namespace ?? '<cluster>'}/${name}`);
  return object;
}

function roleBinding(objects, {name, namespace, roleName, serviceAccount}) {
  const object = find(objects, 'RoleBinding', name, namespace);
  assert.deepEqual(object.roleRef, {apiGroup: 'rbac.authorization.k8s.io', kind: 'Role', name: roleName});
  assert.deepEqual(object.subjects, [{kind: 'ServiceAccount', name: serviceAccount, namespace: 'neon'}]);
  return object;
}

function clusterRoleBinding(objects, {name, roleName, serviceAccount}) {
  const object = find(objects, 'ClusterRoleBinding', name);
  assert.deepEqual(object.roleRef, {apiGroup: 'rbac.authorization.k8s.io', kind: 'ClusterRole', name: roleName});
  assert.deepEqual(object.subjects, [{kind: 'ServiceAccount', name: serviceAccount, namespace: 'neon'}]);
  return object;
}

const [splitPath, combinedPath] = process.argv.slice(2);
assert.ok(splitPath && combinedPath, 'Provide split and combined DSGA-enabled Helm manifests');

for (const [profile, file, combined] of [['split', splitPath, false], ['combined', combinedPath, true]]) {
  const objects = documents(file);
  const api = objects.find(item => item.kind === 'Deployment' && item.metadata?.name?.endsWith('-api'));
  assert.ok(api, `${profile}: API Deployment required`);
  const apiSA = api.spec.template.spec.serviceAccountName;
  const base = apiSA.slice(0, -4);
  const worker = objects.find(item => item.kind === 'Deployment' && item.metadata?.name === `${base}-worker`);
  const workerSA = worker?.spec.template.spec.serviceAccountName ?? apiSA;
  assert.equal(Boolean(worker), !combined, `${profile}: Worker deployment profile mismatch`);

  roleBinding(objects, {name: `${base}-dsga-neon-api`, namespace: 'neon', roleName: 'neon-control-runtime-neon-runtime-access-api', serviceAccount: apiSA});
  roleBinding(objects, {name: `${base}-dsga-neon-worker`, namespace: 'neon', roleName: 'neon-control-runtime-neon-runtime-access-worker', serviceAccount: workerSA});
  roleBinding(objects, {name: `${base}-dsga-platform-policy`, namespace: 'neon-control-runtime', roleName: `${base}-dsga-platform-policy`, serviceAccount: apiSA});
  roleBinding(objects, {name: `${base}-dsga-platform-worker`, namespace: 'neon-control-runtime', roleName: 'neon-control-runtime-neon-runtime-access-platform-worker', serviceAccount: workerSA});
  roleBinding(objects, {name: `${base}-dsga-agent-observation`, namespace: 'kube-system', roleName: 'neon-control-runtime-neon-runtime-access-agent-observation', serviceAccount: apiSA});

  const policy = find(objects, 'Role', `${base}-dsga-platform-policy`, 'neon-control-runtime');
  assert.deepEqual(policy.rules, [{apiGroups: [''], resources: ['configmaps'], verbs: ['get', 'update']}]);
  clusterRoleBinding(objects, {name: `${base}-dsga-node-read`, roleName: 'neon-control-runtime-worker-nodes', serviceAccount: workerSA});
  clusterRoleBinding(objects, {name: `${base}-dsga-runner-delegation`, roleName: 'neon-control-runtime-neon-runtime-access-runner-delegation', serviceAccount: workerSA});

  const actualClusterBindings = objects.filter(item => item.kind === 'ClusterRoleBinding').map(item => item.metadata.name).sort();
  assert.deepEqual(actualClusterBindings, [`${base}-dsga-node-read`, `${base}-dsga-runner-delegation`].sort(), `${profile}: only the two reviewed DSGA ClusterRoleBindings are allowed`);
  assert.equal(objects.some(item => item.kind === 'ClusterRole'), false, `${profile}: chart must not create cluster-scoped roles`);
}

console.log('DSGA RBAC: split and combined profiles, per-namespace grants, Agent observation, fixed Node read and Runner delegation passed');
