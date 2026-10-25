import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { once } from 'node:events';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import path from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import { Client } from '@modelcontextprotocol/client';
import { StdioClientTransport } from '@modelcontextprotocol/client/stdio';

const entry = fileURLToPath(new URL('../mcp.mjs', import.meta.url));
const cli = fileURLToPath(new URL('../cli.mjs', import.meta.url));

for (const modern of [false, true]) test('Official SDK ' + (modern ? '2026 stateless discovery' : '2025 initialize') + ' enforces separate read/write tools and private credentials', async t => {
  const directory = await mkdtemp(path.join(tmpdir(), 'neon-mcp-')); t.after(() => rm(directory, { recursive: true, force: true }));
  const token = 'synthetic-sdk-profile-credential'; let writes = 0;
  const api = http.createServer(async (request, response) => {
    assert.equal(request.headers.authorization, 'Bearer ' + token);
    if (request.method === 'POST') { writes++; for await (const _chunk of request) { /* consume bounded input */ } }
    response.writeHead(200, { 'Content-Type': 'application/json' }); response.end(JSON.stringify({ id: 'owned', token, items: [] }));
  });
  api.listen(0, '127.0.0.1'); await once(api, 'listening'); t.after(() => { api.closeAllConnections(); return new Promise(resolve => api.close(resolve)); });
  const file = path.join(directory, 'profile.json');
  await writeFile(file, JSON.stringify({ version: 1, origin: 'http://127.0.0.1:' + api.address().port, allow_http: true, allow_mutations: true, token }), { mode: 0o600 });
  const transport = new StdioClientTransport({ command: process.execPath, args: [entry], env: { NEON_CONTROL_PROFILE: file }, stderr: 'pipe' });
  const client = new Client({ name: 'control-client-functional-test', version: '1.0.0' }, modern ? { versionNegotiation: { mode: 'auto' } } : {});
  t.after(() => client.close());
  let diagnostics = ''; transport.stderr?.on('data', chunk => { diagnostics += chunk; });
  await client.connect(transport); assert.equal(client.getProtocolEra(), modern ? 'modern' : 'legacy');
  const tools = await client.listTools(); assert.deepEqual(tools.tools.map(v => v.name).sort(), ['api_describe','api_mutate','api_read','api_search']);
  assert.equal(tools.tools.find(v => v.name === 'api_read').annotations.readOnlyHint, true);
  assert.equal(tools.tools.find(v => v.name === 'api_mutate').annotations.destructiveHint, true);
  const catalog = await client.callTool({ name: 'api_search', arguments: { query: 'getProject' } }); assert(catalog.structuredContent.items.some(v => v.operation_id === 'getProject'));
  const denied = await client.callTool({ name: 'api_read', arguments: { operation_id: 'createSavedQuery', path: { project: 'owned' }, body: {} } }); assert.equal(denied.isError, true); assert.equal(writes, 0);
  const value = await client.callTool({ name: 'api_read', arguments: { operation_id: 'getProject', path: { project: 'owned' } } });
  assert.equal(value.isError, undefined); assert.equal(value.structuredContent.body.id, 'owned'); assert(!JSON.stringify(value).includes(token));
  const written = await client.callTool({ name: 'api_mutate', arguments: { operation_id: 'createSavedQuery', path: { project: 'owned' }, headers: { 'Idempotency-Key': 'owned-sdk-write' }, body: { name: 'Owned', sql: 'SELECT 1' } } });
  assert.equal(written.structuredContent.status, 200); assert.equal(writes, 1); assert(!diagnostics.includes(token));
  await client.close();
});

test('CLI searches without credentials, emits JSON and rejects unapproved writes', async () => {
  async function run(args, env = {}) {
    const child = spawn(process.execPath, [cli, ...args], { env: { PATH: process.env.PATH, ...env }, stdio: ['ignore','pipe','pipe'] });
    let stdout = '', stderr = ''; child.stdout.on('data', value => { stdout += value; }); child.stderr.on('data', value => { stderr += value; });
    const [code] = await once(child, 'close'); return { code, stdout, stderr };
  }
  const found = await run(['search','--query','service-plan']); assert.equal(found.code, 0); assert(JSON.parse(found.stdout).total > 0);
  const described = await run(['describe','getProject']); assert.equal(described.code, 0); assert.equal(JSON.parse(described.stdout).operation.method, 'GET');
  const unknown = await run(['call','getProject','--origin','https://override.invalid']); assert.equal(unknown.code, 1); assert.match(unknown.stderr, /not supported/);
  const missing = await run(['call','getProject']); assert.equal(missing.code, 1); assert.match(missing.stderr, /NEON_CONTROL_PROFILE/);
});

test('Read-only operator configuration never advertises a mutation tool', async t => {
  const directory = await mkdtemp(path.join(tmpdir(), 'neon-mcp-readonly-')); t.after(() => rm(directory, { recursive: true, force: true }));
  const file = path.join(directory, 'profile.json'); await writeFile(file, JSON.stringify({ version: 1, origin: 'http://127.0.0.1:1', allow_http: true, token: 'synthetic-readonly-token' }), { mode: 0o600 });
  const transport = new StdioClientTransport({ command: process.execPath, args: [entry], env: { NEON_CONTROL_PROFILE: file }, stderr: 'pipe' });
  const client = new Client({ name: 'readonly-test', version: '1.0.0' }, { versionNegotiation: { mode: 'auto' } }); t.after(() => client.close());
  await client.connect(transport); const listed = await client.listTools(); assert(!listed.tools.some(v => v.name === 'api_mutate'));
  await client.close();
});

for (const modern of [false, true]) test('Official MCP cancellation ' + (modern ? '2026' : '2025') + ' aborts the original API read without a second request', async t => {
  const directory = await mkdtemp(path.join(tmpdir(), 'neon-mcp-cancel-')); t.after(() => rm(directory, { recursive: true, force: true }));
  let accepted, closed, calls = 0;
  const seen = new Promise(resolve => { accepted = resolve; });
  const ended = new Promise(resolve => { closed = resolve; });
  const api = http.createServer((request, response) => { calls++; accepted(); response.on('close', closed); });
  api.listen(0, '127.0.0.1'); await once(api, 'listening'); t.after(() => { api.closeAllConnections(); return new Promise(resolve => api.close(resolve)); });
  const file = path.join(directory, 'profile.json'); await writeFile(file, JSON.stringify({ version: 1, origin: 'http://127.0.0.1:' + api.address().port, allow_http: true, token: 'synthetic-cancel-token' }), { mode: 0o600 });
  const transport = new StdioClientTransport({ command: process.execPath, args: [entry], env: { NEON_CONTROL_PROFILE: file }, stderr: 'pipe' });
  const client = new Client({ name: 'cancel-test', version: '1.0.0' }, modern ? { versionNegotiation: { mode: 'auto' } } : {}); t.after(() => client.close());
  await client.connect(transport);
  const controller = new AbortController();
  const pending = client.callTool({ name: 'api_read', arguments: { operation_id: 'getProject', path: { project: 'owned' } } }, { signal: controller.signal });
  await seen; controller.abort(); await assert.rejects(pending); await ended; assert.equal(calls, 1);
  await client.close();
});
