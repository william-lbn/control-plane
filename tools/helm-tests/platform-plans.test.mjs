import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';
import { documents } from '../check-helm-rbac.mjs';

const base=['template','test','charts/neon-control-plane','-f','charts/neon-control-plane/ci/render-values.yaml'];
const approved=['--set','platformManagement.enabled=true','--set','platformManagement.operatorIds[0]=approved_operator','--set','platformManagement.allowedOrganizations[0]=org_owned'];
const render=(args=[])=>documents(execFileSync('helm',[...base,...args],{encoding:'utf8'}));
test('platform plan policy is absent until explicitly enabled',()=>{
  const objects=render();
  assert.equal(objects.some((v)=>v.kind==='ConfigMap'&&v.metadata.name.includes('-platform-')),false);
  const api=objects.find((v)=>v.kind==='Deployment'&&v.metadata.name.endsWith('-api')).spec.template.spec;
  assert.equal(api.containers[0].env.some((v)=>v.name==='NEON_PLATFORM_POLICY_FILE'),false);
  assert.equal(api.volumes.some((v)=>v.name==='platform-policy'),false);
});
test('approved policy is immutable, content named and projected only into the API',()=>{
  const objects=render(approved),policy=objects.find((v)=>v.kind==='ConfigMap'&&v.metadata.name.includes('-platform-'));
  assert.equal(policy.immutable,true);assert.equal(policy.metadata.annotations['helm.sh/resource-policy'],'keep');
  const value=JSON.parse(policy.data['config.json']);assert.deepEqual(value,{version:1,operator_ids:['approved_operator'],allowed_organizations:['org_owned'],plan_management:true});
  const api=objects.find((v)=>v.kind==='Deployment'&&v.metadata.name.endsWith('-api')).spec.template.spec;
  assert.equal(api.volumes.find((v)=>v.name==='platform-policy').configMap.name,policy.metadata.name);
  assert.equal(api.containers[0].volumeMounts.find((v)=>v.name==='platform-policy').readOnly,true);
  const changed=render([...approved,'--set','platformManagement.operatorIds[0]=another_operator']).find((v)=>v.kind==='ConfigMap'&&v.metadata.name.includes('-platform-'));
  assert.notEqual(changed.metadata.name,policy.metadata.name);
  for(const object of objects.filter((v)=>v.kind==='Deployment'&&!v.metadata.name.endsWith('-api')))assert.equal(object.spec.template.spec.containers.some((c)=>(c.env||[]).some((e)=>e.name==='NEON_PLATFORM_POLICY_FILE')),false);
});
test('empty, duplicate and malformed platform operator approval is rejected',()=>{
  for(const args of [ ['--set','platformManagement.enabled=true'], [...approved,'--set','platformManagement.operatorIds[1]=approved_operator'], [...approved,'--set','platformManagement.operatorIds[0]=bad@example.invalid'], [...approved,'--set','platformManagement.allowedOrganizations[1]=org_owned'] ])assert.throws(()=>execFileSync('helm',[...base,...args],{stdio:'pipe'}));
});
