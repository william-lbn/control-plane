import assert from 'node:assert/strict';
import { readFile, writeFile, rename, stat } from 'node:fs/promises';
import path from 'node:path';
import { randomBytes } from 'node:crypto';

async function main() {
if (process.platform !== 'linux') throw new Error('Cached Linux Node required');
const [input, output] = process.argv.slice(2);
assert(input && output && path.isAbsolute(input) && path.isAbsolute(output));
const cfg = JSON.parse(await readFile(input, 'utf8'));
for (const [key, prefix] of [['project', 'prj'], ['branch', 'br'], ['function_id', 'fnc'], ['deployment_id', 'fdp']])
  assert(new RegExp('^' + prefix + '_[a-f0-9]{16,32}$').test(cfg[key]));
assert(/^[a-z0-9]{1,20}$/.test(cfg.slug) && ['A', 'B'].includes(cfg.version));
assert(/^[a-zA-Z0-9-]{1,40}$/.test(cfg.stage));
function address(text) {
  const url = new URL(text);
  assert(!url.username && !url.password && !url.search && !url.hash);
  if (url.protocol === 'http:') {
    const ip = url.hostname.split('.').map(Number);
    assert(cfg.lab_http === true && ip.length === 4 && ip.every(n => Number.isInteger(n) && n >= 0 && n <= 255) &&
      (ip[0] === 10 || ip[0] === 127 || ip[0] === 192 && ip[1] === 168 || ip[0] === 172 && ip[1] >= 16 && ip[1] <= 31));
  } else assert.equal(url.protocol, 'https:');
  return url;
}
const target = address(cfg.public_url), consoleOrigin = address(cfg.console_origin);
assert.equal(consoleOrigin.pathname, '/');
assert.equal(target.pathname, '/functions/v1/' + cfg.branch + '/' + cfg.slug);
const headers = {};
for (const [name, value] of Object.entries(cfg.console_headers ?? {})) {
  assert(['cookie', 'authorization'].includes(name.toLowerCase()) && typeof value === 'string' && value.length < 8192 && !/[\r\n]/.test(value));
  headers[name] = value;
}
assert(Object.keys(headers).length > 0);
assert.equal((await stat(path.dirname(output))).mode & 0o077, 0, 'Private output directory required');
const binding = Object.fromEntries(['project', 'branch', 'function_id', 'deployment_id', 'version', 'stage'].map(key => [key, cfg[key]]));
let state;
try { state = JSON.parse(await readFile(output, 'utf8')); assert.deepEqual(state.binding, binding); }
catch (error) {
  if (error.code !== 'ENOENT') throw error;
  state = { result: 'running', binding, checks: [], pending: null };
  await writeFile(output, JSON.stringify(state), { flag: 'wx', mode: 0o600 });
}
async function save() {
  await writeFile(output + '.tmp', JSON.stringify(state, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
  await rename(output + '.tmp', output);
}
const base = '/api/v1/projects/' + cfg.project + '/branches/' + cfg.branch + '/functions/' + cfg.slug;
async function api(suffix = '') {
  const r = await fetch(new URL(base + suffix, consoleOrigin), { headers, redirect: 'error', signal: AbortSignal.timeout(20000) });
  assert.equal(r.status, 200); return r.json();
}
async function call(suffix, options = {}) {
  const r = await fetch(target.href + suffix, { ...options, redirect: 'error', signal: AbortSignal.timeout(30000) });
  assert.equal(r.status, 200);
  assert.equal(r.headers.get('x-function-deployment-id'), cfg.deployment_id);
  const requestID = r.headers.get('x-request-id'); assert(requestID);
  state.receipts ??= []; state.receipts.push({ request_id: requestID, deployment_id: cfg.deployment_id });
  return r;
}
async function step(name, fn, readOnly = false) {
  if (state.checks.includes(name)) return;
  assert(state.pending === null || readOnly && state.pending === name, 'Earlier request has unknown completion; inspect its original checkpoint');
  state.pending = name; await save(); await fn(); state.checks.push(name); state.pending = null; await save();
}
try {
  const definition = await api();
  assert.equal(definition.id, cfg.function_id); assert.equal(definition.active_deployment_id, cfg.deployment_id); assert.equal(definition.state, 'active');
  if (!state.baseline) { const usage = await api('/usage'); assert.equal(usage.function_id, cfg.function_id); state.baseline = usage.edge; await save(); }
  await step('http_version_body', async () => {
    const body = 'edge-functional-' + cfg.version;
    assert.deepEqual(await (await call('/http', { method: 'POST', body })).json(), { version: cfg.version, body });
  });
  await step('sse_first_chunk_before_release', async () => {
    const nonce = randomBytes(16).toString('hex'), response = await call('/sse?nonce=' + nonce);
    assert.match(response.headers.get('content-type'), /^text\/event-stream/);
    const reader = response.body.getReader(), decoder = new TextDecoder(); let text = '';
    try {
      while (!text.includes('\n\n')) {
        const item = await reader.read(); assert(!item.done); text += decoder.decode(item.value, { stream: true }); assert(text.length < 4096);
      }
      assert.deepEqual(JSON.parse(text.split('\n\n')[0].slice(6)), { version: cfg.version, seq: 0 });
      assert(!text.includes('"seq":1'));
      assert.deepEqual(await (await call('/release?nonce=' + nonce, { method: 'POST' })).json(), { version: cfg.version, released: true });
      while (true) { const item = await reader.read(); if (item.done) break; text += decoder.decode(item.value, { stream: true }); assert(text.length < 4096); }
      assert.deepEqual(text.trim().split('\n\n').map(line => JSON.parse(line.slice(6))), [
        { version: cfg.version, seq: 0 }, { version: cfg.version, seq: 1, status: 'released' },
      ]);
    } finally { await reader.cancel().catch(() => {}); }
  });
  await step('websocket_protocol_text_binary_close', async () => {
    const url = new URL(target.href + '/ws'); url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    await new Promise((resolve, reject) => {
      const ws = new WebSocket(url, ['edge-probe.v1']); ws.binaryType = 'arraybuffer'; let phase = 0;
      const timer = setTimeout(() => { ws.close(); reject(new Error('WebSocket deadline')); }, 15000);
      const fail = error => { clearTimeout(timer); ws.close(); reject(error); };
      ws.onerror = () => fail(new Error('WebSocket transport'));
      ws.onmessage = event => {
        try {
          if (phase === 0) { assert.equal(ws.protocol, 'edge-probe.v1'); assert.deepEqual(JSON.parse(event.data), { version: cfg.version }); phase++; ws.send('check'); }
          else if (phase === 1) { assert.equal(event.data, cfg.version + ':check'); phase++; ws.send(new Uint8Array([0, 255, 7])); }
          else if (phase === 2) { assert.deepEqual([...new Uint8Array(event.data)], [cfg.version.charCodeAt(0), 0, 255, 7]); phase++; ws.close(1000, 'done'); }
          else throw new Error('Unexpected frame');
        } catch (error) { fail(error); }
      };
      ws.onclose = event => { clearTimeout(timer); try { assert.equal(phase, 3); assert.equal(event.code, 1000); resolve(); } catch (error) { reject(error); } };
    });
  });
  await step('bounded_resource_fixture', async () => {
    const value = await (await call('/cpu')).json(); assert.equal(value.version, cfg.version); assert.equal(value.iterations, 50000); assert(Number.isSafeInteger(value.checksum));
  });
  await step('trusted_receipts_original_boot_counters', async () => {
    let accepted; const deadline = Date.now() + 60000;
    for (let attempt = 0; attempt < 30 && Date.now() < deadline; attempt++) {
      const usage = await api('/usage'), rows = usage.resource_observations.filter(v => v.deployment_id === cfg.deployment_id && (!cfg.boot_id || v.boot_id === cfg.boot_id));
      if (Number(usage.edge.completion_receipts) >= Number(state.baseline.completion_receipts) + 5 && usage.resource_status === 'observed' && rows.some(v => Number(v.observed_cpu_usec) > 0 && Number(v.observed_memory_current_bytes) > 0 && Number(v.observed_memory_peak_bytes) >= Number(v.observed_memory_current_bytes))) { accepted = { usage, rows }; break; }
      await new Promise(resolve => setTimeout(resolve, 2000));
    }
    assert(accepted, 'Actual observations unavailable');
    assert(Number(accepted.usage.edge.observed_response_bytes) > Number(state.baseline.observed_response_bytes));
    assert(Number(accepted.usage.edge.observed_websocket_request_bytes) > Number(state.baseline.observed_websocket_request_bytes));
    state.observation = { completion_receipts: Number(accepted.usage.edge.completion_receipts), boots: accepted.rows.map(v => v.boot_id), samples: accepted.rows.map(v => Number(v.sample_count)), source: accepted.usage.resource_source };
  }, true);
  state.result = 'pass'; await save();
  console.log(JSON.stringify({ result: 'pass', stage: cfg.stage, version: cfg.version, checks: state.checks, credential_values_exported: false }));
} catch (error) {
  state.result = 'fail'; state.error_type = error.name; await save();
  console.log(JSON.stringify({ result: 'fail', pending: state.pending, error_type: error.name, credential_values_exported: false })); process.exitCode = 1;
}
}

main().catch(() => {
  // Invalid private JSON/URLs can appear in native parser error messages.
  // Never print those messages or their causes.
  console.log(JSON.stringify({ result: 'fail', pending: 'configuration_or_checkpoint', credential_values_exported: false }));
  process.exitCode = 1;
});
