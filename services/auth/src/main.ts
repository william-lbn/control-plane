import { createServer } from 'node:http';
import { createSecureContext } from 'node:tls';
import { toNodeHandler } from 'better-auth/node';
import pg from 'pg';
import { createAuth } from './auth.ts';
import { loadConfig } from './config.ts';
import { databaseOptions } from './database.ts';

const config = loadConfig();
// max=1 and a one-second idle lifetime leave no permanent database session.
// Health probes inspect process configuration, never wake a suspended Compute.
const options = databaseOptions(config);
// Invalid/missing CA material must fail startup before any endpoint is served.
createSecureContext(options.ssl as Parameters<typeof createSecureContext>[0]);
const pool = new pg.Pool(options);
pool.on('error', () => { console.error('{"event":"auth_database_idle_error"}'); });
const auth = createAuth(config, pool);
const handler = toNodeHandler(auth);
const allowed = new Set(['sign-up/email', 'sign-in/email', 'sign-out', 'get-session',
  'token', 'jwks', 'list-sessions', 'revoke-session', 'revoke-sessions', 'revoke-other-sessions',
  'change-password', 'update-user']);
const server = createServer(async (req, res) => {
  res.setHeader('Cache-Control', 'no-store');
  const path = (req.url || '').split('?')[0];
  if (path === '/readyz' || path === '/livez') { res.end('ok'); return; }
  const action = path.slice(`/auth/v1/${config.branchID}/`.length);
  if (!path.startsWith(`/auth/v1/${config.branchID}/`) || !allowed.has(action) ||
      !['GET', 'POST', 'OPTIONS'].includes(req.method || '')) { res.writeHead(404).end(); return; }
  try { await handler(req, res); }
  catch { if (!res.headersSent) res.writeHead(503).end('{"error":"auth_runtime_unavailable"}'); else res.destroy(); }
});
server.requestTimeout = 130000;
server.headersTimeout = 10000;
server.listen(9082, '0.0.0.0');
const stop = () => { server.close(() => { void pool.end().then(() => process.exit(0)); });
  setTimeout(() => process.exit(1), 15000).unref(); };
process.on('SIGTERM', stop); process.on('SIGINT', stop);
