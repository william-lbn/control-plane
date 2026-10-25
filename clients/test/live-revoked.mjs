import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
import { promisify } from 'node:util';
import { execFile } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { Client } from '@modelcontextprotocol/client';
import { StdioClientTransport } from '@modelcontextprotocol/client/stdio';

if (process.platform !== 'linux' || !process.env.NEON_CONTROL_LIVE_FIXTURE) throw new Error('Protected Linux fixture required');
const fixture = JSON.parse(await readFile(process.env.NEON_CONTROL_LIVE_FIXTURE, 'utf8'));
const receipt = JSON.parse(await readFile(fixture.public_result_file, 'utf8'));
assert.equal(receipt.result, 'pass'); assert.equal(receipt.checks.length, 6);
let cli;
try { await promisify(execFile)(process.execPath, [fileURLToPath(new URL('../cli.mjs', import.meta.url)), 'call','getProject','--path',JSON.stringify({ project: fixture.project_id })], { env: { ...process.env, NEON_CONTROL_PROFILE: fixture.profile_file }, timeout: 90000 }); throw new Error('Revoked key unexpectedly accepted'); }
catch (error) { assert.equal(error.code, 2); cli = JSON.parse(error.stdout); assert.equal(cli.status, 401); }
const transport = new StdioClientTransport({ command: process.execPath, args: [fileURLToPath(new URL('../mcp.mjs', import.meta.url))], env: { NEON_CONTROL_PROFILE: fixture.profile_file }, stderr: 'pipe' });
const client = new Client({ name: 'revoked-owned-control-key', version: '1.0.0' }, { versionNegotiation: { mode: 'auto' } });
try {
  await client.connect(transport);
  const denied = await client.callTool({ name: 'api_read', arguments: { operation_id: 'getProject', path: { project: fixture.project_id } } });
  assert.equal(denied.isError, true); assert.equal(denied.structuredContent.status, 401);
} finally { await client.close(); }
receipt.checks.push('CLI_and_MCP_recheck_revoked_key_on_current_server'); receipt.owned_key_revoked = true;
await writeFile(fixture.public_result_file, JSON.stringify(receipt), { mode: 0o600 });
