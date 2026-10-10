import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import { assertRbacProfiles, documents } from '../check-helm-rbac.mjs';

const fresh = () => [documents(fs.readFileSync('artifacts/helm/control-plane.yaml', 'utf8')),
  documents(fs.readFileSync('artifacts/helm/combined-profile.yaml', 'utf8'))];
const observer = objects => objects.find(o => o.kind === 'Role' && o.metadata.name.endsWith('-worker-observer'));
const apiRole = objects => objects.find(o => o.kind === 'Role' && o.metadata.name.endsWith('-api'));
const podRule = role => role.rules.find(r => r.apiGroups.includes('') && r.resources.includes('pods'));

test('both actual rendered Helm profiles preserve the observer boundary', () => {
  assertRbacProfiles(...fresh());
});
test('reject missing Worker observation after a Helm change', () => {
  const [split,combined]=fresh(); const index=split.indexOf(observer(split)); split.splice(index,1);
  assert.throws(()=>assertRbacProfiles(split,combined));
});
test('reject accidentally binding Worker observation to API', () => {
  const [split,combined]=fresh();
  const binding=split.find(o=>o.kind==='RoleBinding'&&o.metadata.name.endsWith('-worker-observer'));
  binding.subjects[0].name=binding.subjects[0].name.replace(/-worker$/,'-api');
  assert.throws(()=>assertRbacProfiles(split,combined));
});
test('reject API Pod listing in the split profile', () => {
  const [split,combined]=fresh();podRule(apiRole(split)).verbs.push('list');
  assert.throws(()=>assertRbacProfiles(split,combined));
});
test('reject Pod deletion even through the shared API role', () => {
  const [split,combined]=fresh();podRule(apiRole(split)).verbs.push('delete');
  assert.throws(()=>assertRbacProfiles(split,combined));
});
test('reject wildcard application grants', () => {
  const [split,combined]=fresh();apiRole(split).rules.push({apiGroups:['*'],resources:['*'],verbs:['*']});
  assert.throws(()=>assertRbacProfiles(split,combined));
});
test('reject missing combined process list permission', () => {
  const [split,combined]=fresh();podRule(apiRole(combined)).verbs=['get'];
  assert.throws(()=>assertRbacProfiles(split,combined));
});
test('reject Pod exec grants on observation', () => {
  const [split,combined]=fresh();observer(split).rules.push({apiGroups:[''],resources:['pods/exec'],verbs:['create']});
  assert.throws(()=>assertRbacProfiles(split,combined));
});
