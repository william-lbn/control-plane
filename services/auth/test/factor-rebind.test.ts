import { test } from 'node:test';
import assert from 'node:assert/strict';
import { randomBytes, createHmac } from 'node:crypto';
import { readFileSync } from 'node:fs';
import pg from 'pg';
import { symmetricEncrypt, symmetricDecrypt } from 'better-auth/crypto';
import { createAuth } from '../src/auth.ts';
import type { AuthConfig } from '../src/config.ts';

function code(secret: string) {
  // Better Auth stores the raw generated string. Its enrollment URI encodes
  // these bytes as Base32; the decrypted database value is not a Base32 URI.
  const message = Buffer.alloc(8);
  message.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const digest = createHmac('sha1', Buffer.from(secret, 'utf8')).update(message).digest();
  return String((digest.readUInt32BE(digest[19] & 15) & 0x7fffffff) % 1000000).padStart(6, '0');
}

test('Go rewrapped factor interoperates with pinned Better Auth and preserves fresh-login MFA and recovery boundaries', async () => {
  const dsn = process.env.AUTH_TEST_DATABASE_URL;
  assert(dsn && new URL(dsn).pathname === '/auth_ci', 'Disposable auth_ci required');
  const fixture = JSON.parse(readFileSync(new URL('./fixtures/go-factor-rebind.json', import.meta.url), 'utf8'));
  assert.equal(fixture.synthetic_only, true);
  assert.equal(fixture.producer, 'Go managed Auth factor rebind functional test');
  const parentKey = 'synthetic-parent-auth-key-never-a-customer-secret';
  const childKey = fixture.vector.secret as string;
  const plaintext = fixture.vector.plaintext as string;
  const rewrapped = fixture.vector.ciphertext as string;
  assert.equal(await symmetricDecrypt({ key: { keys: new Map([[0, childKey]]), currentVersion: 0, legacySecret: childKey }, data: rewrapped }), plaintext);
  await assert.rejects(symmetricDecrypt({ key: parentKey, data: rewrapped }));
  const suffix = randomBytes(6).toString('hex');
  const schemas = ['factor_parent_' + suffix, 'factor_child_' + suffix];
  const admin = new pg.Pool({ connectionString: dsn, max: 1 });
  const pools: pg.Pool[] = [];
  const password = 'Synthetic factor migration password 2026';
  const email = 'factor-clone@example.test';
  type Client = { auth: ReturnType<typeof createAuth>; config: AuthConfig; cookies: Map<string,string>; sequence: number };
  const clients: Client[] = [];
  async function request(client: Client, action: string, body?: unknown) {
    if (action === 'sign-in/email') client.sequence++;
    const response = await client.auth.handler(new Request(client.config.baseURL + '/' + action, {
      method: body === undefined ? 'GET' : 'POST',
      headers: { Origin: 'https://auth.example.test', 'x-neon-client-ip': '192.0.2.' + client.sequence,
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
        ...(client.cookies.size ? { Cookie: [...client.cookies].map(([k,v]) => k + '=' + v).join('; ') } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body),
    }));
    for (const item of response.headers.getSetCookie()) {
      const first = item.split(';')[0], at = first.indexOf('='), name = first.slice(0, at), value = first.slice(at + 1);
      if (!value || /max-age=0/i.test(item)) client.cookies.delete(name); else client.cookies.set(name, value);
    }
    return { response, value: await response.json() };
  }
  try {
    for (let i = 0; i < 2; i++) {
      await admin.query('CREATE SCHEMA ' + schemas[i]);
      const db = new pg.Pool({ connectionString: dsn, max: 2, options: '-c search_path=' + schemas[i] + ',pg_catalog' });
      pools.push(db);
      await db.query(readFileSync(new URL('../../../api/internal/control/assets/auth-schema.sql', import.meta.url), 'utf8'));
      await db.query(readFileSync(new URL('../../../api/internal/control/assets/auth-totp-schema.sql', import.meta.url), 'utf8').replaceAll('neon_auth.', schemas[i] + '.'));
      const branchID = i === 0 ? 'br_0123456789abcdef' : 'br_fedcba9876543210';
      const config: AuthConfig = { version: 1, branchID, baseURL: 'https://auth.example.test/auth/v1/' + branchID, databaseURL: dsn,
        databaseTLS: { caFile: '/run/auth-ca/ca.crt', serverName: 'test' }, secret: i === 0 ? parentKey : childKey,
        trustedOrigins: ['https://auth.example.test'], allowLabHTTP: false, twoFactorEnabled: true };
      clients.push({ auth: createAuth(config, db), config, cookies: new Map(), sequence: 1 + i * 100 });
    }
    const parent = clients[0], child = clients[1];
    let result = await request(parent, 'sign-up/email', { email, password, name: 'Synthetic branch factor user' });
    assert.equal(result.response.status, 200);
    const userID = result.value.user.id;
    result = await request(parent, 'two-factor/enable', { password, method: 'totp' });
    assert.equal(result.response.status, 200);
    const recovery = result.value.backupCodes as string[];
    assert(recovery.length >= 2);
    // The parent is synthetic. Seed the same TOTP plaintext produced by the Go
    // test; the child receives the exact Go ciphertext, not a JS round trip.
    const parentCipher = await symmetricEncrypt({ key: parentKey, data: plaintext });
    await pools[0].query('UPDATE "twoFactor" SET secret=$1 WHERE "userId"=$2', [parentCipher, userID]);
    result = await request(parent, 'two-factor/verify-totp', { code: code(plaintext), trustDevice: false });
    assert.equal(result.response.status, 200);
    const parentCookies = new Map(parent.cookies);
    const parentBefore = (await pools[0].query('SELECT to_jsonb(f) AS row FROM "twoFactor" f')).rows;
    for (const table of ['user', 'account']) await admin.query(`INSERT INTO ${schemas[1]}."${table}" SELECT * FROM ${schemas[0]}."${table}"`);
    const emptyBackup = await symmetricEncrypt({ key: childKey, data: '[]' });
    await admin.query(`INSERT INTO ${schemas[1]}."twoFactor" SELECT id,$1,$2,"userId",verified,"failedVerificationCount","lockedUntil" FROM ${schemas[0]}."twoFactor"`, [rewrapped, emptyBackup]);
    child.cookies = new Map(parentCookies);
    assert.equal((await request(child, 'get-session')).value, null);
    assert((await request(child, 'token')).response.status >= 400);
    child.cookies.clear();
    result = await request(child, 'sign-in/email', { email, password });
    assert.equal(result.response.status, 200); assert.equal(result.value.twoFactorRedirect, true);
    assert.equal((await request(child, 'get-session')).value, null);
    assert((await request(child, 'token')).response.status >= 400);
    result = await request(child, 'two-factor/verify-backup-code', { code: recovery[0], trustDevice: false });
    assert(result.response.status >= 400, 'Parent recovery code accepted in child');
    result = await request(child, 'two-factor/verify-totp', { code: code(plaintext), trustDevice: false });
    assert.equal(result.response.status, 200);
    assert.equal((await request(child, 'get-session')).value.user.id, userID);
    assert.equal((await request(child, 'token')).response.status, 200);
    result = await request(child, 'two-factor/generate-backup-codes', { password });
    assert.equal(result.response.status, 200);
    const fresh = result.value.backupCodes as string[];
    assert(fresh.length >= 2);
    await request(child, 'sign-out', {}); await request(child, 'sign-in/email', { email, password });
    assert.equal((await request(child, 'two-factor/verify-backup-code', { code: fresh[0], trustDevice: false })).response.status, 200);
    await request(child, 'sign-out', {}); await request(child, 'sign-in/email', { email, password });
    assert((await request(child, 'two-factor/verify-backup-code', { code: fresh[0], trustDevice: false })).response.status >= 400);
    assert.deepEqual((await pools[0].query('SELECT to_jsonb(f) AS row FROM "twoFactor" f')).rows, parentBefore);
    await request(parent, 'sign-out', {}); await request(parent, 'sign-in/email', { email, password });
    assert.equal((await request(parent, 'two-factor/verify-backup-code', { code: recovery[0], trustDevice: false })).response.status, 200);
    // Recreating the provider models re-enable with the same branch key.
    child.auth = createAuth(child.config, pools[1]); child.cookies.clear();
    await request(child, 'sign-in/email', { email, password });
    assert.equal((await request(child, 'two-factor/verify-totp', { code: code(plaintext), trustDevice: false })).response.status, 200);
  } finally {
    for (const db of pools) await db.end();
    for (const schema of schemas) await admin.query('DROP SCHEMA IF EXISTS ' + schema + ' CASCADE');
    await admin.end();
  }
});
