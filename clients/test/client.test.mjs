import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import https from 'node:https';
import { once } from 'node:events';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { ControlClient, loadProfile } from '../client.mjs';
import { describe, operationDetail, search, operations } from '../catalog.mjs';

const token = 'synthetic-control-client-token';
async function fixture(t, handler) {
  const directory = await mkdtemp(path.join(tmpdir(), 'neon-control-client-'));
  const server = http.createServer(handler); server.listen(0, '127.0.0.1'); await once(server, 'listening');
  const origin = 'http://127.0.0.1:' + server.address().port;
  const profile = { version: 1, origin, allow_http: true, allow_mutations: true, token };
  const profileFile = path.join(directory, 'profile.json'); await writeFile(profileFile, JSON.stringify(profile), { mode: 0o600 });
  t.after(async () => { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); await rm(directory, { recursive: true, force: true }); });
  return { server, origin, directory, profileFile, profile };
}
function json(response, value, status = 200, headers = {}) { response.writeHead(status, { 'Content-Type': 'application/json', ...headers }); response.end(JSON.stringify(value)); }

test('Catalog covers the exact API once and cursors bind filter and source', () => {
  assert.equal(operations.length, 305);
  const ids = [], seen = new Set(); let cursor;
  do {
    const result = search({ limit: 17, cursor });
    for (const item of result.items) { assert(!seen.has(item.operation_id)); seen.add(item.operation_id); ids.push(item.operation_id); }
    cursor = result.next_cursor;
  } while (cursor);
  assert.equal(ids.length, 305);
  const first = search({ limit: 1 });
  assert.throws(() => search({ query: 'different', cursor: first.next_cursor }), /mismatched/);
  assert.equal(describe('getProject').path, '/api/v1/projects/{project}');
  assert.equal(describe('createProject').read_only, false);
  assert(Object.keys(operationDetail('createProject').referenced_schemas).length > 0);
});

test('Fixed origin, current credentials, parameter encoding and mutation approval are enforced', async t => {
  const calls = [];
  const f = await fixture(t, (request, response) => { calls.push({ url: request.url, auth: request.headers.authorization }); json(response, { id: 'owned', token, password: 'new-one-time-secret', credential_mode: 'sealed' }); });
  const client = new ControlClient(f.profile); t.after(() => client.close());
  const value = await client.execute('getProject', { path: { project: 'name/with spaces' } });
  assert.equal(calls[0].url, '/api/v1/projects/name%2Fwith%20spaces'); assert.equal(calls[0].auth, 'Bearer ' + token);
  assert(!JSON.stringify(value).includes(token)); assert.equal(value.body.password, '[REDACTED-SECRET]'); assert.equal(value.body.credential_mode, 'sealed');
  for (const input of [{ path: { project: '..' } }, { path: { project: 'ok' }, headers: { Authorization: 'evil' } }, { path: { project: 'ok' }, origin: 'http://elsewhere.invalid' }, { path: { project: 'ok' }, query: { arbitrary: 'x' } }])
    await assert.rejects(client.execute('getProject', input));
  await assert.rejects(client.execute('createProject', { path: { org: 'owned' }, body: {} }), /approval|approved|explicit/);
  const readonly = new ControlClient({ ...f.profile, allow_mutations: false }); t.after(() => readonly.close());
  await assert.rejects(readonly.execute('createProject', { path: { org: 'owned' }, body: {} }, { allowWrite: true }), /approval|approved|explicit/);
  const scoped = new ControlClient({ ...f.profile, allowed_operation_ids: ['listSavedQueries'] }); t.after(() => scoped.close());
  await assert.rejects(scoped.execute('getProject', { path: { project: 'ok' } }), /allowlist/);
  assert.equal(calls.length, 1);
  await writeFile(f.profileFile, JSON.stringify({ ...f.profile, origin: 'http://public.example' }));
  assert.throws(() => loadProfile({ NEON_CONTROL_PROFILE: f.profileFile }), /HTTP requires/);
});

test('A lost mutation reply has one attempt and an explicit same-key replay keeps one resource', async t => {
  let calls = 0, mutations = 0; const receipts = new Map();
  const f = await fixture(t, async (request, response) => {
    const body = []; for await (const bytes of request) body.push(bytes);
    calls++; assert.equal(request.headers.authorization, 'Bearer ' + token);
    const key = request.headers['idempotency-key']; assert.equal(key, 'same-owned-key');
    if (receipts.has(key)) return json(response, receipts.get(key), 200, { 'Idempotency-Replayed': 'true' });
    assert.equal(JSON.parse(Buffer.concat(body).toString()).name, 'Owned');
    const result = { id: 'saved_owned_' + ++mutations }; receipts.set(key, result); request.socket.destroy();
  });
  const client = new ControlClient(f.profile); t.after(() => client.close());
  const input = { path: { project: 'owned' }, headers: { 'Idempotency-Key': 'same-owned-key' }, body: { name: 'Owned', sql: 'SELECT 1' } };
  await assert.rejects(client.execute('createSavedQuery', input, { allowWrite: true }), /no automatic retry/);
  assert.equal(calls, 1); assert.equal(mutations, 1);
  const replay = await client.execute('createSavedQuery', input, { allowWrite: true });
  assert.equal(replay.status, 200); assert.equal(replay.idempotency_replayed, true); assert.equal(replay.body.id, 'saved_owned_1'); assert.equal(calls, 2); assert.equal(mutations, 1);
});

test('Redirects never forward credentials and cancellation does not resend', async t => {
  let forwarded = 0;
  const other = await fixture(t, (_request, response) => { forwarded++; json(response, {}); });
  let observed; const seen = new Promise(resolve => { observed = resolve; });
  const f = await fixture(t, (request, response) => {
    if (request.url.endsWith('/cancel')) { observed(); return; }
    response.writeHead(302, { Location: other.origin + '/capture' }); response.end();
  });
  const client = new ControlClient(f.profile); t.after(() => client.close());
  await assert.rejects(client.execute('getProject', { path: { project: 'redirect' } }), /Redirect rejected/);
  assert.equal(forwarded, 0);
  const controller = new AbortController();
  const pending = client.execute('getProject', { path: { project: 'cancel' } }, { signal: controller.signal });
  await seen; controller.abort(); await assert.rejects(pending, /transport failed/);
});

test('Private output streams bytes completely and refuses existing destinations', async t => {
  const bytes = Buffer.alloc(2 * 1024 * 1024, 65);
  const f = await fixture(t, (_request, response) => { response.writeHead(200, { 'Content-Type': 'application/octet-stream' }); response.end(bytes); });
  const client = new ControlClient(f.profile); t.after(() => client.close());
  await assert.rejects(client.execute('getProject', { path: { project: 'large' } }), /1 MiB/);
  const target = path.join(f.directory, 'download.bin');
  const value = await client.execute('getProject', { path: { project: 'large' } }, { outputFile: target });
  assert.equal(value.bytes, bytes.length); assert.deepEqual(await readFile(target), bytes);
  await assert.rejects(client.execute('getProject', { path: { project: 'large' } }, { outputFile: target }), /not published/);
  assert.deepEqual(await readFile(target), bytes);
});

test('LAN always bypasses a proxy; WAN uses only its configured proxy', async t => {
  let proxied = 0;
  const proxy = http.createServer((_request, response) => { proxied++; response.writeHead(502); response.end(); });
  proxy.on('connect', (request, socket) => { proxied++; assert(!request.headers.authorization); socket.end('HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n'); });
  proxy.listen(0, '127.0.0.1'); await once(proxy, 'listening');
  t.after(() => new Promise(resolve => proxy.close(resolve)));
  const url = 'http://127.0.0.1:' + proxy.address().port;
  const f = await fixture(t, (_request, response) => json(response, { direct: true }));
  const client = new ControlClient(f.profile, { HTTP_PROXY: url, HTTPS_PROXY: url }); t.after(() => client.close());
  assert.equal((await client.execute('getProject', { path: { project: 'direct' } })).body.direct, true); assert.equal(proxied, 0);
  const external = new ControlClient({ ...f.profile, origin: 'https://controller.invalid' }, { HTTPS_PROXY: url }); t.after(() => external.close());
  await assert.rejects(external.execute('getProject', { path: { project: 'external' } })); assert.equal(proxied, 1);
});

test('HTTPS verifies the server before sending credentials and accepts only the configured CA', async t => {
  const directory = await mkdtemp(path.join(tmpdir(), 'neon-client-tls-')); t.after(() => rm(directory, { recursive: true, force: true }));
  const key = path.join(directory, 'key.pem'), cert = path.join(directory, 'cert.pem');
  execFileSync('openssl', ['req','-x509','-newkey','rsa:2048','-nodes','-keyout',key,'-out',cert,'-subj','/CN=localhost','-addext','subjectAltName=IP:127.0.0.1','-days','1'], { stdio: 'ignore' });
  let requests = 0;
  const server = https.createServer({ key: await readFile(key), cert: await readFile(cert) }, (_request, response) => { requests++; json(response, { trusted: true }); });
  server.listen(0, '127.0.0.1'); await once(server, 'listening'); t.after(() => new Promise(resolve => server.close(resolve)));
  const profile = { origin: 'https://127.0.0.1:' + server.address().port, token, allow_mutations: false };
  const untrusted = new ControlClient(profile); t.after(() => untrusted.close());
  await assert.rejects(untrusted.execute('getProject', { path: { project: 'test' } })); assert.equal(requests, 0);
  const trusted = new ControlClient({ ...profile, ca_file: cert }); t.after(() => trusted.close());
  assert.equal((await trusted.execute('getProject', { path: { project: 'test' } })).body.trusted, true); assert.equal(requests, 1);
});

test('Binary tool replies cannot expose configured credentials through base64', async t => {
  const f = await fixture(t, (_request, response) => { response.writeHead(200, { 'Content-Type': 'application/octet-stream' }); response.end(Buffer.from(token)); });
  const client = new ControlClient(f.profile); t.after(() => client.close());
  const value = await client.execute('getProject', { path: { project: 'echo' } });
  assert.equal(value.body.encoding, 'redacted');
  assert(!JSON.stringify(value).includes(token)); assert(!JSON.stringify(value).includes(Buffer.from(token).toString('base64')));
});

test('Console session profiles preserve same-origin CSRF and reject credential header overrides', async t => {
  let calls = 0;
  const cookie = 'neon_v2_session=synthetic-session-cookie; neon_v2_csrf=synthetic-console-csrf';
  const f = await fixture(t, async (request, response) => {
    calls++; assert.equal(request.headers.cookie, cookie); assert.equal(request.headers['x-csrf-token'], 'synthetic-console-csrf');
    assert.equal(request.headers.origin, f.origin); assert.equal(request.headers.authorization, undefined);
    for await (const _bytes of request) { /* complete the accepted request */ }
    json(response, { id: 'owned', csrf_token: 'synthetic-console-csrf' }, 201);
  });
  const profile = { ...f.profile, token: undefined, session_cookie: cookie, csrf_token: 'synthetic-console-csrf' };
  await writeFile(f.profileFile, JSON.stringify(profile), { mode: 0o600 });
  const client = new ControlClient(loadProfile({ NEON_CONTROL_PROFILE: f.profileFile })); t.after(() => client.close());
  const input = { path: { project: 'owned' }, headers: { 'Idempotency-Key': 'owned-session-write' }, body: { name: 'Owned', sql: 'SELECT 1' } };
  const value = await client.execute('createSavedQuery', input, { allowWrite: true }); assert.equal(value.status, 201); assert.equal(value.body.csrf_token, '[REDACTED-SECRET]');
  await assert.rejects(client.execute('createSavedQuery', { ...input, headers: { ...input.headers, 'X-CSRF-Token': 'override' } }, { allowWrite: true }), /allowed operation parameter/);
  assert.equal(calls, 1);
});
