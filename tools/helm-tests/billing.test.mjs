import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';
import { documents } from '../check-helm-rbac.mjs';

const base = ['template','test','charts/neon-control-plane','-f','charts/neon-control-plane/ci/render-values.yaml'];
const render = (args=[]) => documents(execFileSync('helm',[...base,...args],{encoding:'utf8'}));
test('billing remains absent by default and payment keys never enter the Worker',()=>{
  const plain=render();
  for(const object of plain.filter(v=>v.kind==='Deployment')){
    const spec=object.spec.template.spec;
    assert.equal((spec.volumes||[]).some(v=>v.name==='billing'),false);
    assert.equal(spec.containers.some(c=>(c.env||[]).some(e=>e.name==='NEON_BILLING_CONFIG_FILE')),false);
  }
  const objects=render(['--set','billing.enabled=true','--set','billing.existingSecret=protected-billing','--set','backendCredentials.enabled=true','--set','backendCredentials.existingSecret=protected-backend','--set','backendCredentials.labHTTP=true']);
  for(const object of objects.filter(v=>v.kind==='Deployment')){
    const spec=object.spec.template.spec,isAPI=object.metadata.name.endsWith('-api');
    assert.equal((spec.volumes||[]).some(v=>v.name==='billing'),isAPI);
    assert.equal(spec.containers.some(c=>(c.env||[]).some(e=>e.name==='NEON_BILLING_CONFIG_FILE')),isAPI);
    if(isAPI){const volume=spec.volumes.find(v=>v.name==='billing');assert.equal(volume.secret.secretName,'protected-billing');assert.equal(volume.secret.defaultMode,288);assert.deepEqual(volume.secret.items,[{key:'config.json',path:'config.json'}]);assert.equal(spec.containers[0].volumeMounts.find(v=>v.name==='billing').readOnly,true);}
  }
  assert.equal(objects.some(v=>v.kind==='Secret'&&v.metadata.name==='protected-billing'),false);
});
test('billing rejects missing protected configuration, keys and process separation',()=>{
  for(const args of [ ['--set','billing.enabled=true'], ['--set','billing.enabled=true','--set','billing.existingSecret=protected-billing'], ['--set','billing.enabled=true','--set','billing.existingSecret=protected-billing','--set','backendCredentials.enabled=true','--set','backendCredentials.existingSecret=protected-backend','--set','backendCredentials.labHTTP=true','--set','worker.enabled=false'] ]) assert.throws(()=>execFileSync('helm',[...base,...args],{stdio:'pipe'}));
});
