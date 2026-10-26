import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import test from 'node:test';
import { documents } from '../check-helm-rbac.mjs';

const origin = 'https://functions.example.test';
const base = ['template', 'test', 'charts/neon-control-plane', '--namespace', 'neon',
  '-f', 'charts/neon-control-plane/ci/render-values.yaml',
  '--set', 'functions.enabled=true', '--set', 'functions.existingConfigSecret=functions-common',
  '--set', 'functions.existingIssuerSecret=functions-issuer', '--set', 'functions.proxyCASecret=proxy-public-ca',
  '--set', 'functions.publicHTTP.enabled=true', '--set', `functions.publicHTTP.origin=${origin}`,
  '--set', 'functions.publicHTTP.serviceType=NodePort', '--set', 'functions.publicHTTP.nodePort=30892',
  '--set', 'managedAuth.publicOrigin=https://console.example.test',
  '--set', 'objectStorage.enabled=true', '--set', 'objectStorage.existingSecret=product-storage',
  '--set', 'backendCredentials.enabled=true', '--set', 'backendCredentials.existingSecret=backend-keys'];
const enabledArgs = ['--set', 'functions.edge.enabled=true',
  '--set', 'functions.edge.existingClientSecret=dedicated-edge-client',
  '--set', 'functions.edge.existingBrokerSecret=dedicated-edge-broker'];
const render = (extra = []) => documents(execFileSync('helm', [...base, ...extra], {
  encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
}));
let enabledObjects, legacyObjects;
const enabled = () => enabledObjects ??= render(enabledArgs);
const legacy = () => legacyObjects ??= render();
const deployment = (objects, component) => objects.find(v => v.kind === 'Deployment'
  && v.spec.template.metadata.labels['app.kubernetes.io/component'] === component);
const environment = container => Object.fromEntries(container.env.map(v => [v.name, v.value]));
const named = (objects, kind, suffix) => objects.find(v => v.kind === kind && v.metadata.name.endsWith(suffix));

test('Functions edge routes the public Service to its process and gives API a private TLS broker', () => {
  const objects = enabled();
  const publicService = named(objects, 'Service', '-functions-http');
  assert.equal(publicService.spec.selector['app.kubernetes.io/component'], 'function-edge');
  assert.equal(publicService.spec.ports[0].port, 8792);
  assert.equal(publicService.spec.ports[0].nodePort, 30892);
  const apiSpec = deployment(objects, 'api').spec.template.spec;
  const api = apiSpec.containers[0], env = environment(api);
  assert.equal(env.NEON_FUNCTIONS_EDGE_ENABLED, 'true');
  assert.equal(env.NEON_FUNCTIONS_PUBLIC_ORIGIN, origin);
  assert.equal(env.NEON_FUNCTIONS_EDGE_BROKER_BIND, ':8795');
  assert.equal(env.NEON_FUNCTIONS_EDGE_BROKER_CERT_FILE, '/run/function-edge-broker/tls.crt');
  assert.equal(env.NEON_FUNCTIONS_EDGE_BROKER_KEY_FILE, '/run/function-edge-broker/tls.key');
  assert.equal(env.NEON_FUNCTIONS_EDGE_CLIENT_CA_FILE, '/run/function-edge-broker/client-ca.crt');
  assert.ok(api.ports.some(v => v.name === 'edge-broker' && v.containerPort === 8795));
  assert.equal(api.ports.some(v => v.name === 'functions-http'), false);
  assert.deepEqual(apiSpec.volumes.find(v => v.name === 'function-edge-broker').secret, {
    secretName: 'dedicated-edge-broker', defaultMode: 288,
    items: [{ key: 'tls.crt', path: 'tls.crt' }, { key: 'tls.key', path: 'tls.key' }, { key: 'client-ca.crt', path: 'client-ca.crt' }],
  });
  assert.equal(api.volumeMounts.find(v => v.name === 'function-edge-broker').readOnly, true);
  const broker = named(objects, 'Service', '-function-edge-broker');
  assert.equal(broker.spec.type, 'ClusterIP');
  assert.equal(broker.spec.selector['app.kubernetes.io/component'], 'api');
  assert.deepEqual(broker.spec.ports, [{ name: 'edge-broker', port: 8795, targetPort: 8795 }]);
  assert.equal(broker.spec.ports.some(v => 'nodePort' in v), false);
});

test('Functions edge projects only its client leaf and public guest CA without control credentials or RBAC', () => {
  const objects = enabled(), object = deployment(objects, 'function-edge');
  const spec = object.spec.template.spec, container = spec.containers[0], env = environment(container);
  assert.equal(object.spec.replicas, 1);
  assert.equal(object.spec.strategy.type, 'Recreate');
  assert.deepEqual(container.command, ['/opt/neon-control/control-function-edge']);
  assert.equal(spec.automountServiceAccountToken, false);
  const account = objects.find(v => v.kind === 'ServiceAccount' && v.metadata.name === spec.serviceAccountName);
  assert.equal(account.automountServiceAccountToken, false);
  assert.equal(objects.filter(v => v.kind === 'RoleBinding' || v.kind === 'ClusterRoleBinding')
    .some(v => v.subjects?.some(s => s.kind === 'ServiceAccount' && s.name === spec.serviceAccountName)), false);
  assert.equal(spec.securityContext.runAsNonRoot, true);
  assert.equal(container.securityContext.readOnlyRootFilesystem, true);
  assert.equal(container.securityContext.allowPrivilegeEscalation, false);
  assert.deepEqual(container.securityContext.capabilities.drop, ['ALL']);
  assert.deepEqual(container.volumeMounts.map(v => v.name).sort(), ['edge-identity', 'guest-ca']);
  assert.ok(container.volumeMounts.every(v => v.readOnly));
  assert.deepEqual(spec.volumes.map(v => v.name).sort(), ['edge-identity', 'guest-ca']);
  assert.deepEqual(spec.volumes.find(v => v.name === 'edge-identity').secret, {
    secretName: 'dedicated-edge-client', defaultMode: 288,
    items: [{ key: 'tls.crt', path: 'tls.crt' }, { key: 'tls.key', path: 'tls.key' }, { key: 'broker-ca.crt', path: 'broker-ca.crt' }],
  });
  assert.deepEqual(spec.volumes.find(v => v.name === 'guest-ca').secret, {
    secretName: 'functions-common', defaultMode: 288, items: [{ key: 'guest-ca.crt', path: 'guest-ca.crt' }],
  });
  assert.equal(env.NEON_FUNCTIONS_EDGE_CLIENT_CERT_FILE, '/run/function-edge/tls.crt');
  assert.equal(env.NEON_FUNCTIONS_EDGE_CLIENT_KEY_FILE, '/run/function-edge/tls.key');
  assert.equal(env.NEON_FUNCTIONS_EDGE_BROKER_CA_FILE, '/run/function-edge/broker-ca.crt');
  assert.equal(env.NEON_FUNCTIONS_EDGE_GUEST_CA_FILE, '/run/function-guest-ca/guest-ca.crt');
  assert.equal(env.NEON_FUNCTIONS_EDGE_BROKER_ORIGIN, 'https://neon-control-function-edge-broker.neon.svc:8795');
  assert.equal(env.NEON_FUNCTIONS_PUBLIC_ORIGIN, origin);
  assert.doesNotMatch(JSON.stringify(spec), /guest-ca\.key|backing-mac|keyring\.json|DATABASE_URL|POSTGRES|KUBERNETES|serviceAccountToken|functions-issuer|product-storage|neon-secrets/);
  const worker = deployment(objects, 'worker').spec.template.spec;
  assert.equal(worker.volumes.some(v => v.name === 'function-edge-broker' || v.name === 'edge-identity'), false);
  // Helm cannot inspect externally retained certificate bytes. The deployment
  // contract assigns ClientAuth to this leaf; the broker verifies client chains.
  const contract = JSON.parse(fs.readFileSync('contracts/functions-edge-v1.json', 'utf8'));
  assert.deepEqual(contract.deployment.client_leaf_extended_key_usage, ['ClientAuth']);
  assert.deepEqual(contract.deployment.broker_leaf_extended_key_usage, ['ServerAuth']);
  assert.equal(contract.private_broker.client_common_name, 'neon-function-edge');
});

test('Functions edge exposes only public 8792 and limits egress to broker guest TLS and cluster DNS', () => {
  const objects = enabled(), spec = deployment(objects, 'function-edge').spec.template.spec;
  assert.deepEqual(spec.containers[0].ports, [{ name: 'functions-http', containerPort: 8792 }]);
  const policy = named(objects, 'NetworkPolicy', '-function-edge-egress');
  assert.equal(policy.spec.podSelector.matchLabels['app.kubernetes.io/component'], 'function-edge');
  assert.deepEqual(policy.spec.policyTypes, ['Egress']);
  assert.deepEqual(policy.spec.egress, [
    { to: [{ podSelector: { matchLabels: { 'app.kubernetes.io/instance': 'test', 'app.kubernetes.io/component': 'api' } } }], ports: [{ protocol: 'TCP', port: 8795 }] },
    { to: [{ podSelector: { matchExpressions: [{ key: 'vm.neon.tech/name', operator: 'Exists' }] } }], ports: [{ protocol: 'TCP', port: 9090 }] },
    { to: [{ namespaceSelector: { matchLabels: { 'kubernetes.io/metadata.name': 'kube-system' } } }], ports: [{ protocol: 'UDP', port: 53 }, { protocol: 'TCP', port: 53 }] },
  ]);
  assert.equal(objects.filter(v => v.kind === 'Service' && v.spec.selector?.['app.kubernetes.io/component'] === 'function-edge')
    .flatMap(v => v.spec.ports).some(v => v.port === 8795 || v.port === 9090), false);
});

test('disabled Functions edge preserves the legacy public route without leaf or broker projections', () => {
  const objects = legacy(), spec = deployment(objects, 'api').spec.template.spec;
  assert.equal(deployment(objects, 'function-edge'), undefined);
  assert.equal(named(objects, 'ServiceAccount', '-function-edge'), undefined);
  assert.equal(named(objects, 'Service', '-function-edge-broker'), undefined);
  assert.equal(named(objects, 'NetworkPolicy', '-function-edge-egress'), undefined);
  const service = named(objects, 'Service', '-functions-http');
  assert.equal(service.spec.selector['app.kubernetes.io/component'], 'api');
  assert.equal(service.spec.ports[0].nodePort, 30892);
  assert.ok(spec.containers[0].ports.some(v => v.name === 'functions-http' && v.containerPort === 8792));
  assert.equal(spec.containers[0].ports.some(v => v.name === 'edge-broker'), false);
  assert.equal(environment(spec.containers[0]).NEON_FUNCTIONS_PUBLIC_ORIGIN, origin);
  assert.equal(spec.containers[0].env.some(v => v.name.startsWith('NEON_FUNCTIONS_EDGE_')), false);
  assert.equal(spec.volumes.some(v => v.name === 'function-edge-broker'), false);
});

test('Functions edge schema and template reject unknown fields missing trust and unsupported process layouts', () => {
  for (const extra of [
    ['--set', 'functions.edge.existingClientSecret='],
    ['--set', 'functions.edge.existingBrokerSecret='],
    ['--set', 'functions.edge.replicaCount=2'],
    ['--set', 'functions.edge.unknownSetting=true'],
    ['--set', 'functions.edge.existingClientSecret=https://external.example'],
    ['--set', 'functions.edge.existingBrokerSecret=invalid..secret'],
    ['--set', 'functions.publicHTTP.enabled=false'],
    ['--set', 'functions.enabled=false'],
    ['--set', 'worker.enabled=false'],
    ['--set', 'objectStorage.enabled=false'],
  ]) assert.throws(() => render([...enabledArgs, ...extra]), /./);
});
