import test from 'node:test';
import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
test('console mapping preserves stdout stderr and bounded inspection without changing runtime globals in tests',()=>{
 const code=`import {installOutputConsole} from ${JSON.stringify(new URL('../runtime/output.mjs',import.meta.url).href)};installOutputConsole();console.debug('debug');console.log('info');console.warn('warn');console.error('error');`;
 const result=spawnSync(process.execPath,['--input-type=module','-e',code],{encoding:'utf8',timeout:5000});assert.equal(result.status,0);
 const parse=s=>s.trim().split('\n').map(line=>JSON.parse(line.replace('@neon-app-log-v1:','')));
 assert.deepEqual(parse(result.stdout).map(v=>v.level),['DEBUG','INFO']);assert.deepEqual(parse(result.stderr).map(v=>v.level),['WARN','ERROR']);
});
