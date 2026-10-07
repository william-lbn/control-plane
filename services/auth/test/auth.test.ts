import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import pg from 'pg';
import { authOptions, createAuth } from '../src/auth.ts';
import { parseConfig, type AuthConfig } from '../src/config.ts';
import { databaseOptions } from '../src/database.ts';

const database = process.env.AUTH_TEST_DATABASE_URL;
if (!database) throw new Error('AUTH_TEST_DATABASE_URL is required; Auth tests must not skip real PostgreSQL');
const disposable = new URL(database);
if (disposable.pathname !== '/auth_ci' || disposable.username !== 'auth_ci' ||
    !['127.0.0.1', 'localhost', '[::1]'].includes(disposable.hostname) || disposable.searchParams.has('options'))
  throw new Error('Auth tests require the dedicated local auth_ci user/database, never a Neon endpoint');
const schemaSQL = readFileSync(new URL('../../../api/internal/control/assets/auth-schema.sql', import.meta.url), 'utf8');
const branch = 'br_0123456789abcdef';
const config: AuthConfig = { version: 1, branchID: branch,
  baseURL: `https://auth.example.test/auth/v1/${branch}`, secret: randomBytes(48).toString('base64url'),
  databaseURL: 'postgresql://synthetic:synthetic@proxy.example.test/postgres?sslmode=verify-full&options=endpoint%3Dep-0123456789abcdef',
  databaseTLS: { caFile: '/run/auth-ca/ca.crt', serverName: 'proxy.example.test' },
  trustedOrigins: [], allowLabHTTP: false };

test('configuration rejects floating routing, weak secrets and untrusted origins', () => {
  assert.equal(parseConfig(config).branchID, branch);
  for (const change of [{ secret: 'short' }, { branchID: '../admin' },
    { baseURL: 'http://auth.example.test/auth/v1/' + branch },
    { baseURL: config.baseURL + '?origin=evil' },
    { trustedOrigins: ['https://*.example.test'] }, { trustedOrigins: ['https://a.example.test/path'] },
    { databaseTLS: { caFile: '/private/operator.key', serverName: 'proxy.example.test' } },
    { databaseTLS: { caFile: '/run/auth-ca/ca.crt', serverName: '*.example.test' } },
    { databaseURL: config.databaseURL + '&uselibpqcompat=true' },
    { databaseURL: 'postgresql://u:p@proxy.example.test/postgres?sslmode=disable' }])
    assert.throws(() => parseConfig({ ...config, ...change }));
});

test('SQL certificate verification survives HTTP laboratory configuration', () => {
  const ca = Buffer.from('synthetic-public-certificate');
  const options = databaseOptions({ ...config, allowLabHTTP: true }, ca);
  assert.equal(new URL(options.connectionString!).searchParams.get('sslmode'), null);
  assert.equal(new URL(options.connectionString!).searchParams.get('options'), 'endpoint=ep-0123456789abcdef');
  assert.deepEqual(options.ssl, { ca, servername: 'proxy.example.test', rejectUnauthorized: true });
});

test('HTTPS cookies stay secure when the laboratory exception is acknowledged', async () => {
  const pool = new pg.Pool({ connectionString: database });
  try {
    assert.equal(authOptions({ ...config, allowLabHTTP: true }, pool).advanced.useSecureCookies, true);
    assert.equal(authOptions({ ...config, allowLabHTTP: true }, pool).advanced.defaultCookieAttributes.sameSite, 'none');
    assert.equal(authOptions({ ...config, allowLabHTTP: true,
      baseURL: 'http://192.0.2.1:30788/auth/v1/' + branch }, pool).advanced.useSecureCookies, false);
  } finally { await pool.end(); }
});

test('maintained Better Auth persists accounts, revokes sessions and isolates cloned identities', async () => {
  const schema = 'auth_ci_' + randomBytes(6).toString('hex');
  const child = schema + '_child';
  const admin = new pg.Pool({ connectionString: database });
  const parentPool = new pg.Pool({ connectionString: database, options: `-c search_path=${schema},pg_catalog`, max: 1 });
  const childPool = new pg.Pool({ connectionString: database, options: `-c search_path=${child},pg_catalog`, max: 1 });
  try {
    await admin.query(`CREATE SCHEMA ${schema}; SET search_path=${schema},pg_catalog; ${schemaSQL}`);
    const auth = createAuth(config, parentPool);
    const call = (app: ReturnType<typeof createAuth>, c: AuthConfig, path: string, body?: unknown, cookie?: string, origin?: string) => {
      const headers: Record<string,string> = { Origin: origin || new URL(c.baseURL).origin, 'X-Neon-Client-IP': '192.0.2.1' };
      if (cookie) headers.Cookie = cookie;
      if (body) headers['Content-Type'] = 'application/json';
      return app.handler(new Request(c.baseURL + '/' + path, { method: body ? 'POST' : 'GET', headers, body: body ? JSON.stringify(body) : undefined }));
    };
    const identity = { email: 'synthetic@example.test', name: 'Synthetic user', password: randomBytes(24).toString('base64url') };
    const signup = await call(auth, config, 'sign-up/email', identity);
    assert.equal(signup.status, 200);
    const cookies = signup.headers.getSetCookie();
    assert.ok(cookies.some(c => c.includes(`neon_app_${branch}.session_token`) && c.includes('HttpOnly') && c.includes(`Path=/auth/v1/${branch}`) && c.includes('Secure')));
    const cookie = cookies.map(c => c.split(';')[0]).join('; ');
    const parentSession = await call(auth, config, 'get-session', undefined, cookie);
    assert.equal(parentSession.status, 200);
    const session = await parentSession.json(); assert.equal(session.user.email, identity.email);
    const tokenResponse = await call(auth, config, 'token', undefined, cookie); assert.equal(tokenResponse.status, 200);
    const token = (await tokenResponse.json()).token as string; assert.ok(token);
    const claims = JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString());
    assert.equal(claims.iss, config.baseURL); assert.equal(claims.aud, branch); assert.equal(claims.branch_id, branch);
    assert.ok(claims.exp - claims.iat <= 300); assert.equal(claims.email, undefined);
    const parentKeys = await (await call(auth, config, 'jwks')).json();
    assert.ok(parentKeys.keys.length > 0); assert.equal(parentKeys.keys[0].d, undefined);
    const stored = await parentPool.query('SELECT password FROM account');
    assert.notEqual(stored.rows[0].password, identity.password);
    assert.equal((await call(auth, config, 'sign-in/email', { email: identity.email, password: 'invalid-password' })).status, 401);
    assert.equal((await call(auth, config, 'sign-up/email', { ...identity, email: 'short@example.test', password: 'short' })).status, 400);
    assert.equal((await call(auth, config, 'sign-up/email', { ...identity, email: 'csrf@example.test' }, undefined, 'https://evil.example.test')).status, 403);
    // A physical Neon branch is verified separately in the UI suite. This
    // disposable PostgreSQL clone verifies the library's inherited row contract.
    await admin.query(`CREATE SCHEMA ${child}; SET search_path=${child},pg_catalog; ${schemaSQL}`);
    for (const table of ['user', 'account']) await admin.query(`INSERT INTO ${child}."${table}" SELECT * FROM ${schema}."${table}"`);
    const childConfig = { ...config, branchID: 'br_fedcba9876543210', baseURL: 'https://auth.example.test/auth/v1/br_fedcba9876543210', secret: randomBytes(48).toString('base64url') };
    const childAuth = createAuth(childConfig, childPool);
    const rejected = await (await call(childAuth, childConfig, 'get-session', undefined, cookie)).json();
    assert.equal(rejected, null);
    const childLogin = await call(childAuth, childConfig, 'sign-in/email', { email: identity.email, password: identity.password });
    assert.equal(childLogin.status, 200);
    const childKeys = await (await call(childAuth, childConfig, 'jwks')).json();
    assert.notEqual(parentKeys.keys[0].x, childKeys.keys[0].x);
    const childCookie = childLogin.headers.getSetCookie().map(c => c.split(';')[0]).join('; ');
    assert.equal((await call(childAuth, childConfig, 'update-user', { name: 'Changed in child' }, childCookie)).status, 200);
    assert.equal((await parentPool.query('SELECT name FROM "user"')).rows[0].name, identity.name);
    assert.equal((await childPool.query('SELECT name FROM "user"')).rows[0].name, 'Changed in child');
    assert.equal((await call(auth, config, 'sign-out', {}, cookie)).status, 200);
    assert.equal(await (await call(auth, config, 'get-session', undefined, cookie)).json(), null);
    assert.equal((await call(auth, config, 'token', undefined, cookie)).status, 401);
  } finally {
    await parentPool.end(); await childPool.end();
    await admin.query(`DROP SCHEMA IF EXISTS ${schema},${child} CASCADE`); await admin.end();
  }
});
