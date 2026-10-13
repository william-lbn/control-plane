import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import test from 'node:test';
import {documents} from '../check-helm-rbac.mjs';

const base=['template','test','charts/neon-control-plane','-f','charts/neon-control-plane/ci/render-values.yaml'];
const enabled=['--set','aiGateway.enabled=true','--set','aiGateway.labHTTP=true','--set','backendCredentials.enabled=true','--set','backendCredentials.existingSecret=backend-keys','--set','aiGateway.existingConfigSecret=ai-routes','--set','aiGateway.existingDatabaseSecret=ai-db','--set','aiGateway.image=example.invalid/control-ai@sha256:'+ 'a'.repeat(64)];
test('AI gateway is independent and keeps provider secrets out of the API',()=>{
 const objects=documents(execFileSync('helm',[...base,...enabled],{encoding:'utf8'}));
 const gateway=objects.find(o=>o.kind==='Deployment'&&o.metadata.name.endsWith('-ai')).spec.template.spec;
 assert.equal(gateway.automountServiceAccountToken,false);
 assert.equal(gateway.hostNetwork,undefined);
 assert.equal(gateway.containers[0].securityContext.readOnlyRootFilesystem,true);
 assert.equal(gateway.containers[0].env.some(e=>e.name==='NEON_V2_DATABASE_URL'),false);
 const api=objects.find(o=>o.kind==='Deployment'&&o.metadata.name.endsWith('-api')).spec.template.spec;
 assert.deepEqual(api.volumes.find(v=>v.name==='ai-config').secret.items,[{key:'config.json',path:'config.json'}]);
 assert.equal(api.volumes.some(v=>v.name==='ai-database'),false);
});
test('disabled gateway does not leave an unresolved nginx upstream',()=>{
 const objects=documents(execFileSync('helm',base,{encoding:'utf8'}));
 assert.equal(objects.some(o=>o.kind==='Deployment'&&o.metadata.name.endsWith('-ai')),false);
 assert.equal(objects.find(o=>o.kind==='ConfigMap'&&o.metadata.name.endsWith('-web')).data['nginx.conf'].includes('/ai/v1/'),false);
});
test('reject mutable image and unacknowledged laboratory HTTP',()=>{
 for(const overrides of [['--set','aiGateway.image=example.invalid/ai:latest'],['--set','aiGateway.labHTTP=false']]){
  assert.throws(()=>execFileSync('helm',[...base,...enabled,...overrides],{stdio:'pipe'}));
 }
});
