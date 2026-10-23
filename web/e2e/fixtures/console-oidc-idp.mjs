// Bounded laboratory IdP for the protected Console SSO acceptance only.
// It has no metadata/Kubernetes privileges and never logs requests or secrets.
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { createHash, generateKeyPairSync, randomBytes, sign, timingSafeEqual } from 'node:crypto';

const config = JSON.parse(await readFile(process.env.NEON_OIDC_FIXTURE_CONFIG_FILE, 'utf8'));
if (
  !/^http:\/\/192\.168\.146\.101:31851$/.test(config.issuer) ||
  !/^http:\/\/192\.168\.146\.100:30788$/.test(config.console_origin) ||
  !/^org_[a-f0-9]{24}$/.test(config.organization_id) ||
  !/^sso_[0-9]{14}$/.test(config.username)
)
  throw new Error('Exact isolated laboratory fixture required');
const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
const jwk = {
  ...publicKey.export({ format: 'jwk' }),
  kid: 'local-oidc-1',
  alg: 'RS256',
  use: 'sig',
};
const requests = new Map(),
  codes = new Map();
const random = () => randomBytes(32).toString('base64url');
const equal = (a, b) =>
  timingSafeEqual(
    createHash('sha256').update(String(a)).digest(),
    createHash('sha256').update(String(b)).digest(),
  );
const redirect = config.console_origin + '/auth/sso/callback';
const json = (res, status, body) => {
  res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(body));
};
async function form(req) {
  let body = '';
  for await (const chunk of req) {
    body += chunk;
    if (Buffer.byteLength(body) > 8192) throw new Error('Body limit');
  }
  return new URLSearchParams(body);
}
const server = http.createServer(async (req, res) => {
  try {
    for (const map of [requests, codes])
      for (const [key, value] of map) if (value.expiry < Date.now()) map.delete(key);
    if (requests.size > 64 || codes.size > 64) {
      json(res, 429, { error: 'fixture_capacity' });
      return;
    }
    const url = new URL(req.url, config.issuer);
    if (req.method === 'GET' && url.pathname === '/healthz') {
      json(res, 200, { ready: true });
      return;
    }
    if (req.method === 'GET' && url.pathname === '/jwks') {
      json(res, 200, { keys: [jwk] });
      return;
    }
    if (req.method === 'GET' && url.pathname === '/authorize') {
      const q = url.searchParams;
      if (
        q.get('client_id') !== config.client_id ||
        q.get('redirect_uri') !== redirect ||
        q.get('response_type') !== 'code' ||
        q.get('scope') !== 'openid' ||
        q.get('code_challenge_method') !== 'S256' ||
        !/^[A-Za-z0-9_-]{43}$/.test(q.get('code_challenge') || '') ||
        !/^[a-f0-9]{64}$/.test(q.get('state') || '') ||
        !/^[a-f0-9]{64}$/.test(q.get('nonce') || '')
      ) {
        json(res, 400, { error: 'invalid_request' });
        return;
      }
      const key = random(),
        browser = random();
      requests.set(key, { query: new URLSearchParams(q), browser, expiry: Date.now() + 300000 });
      res.writeHead(200, {
        'Content-Type': 'text/html; charset=utf-8',
        'Cache-Control': 'no-store',
        'Referrer-Policy': 'no-referrer',
        'Content-Security-Policy': `default-src 'none'; form-action 'self' ${config.console_origin}; frame-ancestors 'none'`,
        'Set-Cookie': `fixture_oidc=${browser}; Path=/authorize; HttpOnly; SameSite=Lax; Max-Age=300`,
      });
      res.end(
        `<!doctype html><html lang="zh"><meta charset="utf-8"><title>实验室 OIDC 登录</title><h1>实验室 OIDC 身份提供商</h1><form method="post" action="/authorize"><input type="hidden" name="request" value="${key}"><label>IdP 用户名<input name="username" autocomplete="username" required></label><label>IdP 密码<input name="password" type="password" autocomplete="current-password" required></label><button>在 IdP 登录</button></form></html>`,
      );
      return;
    }
    if (req.method === 'POST' && url.pathname === '/authorize') {
      const f = await form(req),
        key = f.get('request'),
        request = requests.get(key);
      requests.delete(key);
      const browser = (req.headers.cookie || '')
        .split(';')
        .map((v) => v.trim())
        .find((v) => v.startsWith('fixture_oidc='))
        ?.slice('fixture_oidc='.length);
      if (
        !request ||
        !equal(browser, request.browser) ||
        !equal(f.get('username'), config.username) ||
        !equal(f.get('password'), config.idp_password)
      ) {
        json(res, 401, { error: 'invalid_credentials' });
        return;
      }
      const code = random();
      codes.set(code, { ...request, expiry: Date.now() + 120000 });
      const location = new URL(redirect);
      location.searchParams.set('code', code);
      location.searchParams.set('state', request.query.get('state'));
      res.writeHead(303, {
        Location: location.href,
        'Cache-Control': 'no-store',
        'Referrer-Policy': 'no-referrer',
      });
      res.end();
      return;
    }
    if (req.method === 'POST' && url.pathname === '/token') {
      const f = await form(req),
        auth = req.headers.authorization || '';
      const expected =
        'Basic ' + Buffer.from(config.client_id + ':' + config.client_secret).toString('base64');
      const code = f.get('code'),
        value = codes.get(code);
      codes.delete(code);
      if (
        !equal(auth, expected) ||
        !value ||
        f.get('grant_type') !== 'authorization_code' ||
        f.get('redirect_uri') !== redirect ||
        createHash('sha256')
          .update(f.get('code_verifier') || '')
          .digest('base64url') !== value.query.get('code_challenge')
      ) {
        json(res, 400, { error: 'invalid_grant' });
        return;
      }
      const now = Math.floor(Date.now() / 1000),
        header = Buffer.from(JSON.stringify({ alg: 'RS256', typ: 'JWT', kid: jwk.kid })).toString(
          'base64url',
        );
      const payload = Buffer.from(
        JSON.stringify({
          iss: config.issuer,
          aud: config.client_id,
          azp: config.client_id,
          sub: config.subject,
          nonce: value.query.get('nonce'),
          iat: now,
          exp: now + 600,
        }),
      ).toString('base64url');
      const signed = header + '.' + payload,
        signature = sign('RSA-SHA256', Buffer.from(signed), privateKey).toString('base64url');
      json(res, 200, {
        token_type: 'Bearer',
        access_token: random(),
        expires_in: 600,
        id_token: signed + '.' + signature,
      });
      return;
    }
    json(res, 404, { error: 'not_found' });
  } catch {
    json(res, 400, { error: 'fixture_request_rejected' });
  }
});
server.requestTimeout = 15000;
server.headersTimeout = 10000;
server.maxHeadersCount = 32;
server.listen(8089, '0.0.0.0', () => process.stdout.write('Isolated local OIDC fixture ready\n'));
