import test from 'node:test';
import assert from 'node:assert/strict';
import { createProbe } from './handler.mjs';

const request = (suffix, options) => new Request('http://probe.test/functions/v1/br_0000000000000000/canary/' + suffix, options);
test('Probe versions are distinct and bounded without SQL or package input', async () => {
  assert.throws(() => createProbe('unreviewed', () => {}));
  for (const version of ['A', 'B']) {
    const run = createProbe(version, () => { throw new Error('Unexpected upgrade'); });
    assert.deepEqual(await (await run(request('http', { method: 'POST', body: 'small' }))).json(), { version, body: 'small' });
    assert.equal((await run(request('http', { method: 'POST', body: 'x'.repeat(1025) }))).status, 413);
    const value = await (await run(request('cpu'))).json();
    assert.equal(value.version, version); assert.equal(value.iterations, 50000); assert(Number.isSafeInteger(value.checksum));
  }
});
test('SSE requires explicit release and cancellation removes the original nonce', async () => {
  const run = createProbe('A', () => {}), nonce = 'a'.repeat(32), decoder = new TextDecoder();
  const response = await run(request('sse?nonce=' + nonce)), reader = response.body.getReader();
  assert.match(response.headers.get('content-type'), /^text\/event-stream/);
  assert.equal(decoder.decode((await reader.read()).value), 'data: {"version":"A","seq":0}\n\n');
  assert.equal((await run(request('sse?nonce=' + nonce))).status, 409);
  assert.deepEqual(await (await run(request('release?nonce=' + nonce, { method: 'POST' }))).json(), { version: 'A', released: true });
  assert.equal(decoder.decode((await reader.read()).value), 'data: {"version":"A","seq":1,"status":"released"}\n\n');
  assert((await reader.read()).done);
  const second = await run(request('sse?nonce=' + nonce)); await second.body.cancel();
  assert.equal((await run(request('release?nonce=' + nonce, { method: 'POST' }))).status, 404);
  assert.equal((await run(request('sse?nonce=invalid'))).status, 422);
});
test('Fixture WebSocket has deterministic version text binary and protocol', async () => {
  for (const version of ['A', 'B']) {
    const messages = [], socket = { send: value => messages.push(value) }, response = new Response('mock handshake');
    const run = createProbe(version, (r, options) => { assert.equal(new URL(r.url).pathname.split('/').at(-1), 'ws'); assert.equal(options.protocol, 'edge-probe.v1'); return { socket, response }; });
    assert.equal(await run(request('ws')), response);
    socket.onopen(); socket.onmessage({ data: 'check' }); socket.onmessage({ data: new Uint8Array([0, 255, 7]).buffer });
    assert.deepEqual(JSON.parse(messages[0]), { version }); assert.equal(messages[1], version + ':check');
    assert.deepEqual([...messages[2]], [version.charCodeAt(0), 0, 255, 7]);
  }
});
