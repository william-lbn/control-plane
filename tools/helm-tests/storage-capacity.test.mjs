import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';
import { documents } from '../check-helm-rbac.mjs';
const base=['template','test','charts/neon-control-plane','-f','charts/neon-control-plane/ci/render-values.yaml','--set','objectStorage.enabled=true','--set','objectStorage.existingSecret=owned-storage'];
const render=(extra=[])=>documents(execFileSync('helm',[...base,...extra],{encoding:'utf8'}));
test('storage defaults retain 64 MiB admission and 256 MiB disk',()=>{
  const objects=render(),api=objects.find(v=>v.kind==='Deployment'&&v.metadata.name.endsWith('-api')).spec.template.spec;
  assert.equal(api.containers[0].env.find(v=>v.name==='NEON_STORAGE_OBJECT_BYTES').value,'67108864');
  assert.equal(api.volumes.find(v=>v.name==='product-storage-buffer').emptyDir.sizeLimit,'256Mi');
});
test('5 GiB storage profile has explicit branch disk and finite time budgets',()=>{
  const objects=render(['--set','objectStorage.capacity.objectBytes=5368709120','--set','objectStorage.capacity.branchBytes=21474836480','--set','objectStorage.capacity.bufferMiB=10240','--set','objectStorage.capacity.transferSeconds=1800']);
  for(const suffix of ['-api','-worker']){
    const spec=objects.find(v=>v.kind==='Deployment'&&v.metadata.name.endsWith(suffix)).spec.template.spec,env=spec.containers[0].env;
    assert.equal(env.find(v=>v.name==='NEON_STORAGE_OBJECT_BYTES').value,'5368709120');assert.equal(env.find(v=>v.name==='NEON_STORAGE_BUFFER_BYTES').value,'10737418240');assert.equal(env.find(v=>v.name==='NEON_STORAGE_TRANSFER_SECONDS').value,'1800');
    if(suffix==='-api')assert.equal(spec.volumes.find(v=>v.name==='product-storage-buffer').emptyDir.sizeLimit,'10240Mi');
  }
});
test('incomplete undersized or unbounded storage budgets are rejected',()=>{
  for(const extra of [['--set','objectStorage.capacity.objectBytes=5368709120'],['--set','objectStorage.capacity.objectBytes=5368709121'],['--set','objectStorage.capacity.branchBytes=8388608'],['--set','objectStorage.capacity.bufferMiB=16'],['--set','objectStorage.capacity.transferSeconds=3601']])assert.throws(()=>render(extra));
});
