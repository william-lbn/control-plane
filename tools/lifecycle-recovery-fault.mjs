#!/usr/bin/env node
// Trusted Linux operator tool. Browser code never receives Kubernetes credentials.
// Inject a terminal status only for an explicitly identified, freshly queued
// ci-lifecycle recovery. Native timelines, user data and Secrets are untouched.
import fs from 'node:fs';
import path from 'node:path';
import {parseArgs} from 'node:util';
import {spawnSync} from 'node:child_process';
import {setTimeout as delay} from 'node:timers/promises';

const {values} = parseArgs({options: {'fixture-dir': {type: 'string'}, 'evidence-dir': {type: 'string'}}});
if (process.platform !== 'linux' || !values['fixture-dir'] || !values['evidence-dir']) throw new Error('Explicit Linux fixture/evidence directories required');
const fixture = path.resolve(values['fixture-dir']);
const evidence = path.resolve(values['evidence-dir']);
const owner = fs.lstatSync(fixture);
if (!owner.isDirectory() || owner.isSymbolicLink() || (owner.mode & 0o077)) throw new Error('Private nonsymlink fixture directory required');
fs.mkdirSync(evidence, {mode: 0o700}); // Exclusive attempt; never overwrite an earlier report.
const report = {result: 'running', mode: 'operator_injected_terminal_status', steps: [], dataDeleted: false, secretsChanged: false};
const save = () => fs.writeFileSync(path.join(evidence, 'result.json'), JSON.stringify(report, null, 2) + '\n', {mode: 0o600});
let stopped = false;
let deploymentUID;
let aborted = false;
process.on('SIGTERM', () => {aborted = true;});
process.on('SIGINT', () => {aborted = true;});
function kube(args, timeout = 45000) {
  const server = process.env.NEON_E2E_KUBE_SERVER;
  if (server && !/^https:\/\/[a-zA-Z0-9.:-]+$/.test(server)) throw new Error('Invalid explicit Kubernetes server');
  const out = spawnSync(process.env.KUBECTL_BIN || 'kubectl', [...(server ? ['--server=' + server] : []), '--request-timeout=30s', '-n', 'neon', ...args], {encoding: 'utf8', timeout, maxBuffer: 1024 * 1024});
  if (out.error || out.status !== 0) throw new Error('Operator Kubernetes step failed');
  return out.stdout.trim();
}
function sql(statement) {
  // All interpolated IDs pass fixed allowlists below. PGPASSWORD is expanded
  // solely inside the existing PostgreSQL container, never on the operator host.
  const quote = s => "'" + s.replaceAll("'", "'\"'\"'") + "'";
  return kube(['exec', 'deployment/neon-control-v2-db', '--', 'sh', '-c',
    'PGPASSWORD="$POSTGRES_PASSWORD" psql -U neon_control_v2 -d neon_control_v2 -At -v ON_ERROR_STOP=1 -c ' + quote(statement)]);
}
function worker() {return JSON.parse(kube(['get', 'deployment', 'neon-control-worker', '-o', 'json']));}
function scale(replicas) {
  const d = worker();
  const labels = d.spec.selector?.matchLabels;
  if (d.metadata.uid !== deploymentUID || d.metadata.labels?.['app.kubernetes.io/instance'] !== 'neon-control-plane' || labels?.['app.kubernetes.io/component'] !== 'worker' || labels?.['app.kubernetes.io/instance'] !== 'neon-control-plane') throw new Error('Worker identity changed');
  kube(['patch', 'deployment', 'neon-control-worker', '--type=json', '-p', JSON.stringify([
    {op: 'test', path: '/metadata/uid', value: deploymentUID},
    {op: 'test', path: '/metadata/resourceVersion', value: d.metadata.resourceVersion},
    {op: 'replace', path: '/spec/replicas', value: replicas},
  ])]);
}
async function marker(name, seconds) {
  const file = path.join(fixture, name);
  const deadline = Date.now() + seconds * 1000;
  while (Date.now() < deadline && !aborted) {
    try {
      const stat = fs.lstatSync(file);
      if (!stat.isFile() || stat.isSymbolicLink() || stat.size > 1024 || (stat.mode & 0o077)) throw new Error('Unsafe marker');
      return JSON.parse(fs.readFileSync(file, 'utf8'));
    } catch (e) {if (e.code !== 'ENOENT') throw e;}
    await delay(1000);
  }
  throw new Error('Fixture marker deadline or operator interruption');
}
save();
try {
  const prepared = await marker('lifecycle-recovery-prepare.json', 1500);
  const project = prepared.project_id;
  if (!/^prj_[a-f0-9]{16}$/.test(project)) throw new Error('Invalid fixture project');
  if (sql(`SELECT count(*) FROM projects WHERE id='${project}' AND source='managed' AND name LIKE 'ci-lifecycle-%' AND state='deleted' AND deleted_at IS NOT NULL`) !== '1') throw new Error('Owned completed deletion required');
  if (sql("SELECT count(*) FROM operations WHERE state IN ('queued','running','retry_wait')") !== '0') throw new Error('Operation queue must be drained');
  const before = worker();
  if (before.spec.replicas !== 1 || before.status.readyReplicas !== 1) throw new Error('Single ready Worker required');
  deploymentUID = before.metadata.uid;
  report.projectID = project;
  report.workerUID = deploymentUID;
  report.previousEpoch = Number(sql("SELECT epoch FROM control_runtime_leases WHERE name='controllers'"));
  if (!Number.isSafeInteger(report.previousEpoch) || report.previousEpoch < 1) throw new Error('Valid existing leader epoch required');
  stopped = true; // Finally restores even if the scale result is uncertain.
  scale(0);
  kube(['wait', '--for=delete', 'pods', '-l', 'app.kubernetes.io/instance=neon-control-plane,app.kubernetes.io/component=worker', '--timeout=120s'], 150000);
  const ack = path.join(fixture, 'lifecycle-recovery-worker-paused.json');
  fs.writeFileSync(ack, JSON.stringify({project_id: project}), {flag: 'wx', mode: 0o600});
  if (process.getuid() === 0) fs.chownSync(ack, owner.uid, owner.gid);
  report.steps.push('owned_worker_paused_after_delete'); save();
  const queued = await marker('lifecycle-recovery-queued.json', 180);
  const op = queued.operation_id;
  if (queued.project_id !== project || !/^op_[a-f0-9]{24}$/.test(op)) throw new Error('Recovery identity mismatch');
  const scope = `o.id='${op}' AND o.project_id='${project}' AND o.action='recover_project' AND o.state='queued' AND o.lease_owner IS NULL AND p.id=o.project_id AND p.state='recovering' AND p.deleted_at IS NOT NULL AND p.source='managed' AND p.name LIKE 'ci-lifecycle-%'`;
  const prior = sql(`SELECT row_to_json(x) FROM (SELECT o.id,o.project_id,o.action,o.state,p.state AS project_state FROM operations o JOIN projects p ON p.id=o.project_id WHERE ${scope}) x`);
  if (!prior) throw new Error('Original queued recovery required');
  fs.writeFileSync(path.join(evidence, 'before.json'), prior + '\n', {flag: 'wx', mode: 0o600});
  const changed = sql(`UPDATE operations o SET state='failed',retryable=true,error_code='controlled_test_failure',error_message='Controlled terminal recovery status for UI retry acceptance',finished_at=now() FROM projects p WHERE ${scope} RETURNING o.id`);
  if (changed.split('\n')[0] !== op) throw new Error('Exactly one owned recovery must change');
  report.operationID = op; report.steps.push('owned_terminal_status_injected'); save();
  scale(1);
  kube(['rollout', 'status', 'deployment/neon-control-worker', '--timeout=120s'], 150000);
  stopped = false;
  report.successorEpoch = Number(sql("SELECT epoch FROM control_runtime_leases WHERE name='controllers'"));
  if (!Number.isSafeInteger(report.successorEpoch) || report.successorEpoch <= report.previousEpoch) throw new Error('Successor leader epoch must advance');
  report.result = 'pass';
} catch (e) {
  report.result = 'fail'; report.error = e.message; process.exitCode = 1;
} finally {
  if (stopped) {
    try {scale(1); report.workerRestoredAfterFailure = true;}
    catch {report.workerRestorationFailed = true; report.result = 'fail'; process.exitCode = 1;}
  }
  save(); console.log(JSON.stringify({result: report.result, dataDeleted: false, secretsChanged: false}));
}
