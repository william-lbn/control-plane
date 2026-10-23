import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';
import { documents } from '../check-helm-rbac.mjs';

const base = ['template', 'test', 'charts/neon-control-plane', '-f', 'charts/neon-control-plane/ci/render-values.yaml'];
const keys = ['--set', 'backendCredentials.enabled=true', '--set', 'backendCredentials.existingSecret=retained-keys'];
test('Console OIDC client config is API-only and disabled by default', () => {
  const disabled = documents(execFileSync('helm', base, { encoding: 'utf8' }));
  assert(disabled.filter((o) => o.kind === 'Deployment').every((o) => !(o.spec.template.spec.volumes || []).some((v) => v.name === 'console-oidc')));
  const objects = documents(execFileSync('helm', [...base, ...keys, '--set', 'consoleSSO.enabled=true', '--set', 'consoleSSO.existingSecret=operator-oidc'], { encoding: 'utf8' }));
  for (const object of objects.filter((o) => o.kind === 'Deployment')) {
    const pod = object.spec.template.spec;
    const mounted = (pod.volumes || []).find((v) => v.name === 'console-oidc');
    if (object.metadata.name.endsWith('-api')) {
      assert.equal(mounted.secret.secretName, 'operator-oidc');
      assert.deepEqual(mounted.secret.items, [{ key: 'config.json', path: 'config.json' }]);
      assert.equal(pod.containers[0].env.find((e) => e.name === 'NEON_CONSOLE_OIDC_CONFIG_FILE').value, '/run/console-oidc/config.json');
    } else assert.equal(mounted, undefined);
  }
  const nginx = objects.find((o) => o.kind === 'ConfigMap' && o.metadata.name.endsWith('-web')).data['nginx.conf'];
  assert.match(nginx, /location \^~ \/auth\/sso\/ \{[\s\S]*?error_log \/dev\/null;/);
  assert.equal(objects.some((o) => o.kind === 'Secret'), false);
});
test('Console SSO rejects missing keys and missing operator config', () => {
  for (const [settings, message] of [[['--set', 'consoleSSO.enabled=true'], /consoleSSO requires backendCredentials/], [[...keys, '--set', 'consoleSSO.enabled=true'], /consoleSSO.existingSecret required/]])
    assert.throws(() => execFileSync('helm', [...base, ...settings], { stdio: 'pipe' }), (error) => message.test(error.stderr.toString()));
});
