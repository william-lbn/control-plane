import { test } from 'node:test';
import assert from 'node:assert/strict';
import net from 'node:net';
import tls from 'node:tls';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import pg from 'pg';
import { databaseOptions } from '../src/database.ts';
import type { AuthConfig } from '../src/config.ts';

// Exercise pg itself: its connection implementation overrides ssl.servername
// when the TCP host is DNS. A native tls.connect test cannot catch this bug.
test('pg authenticates the configured certificate identity across Service routing', { timeout: 25000 }, async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'neon-auth-pg-tls-'));
  fs.chmodSync(dir, 0o700);
  const key = path.join(dir, 'key.pem');
  const cert = path.join(dir, 'ca.pem');
  const sockets = new Set<net.Socket>();
  let listener: net.Server | undefined;
  let authenticated = 0;
  try {
    const generated = spawnSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
      '-subj', '/CN=proxy.example.test', '-addext', 'subjectAltName=DNS:proxy.example.test',
      '-keyout', key, '-out', cert], { timeout: 15000, stdio: 'pipe' });
    assert.equal(generated.status, 0, 'OpenSSL is required for real Linux TLS tests');
    const ca = fs.readFileSync(cert);
    const context = tls.createSecureContext({ key: fs.readFileSync(key), cert: ca });
    listener = net.createServer(socket => {
      sockets.add(socket); socket.on('close', () => sockets.delete(socket)); socket.on('error', () => {});
      let request = Buffer.alloc(0);
      const start = (chunk: Buffer) => {
        request = Buffer.concat([request, chunk]);
        if (request.length < 8) return;
        socket.removeListener('data', start);
        assert.equal(request.length, 8); assert.equal(request.readInt32BE(4), 80877103);
        socket.write('S');
        const encrypted = new tls.TLSSocket(socket, { isServer: true, secureContext: context });
        encrypted.on('error', () => {});
        let startup = Buffer.alloc(0);
        const accept = (chunk: Buffer) => {
          startup = Buffer.concat([startup, chunk]);
          if (startup.length < 4 || startup.length < startup.readInt32BE(0)) return;
          assert.equal(startup.readInt32BE(4), 196608);
          encrypted.removeListener('data', accept);
          authenticated++;
          // PostgreSQL AuthenticationOk + ReadyForQuery, then pg's normal
          // Terminate. No SQL, production credentials or endpoint are involved.
          encrypted.write(Buffer.from([82,0,0,0,8,0,0,0,0,90,0,0,0,5,73]));
          encrypted.on('data', data => { assert.equal(data[0], 88); encrypted.end(); });
        };
        encrypted.on('data', accept);
      };
      socket.on('data', start);
    });
    await new Promise<void>((resolve, reject) => { listener!.once('error', reject); listener!.listen(0, '127.0.0.1', resolve); });
    const port = (listener.address() as net.AddressInfo).port;
    const config: AuthConfig = { version: 1, branchID: 'br_0123456789abcdef',
      baseURL: 'https://auth.example.test/auth/v1/br_0123456789abcdef', secret: 'A'.repeat(64),
      trustedOrigins: [], allowLabHTTP: false,
      databaseURL: `postgresql://synthetic:synthetic@localhost:${port}/auth_ci?sslmode=verify-full`,
      databaseTLS: { caFile: '/run/auth-ca/ca.crt', serverName: 'proxy.example.test' } };
    const client = new pg.Client({ ...databaseOptions(config, ca), connectionTimeoutMillis: 5000 });
    try { await client.connect(); } finally { await client.end(); }
    assert.equal(authenticated, 1, 'Actual pg connection completed TLS and PostgreSQL startup');
    for (const [name, trust, expected] of [
      ['wrong.example.test', ca, 'ERR_TLS_CERT_ALTNAME_INVALID'],
      ['proxy.example.test', Buffer.alloc(0), 'DEPTH_ZERO_SELF_SIGNED_CERT'],
    ] as const) {
      const invalid = new pg.Client({ ...databaseOptions({ ...config, databaseTLS: { ...config.databaseTLS, serverName: name } }, trust), connectionTimeoutMillis: 5000 });
      try { await assert.rejects(invalid.connect(), { code: expected }); } finally { await invalid.end(); }
    }
    assert.equal(authenticated, 1, 'Invalid name or CA cannot reach PostgreSQL startup');
  } finally {
    for (const socket of sockets) socket.destroy();
    if (listener) await new Promise<void>(resolve => listener!.close(() => resolve()));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
