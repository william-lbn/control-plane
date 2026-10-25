// Explicit Linux integration entry; ordinary npm test never creates live data.
import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
import { promisify } from 'node:util';
import { execFile } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { Client } from '@modelcontextprotocol/client';
import { StdioClientTransport } from '@modelcontextprotocol/client/stdio';

if (process.platform !== 'linux' || !process.env.NEON_CONTROL_LIVE_FIXTURE) throw new Error('Explicit protected Linux live fixture required');
const fixture = JSON.parse(await readFile(process.env.NEON_CONTROL_LIVE_FIXTURE, 'utf8'));
assert.equal(fixture.project_id, 'prj_6f3c51bd15b772e3'); assert.equal(fixture.org_id, 'local'); assert(fixture.owned_api_key === true);
const profile = JSON.parse(await readFile(fixture.profile_file, 'utf8')); assert.equal(profile.origin, 'http://192.168.146.100:30788');
const state = fixture.resume_file ? JSON.parse(await readFile(fixture.resume_file, 'utf8')) : { checks: [], keys: {}, nonce: fixture.nonce };
if (state.running) throw new Error('Inspect original mutation before continuing the private checkpoint');
let result = 'running';
const run = promisify(execFile), cliEntry = fileURLToPath(new URL('../cli.mjs', import.meta.url));
async function save() {
  await writeFile(fixture.checkpoint_file, JSON.stringify(state), { mode: 0o600 });
  await writeFile(fixture.public_result_file, JSON.stringify({ result, checks: state.checks, project_id: fixture.project_id, api_key_id: fixture.api_key_id,
    saved_query_id: state.query_id || null, compute_created: false, SQL_executed: false, commercial_calls: false, credentials_exported: false,
    performance_reliability_campaign: false }), { mode: 0o600 });
}
async function step(name, work) {
  if (state.checks.includes(name)) return;
  state.running = name; await save(); await work(); state.checks.push(name); state.running = ''; await save();
}
async function cli(operation, input = {}, expected = 200) {
  const args = ['call', operation];
  for (const [key,value] of Object.entries(input)) {
    if (key === 'write') { if (value) args.push('--write'); }
    else args.push('--' + key, JSON.stringify(value));
  }
  let output;
  try { output = await run(process.execPath, [cliEntry, ...args], { env: { ...process.env, NEON_CONTROL_PROFILE: fixture.profile_file }, timeout: 90000, maxBuffer: 2 * 1024 * 1024 }); }
  catch (error) { if (error.code !== 2) throw error; output = error; }
  assert(!output.stdout.includes(profile.token)); assert(!output.stderr.includes(profile.token));
  const value = JSON.parse(output.stdout); assert.equal(value.status, expected); return value;
}
const transport = new StdioClientTransport({ command: process.execPath, args: [fileURLToPath(new URL('../mcp.mjs', import.meta.url))], env: { NEON_CONTROL_PROFILE: fixture.profile_file }, stderr: 'pipe' });
const mcp = new Client({ name: 'owned-live-control-acceptance', version: '1.0.0' }, { versionNegotiation: { mode: 'auto' } });
async function tool(name, operation, args = {}, status = 200) {
  const value = await mcp.callTool({ name, arguments: { operation_id: operation, ...args } });
  assert(!JSON.stringify(value).includes(profile.token)); assert.equal(value.structuredContent.status, status); return value.structuredContent;
}
const path = () => ({ project: fixture.project_id, query: state.query_id });
const body = { name: 'ci-control-client-' + fixture.nonce, sql: "SELECT 'owned-client-" + fixture.nonce + "'" };
try {
  await mcp.connect(transport); assert.equal(mcp.getProtocolEra(), 'modern');
  await step('CLI_and_MCP_read_the_exact_owned_scope', async () => {
    const original = await cli('getProject', { path: { project: fixture.project_id } }); assert.equal(original.body.id, fixture.project_id);
    const read = await tool('api_read','listSavedQueries',{ path: { project: fixture.project_id } }); assert(Array.isArray(read.body.items));
    const tools = await mcp.listTools(); assert(tools.tools.some(value => value.name === 'api_mutate'));
  });
  await step('CLI_create_and_explicit_same_key_receipt_replay', async () => {
    state.keys.create ||= 'client-' + fixture.nonce + '-create'; await save();
    const input = { path: { project: fixture.project_id }, headers: { 'Idempotency-Key': state.keys.create }, body, write: true };
    if (!state.query_id) {
      const created = await cli('createSavedQuery', input, 201); state.query_id = created.body.query.id; state.generation = created.body.query.generation; await save();
      assert.match(state.query_id, /^sq_[a-f0-9]{24}$/); assert.equal(created.body.sql_included, false);
    }
    const replay = await cli('createSavedQuery', input, 200); assert.equal(replay.body.query.id, state.query_id); assert.equal(replay.body.query.generation, 1);
  });
  await step('MCP_reads_original_SQL_and_performs_versioned_update', async () => {
    const before = await tool('api_read','getSavedQuery',{ path: path() }); assert.equal(before.body.sql, body.sql); assert.equal(before.etag, '"1"');
    state.keys.update ||= 'client-' + fixture.nonce + '-update'; await save();
    const changed = await tool('api_mutate','updateSavedQuery',{ path: path(), headers: { 'Idempotency-Key': state.keys.update, 'If-Match': before.etag }, body: { ...body, sql: body.sql + ' AS marker' } });
    assert.equal(changed.body.query.generation, 2); state.generation = 2; await save();
    const read = await cli('getSavedQuery',{ path: path() }); assert.equal(read.body.sql, body.sql + ' AS marker'); assert.equal(read.etag, '"2"');
  });
  await step('stale_version_and_changed_same_key_rejected_without_partial_mutation', async () => {
    state.keys.stale ||= 'client-' + fixture.nonce + '-stale'; await save();
    await tool('api_mutate','updateSavedQuery',{ path: path(), headers: { 'Idempotency-Key': state.keys.stale, 'If-Match': '"1"' }, body },412);
    await cli('createSavedQuery',{ path: { project: fixture.project_id }, headers: { 'Idempotency-Key': state.keys.create }, body: { ...body, name: body.name + ' changed' }, write: true },409);
    const value = await tool('api_read','getSavedQuery',{ path: path() }); assert.equal(value.body.generation, 2); assert.equal(value.body.sql, body.sql + ' AS marker');
  });
  await step('current_server_project_scope_rejects_another_owned_project', async () => {
    const value = await mcp.callTool({ name: 'api_read', arguments: { operation_id: 'getProject', path: { project: 'prj_6370b0aa78f4ee97' } } });
    assert.equal(value.isError, true); assert([403,404].includes(value.structuredContent.status));
  });
  await step('CLI_deletes_only_its_query_and_MCP_observes_absence', async () => {
    state.keys.delete ||= 'client-' + fixture.nonce + '-delete'; await save();
    const deleted = await cli('deleteSavedQuery',{ path: path(), headers: { 'Idempotency-Key': state.keys.delete, 'If-Match': '"2"' }, write: true }); assert.equal(deleted.body.query.id, state.query_id);
    await tool('api_read','getSavedQuery',{ path: path() },404);
    const list = await cli('listSavedQueries',{ path: { project: fixture.project_id } }); assert(!list.body.items.some(value => value.id === state.query_id));
  });
  result = 'pass';
} catch (error) { result = 'fail'; throw error; }
finally { await save(); await mcp.close(); }
