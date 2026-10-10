import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import test from 'node:test';
import { startRuntime } from '../runtime/server.mjs';
import { waitUntil } from '../runtime/context.mjs';

async function fixture(t, source, options = {}) {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'neon-function-'));
  const entry = path.join(directory, 'index.mjs');
  await fs.writeFile(entry, source);
  const runtime = await startRuntime({ entry, port: 0, ...options });
  const origin = `http://127.0.0.1:${runtime.server.address().port}`;
  t.after(async () => { await runtime.close(); await fs.rm(directory, { recursive: true }); });
  const invoke = (suffix = '', options = {}) => fetch(origin + suffix, {
    ...options, headers: { 'x-neon-request-url': 'https://functions.example.test/hello' + suffix, ...options.headers },
  });
  return { runtime, origin, invoke };
}

test('real HTTP executes object and bare async fetch handlers', async t => {
  for (const source of [
    `export default { fetch: req => new Response(JSON.stringify({path:new URL(req.url).pathname, header:req.headers.get('x-test')}), {headers:{'content-type':'application/json'}}) };`,
    `export default async req => new Response(JSON.stringify({path:new URL(req.url).pathname, header:req.headers.get('x-test')}), {headers:{'content-type':'application/json'}});`,
  ]) {
    const { invoke } = await fixture(t, source);
    const response = await invoke('/child', { headers: { 'x-test': 'forwarded' } });
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { path: '/hello/child', header: 'forwarded' });
  }
});

test('real POST preserves body and response cookies without internal headers', async t => {
  const { invoke } = await fixture(t, `export default async req => new Response(await req.text()+':'+req.headers.has('x-neon-request-url'), {status:201,headers:{'set-cookie':'session=test; HttpOnly','x-app':'yes'}});`);
  const response = await invoke('', { method: 'POST', body: 'payload' });
  assert.equal(response.status, 201);
  assert.equal(response.headers.get('set-cookie'), 'session=test; HttpOnly');
  assert.equal(await response.text(), 'payload:false');
});

test('SSE delivers first event before delayed second event', async t => {
  const { invoke } = await fixture(t, `export default () => new Response(new ReadableStream({start(c){c.enqueue(new TextEncoder().encode('data: first\\n\\n'));setTimeout(()=>{c.enqueue(new TextEncoder().encode('data: second\\n\\n'));c.close();},120)}}),{headers:{'content-type':'text/event-stream'}});`);
  const response = await invoke();
  const reader = response.body.getReader();
  const first = await reader.read();
  assert.equal(new TextDecoder().decode(first.value), 'data: first\n\n');
  const second = await reader.read();
  assert.equal(new TextDecoder().decode(second.value), 'data: second\n\n');
  assert.equal((await reader.read()).done, true);
});

test('waitUntil retains pending accounting after HTTP body completes', async t => {
  const sdk = pathToFileURL(path.resolve('runtime/context.mjs')).href;
  const { invoke, runtime } = await fixture(t, `import {waitUntil} from '${sdk}';export default () => {waitUntil(new Promise(r=>setTimeout(r,150)));return new Response('finished');};`);
  assert.equal(await (await invoke()).text(), 'finished');
  assert.equal(runtime.status().pending, 1);
  await new Promise(resolve => setTimeout(resolve, 200));
  assert.equal(runtime.status().pending, 0);
  assert.equal(runtime.status().active, 0);
});

test('background rejection is observed without exposing exception contents', async t => {
  const sdk = pathToFileURL(path.resolve('runtime/context.mjs')).href;
  const { invoke, runtime } = await fixture(t, `import {waitUntil} from '${sdk}';export default () => {waitUntil(Promise.reject(new Error('private-secret-value')));return new Response('finished');};`);
  assert.equal(await (await invoke()).text(), 'finished');
  await new Promise(resolve => setTimeout(resolve, 20));
  assert.equal(runtime.status().failures, 1);
  assert.equal(JSON.stringify(runtime.status()).includes('private-secret-value'), false);
});

test('invalid handler result returns bounded generic error', async t => {
  const { invoke, runtime } = await fixture(t, `export default () => ({secret:'private-value'});`);
  const response = await invoke();
  assert.equal(response.status, 502);
  assert.equal(await response.text(), 'Function execution failed');
  assert.equal(runtime.status().active, 0);
});

test('deadlines cancel a handler that never resolves and release accounting', async t => {
  const { invoke, runtime } = await fixture(t, `export default () => new Promise(()=>{});`, { deadlineMs: 50 });
  await assert.rejects(invoke());
  await new Promise(resolve => setTimeout(resolve, 20));
  assert.equal(runtime.status().active, 0);
});

test('reject missing invocation origin and oversized POST body', async t => {
  const { origin, invoke } = await fixture(t, `export default () => new Response('ok');`);
  assert.equal((await fetch(origin)).status, 502);
  assert.equal((await invoke('', { method: 'POST', body: 'a'.repeat(1024 * 1024 + 1) })).status, 413);
});

test('waitUntil outside request context is rejected', () => {
  assert.throws(() => waitUntil(Promise.resolve()), /active invocation/);
});
