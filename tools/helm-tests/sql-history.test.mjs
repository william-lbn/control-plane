import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import test from 'node:test';
const base=['template','history-test','charts/neon-control-plane','--namespace','neon','-f','charts/neon-control-plane/ci/render-values.yaml'];
function render(args=[]){return spawnSync(process.env.HELM_BIN||'helm',[...base,...args],{encoding:'utf8'});}
test('SQL history is opt-in and cannot start without retained encryption configuration',()=>{
 const disabled=render();assert.equal(disabled.status,0);assert.match(disabled.stdout,/NEON_V2_SQL_HISTORY_ENABLED,\s*value:\s*"false"/);
 const rejected=render(['--set','sqlHistory.enabled=true']);assert.notEqual(rejected.status,0);assert.match(rejected.stderr,/sqlHistory requires backendCredentials.enabled/);
 const enabled=render(['--set','sqlHistory.enabled=true','--set','backendCredentials.enabled=true','--set','backendCredentials.existingSecret=retained-versioned-keys','--set','api.cookieSecure=true']);assert.equal(enabled.status,0,enabled.stderr);assert.match(enabled.stdout,/NEON_V2_SQL_HISTORY_ENABLED,\s*value:\s*"true"/);assert.match(enabled.stdout,/retained-versioned-keys/);
});
