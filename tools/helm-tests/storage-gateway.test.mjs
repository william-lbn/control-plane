import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';
import { documents } from '../check-helm-rbac.mjs';

const origin = 'http://192.168.146.100:30893';
const base = ['template', 'test', 'charts/neon-control-plane', '-f', 'charts/neon-control-plane/ci/render-values.yaml',
  '--set', 'objectStorage.enabled=true', '--set', 'objectStorage.existingSecret=owned-storage',
  '--set', 'objectStorage.s3.enabled=true', '--set', `objectStorage.s3.origin=${origin}`, '--set', 'objectStorage.s3.labHTTP=true',
  '--set', 'objectStorage.s3.serviceType=NodePort', '--set', 'objectStorage.s3.nodePort=30893',
  '--set', 'backendCredentials.enabled=true', '--set', 'backendCredentials.existingSecret=backend-keys'];
const takeover = ['--set', 'objectStorage.s3.apiListenerEnabled=false', '--set', 'objectStorage.s3.gatewayService=dedicated-storage.neon.svc.cluster.local'];
const render = (extra = []) => documents(execFileSync('helm', [...base, ...extra], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }));
const api = objects => objects.find(v => v.kind === 'Deployment' && v.metadata.name.endsWith('-api')).spec.template.spec.containers[0];
const nginx = objects => objects.find(v => v.kind === 'ConfigMap' && v.metadata.name.endsWith('-web')).data['nginx.conf'];
const storageLocation = config => {
  const match = config.match(/location \^~ \/storage\/v1\/ \{([\s\S]*?)\n    \}/);
  assert.ok(match, 'Signed URL path requires its own prefix location');
  return match[1];
};

test('legacy Storage retains its listener NodePort and signed REST API upstream', () => {
  const objects = render();
  const service = objects.find(v => v.kind === 'Service' && v.metadata.name.endsWith('-s3'));
  assert.equal(service.spec.ports[0].nodePort, 30893);
  assert.equal(api(objects).env.find(v => v.name === 'NEON_S3_LISTENER_ENABLED').value, 'true');
  assert.ok(api(objects).ports.some(v => v.name === 's3'));
  assert.match(storageLocation(nginx(objects)), /proxy_pass http:\/\/neon-control-api:8788; # storage-public-upstream/);
});

test('independent takeover releases the old NodePort and preserves the signed origin', () => {
  const objects = render(takeover);
  assert.equal(objects.some(v => v.kind === 'Service' && v.metadata.name.endsWith('-s3')), false);
  assert.equal(api(objects).ports.some(v => v.name === 's3'), false);
  assert.equal(api(objects).env.find(v => v.name === 'NEON_S3_LISTENER_ENABLED').value, 'false');
  assert.equal(api(objects).env.find(v => v.name === 'NEON_S3_PUBLIC_ORIGIN').value, origin);
  const config = nginx(objects), location = storageLocation(config);
  assert.match(location, /proxy_pass http:\/\/dedicated-storage\.neon\.svc\.cluster\.local:8793; # storage-public-upstream/);
  assert.match(location, /proxy_set_header Cookie "";/);
  assert.match(location, /proxy_set_header Authorization "";/);
  assert.match(location, /proxy_hide_header Set-Cookie;/);
  assert.match(location, /error_log \/dev\/null;/);
  assert.match(location, /proxy_request_buffering off;/);
  assert.doesNotMatch(location, /rewrite\s|proxy_pass http:\/\/[^;\s]+\//);
  assert.match(config, /location ~ \^\/\(api\/\|auth\/\|data\/v1\/\) \{/);
  assert.match(config, /proxy_pass http:\/\/neon-control-api:8788;/);
});

test('signed REST proxy budgets follow the configured Storage payload and transfer deadline', () => {
  const config = nginx(render([...takeover, '--set', 'objectStorage.capacity.objectBytes=134217728',
    '--set', 'objectStorage.capacity.branchBytes=536870912', '--set', 'objectStorage.capacity.bufferMiB=256',
    '--set', 'objectStorage.capacity.transferSeconds=300']));
  const location = storageLocation(config);
  assert.match(location, /client_max_body_size 135266304;/);
  assert.match(location, /proxy_read_timeout 310s;/);
  assert.match(location, /proxy_send_timeout 310s;/);
});

test('incomplete mixed and injected Storage takeover configuration is rejected', () => {
  for (const extra of [
    ['--set', 'objectStorage.s3.apiListenerEnabled=false'],
    ['--set', 'objectStorage.s3.gatewayService=dedicated-storage'],
    [...takeover, '--set', 'objectStorage.s3.enabled=false'],
    [...takeover, '--set', 'objectStorage.s3.gatewayService=http://external.example'],
    [...takeover, '--set', 'objectStorage.s3.gatewayService=dedicated-storage:8793'],
    [...takeover, '--set', 'objectStorage.s3.gatewayService=dedicated-storage/path'],
    [...takeover, '--set', 'objectStorage.s3.gatewayService=dedicated..storage'],
  ]) assert.throws(() => render(extra));
});

test('standalone chart owns the released NodePort with exact endpoint Secret GET permissions', () => {
  const objects = documents(execFileSync('helm', ['template', 'dedicated', 'charts/storage-gateway', '-n', 'neon',
    '-f', 'charts/storage-gateway/ci/render-values.yaml', '--set', 'laboratory.allowHTTP=true', '--set', 'ingress.enabled=false',
    '--set', `publicOrigin=${origin}`, '--set', 'service.type=NodePort', '--set', 'service.nodePort=30893'], { encoding: 'utf8' }));
  const service = objects.find(v => v.kind === 'Service');
  assert.equal(service.metadata.name, 'dedicated-storage');
  assert.equal(service.spec.ports[0].nodePort, 30893);
  assert.equal(service.spec.ports[0].port, 8793);
  assert.equal(service.spec.ports.some(v => v.port === 8794), false);
  assert.deepEqual(objects.find(v => v.kind === 'Role').rules, [{ apiGroups: [''], resources: ['secrets'],
    resourceNames: ['cp-0123456789abcdef-credentials'], verbs: ['get'] }]);
});
