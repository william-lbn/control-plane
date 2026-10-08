import { readFileSync } from 'node:fs';
import { checkServerIdentity } from 'node:tls';
import type { PoolConfig } from 'pg';
import type { AuthConfig } from './config.ts';

// pg connection-string SSL parameters replace the supplied ssl object. Strip
// the already validated mode so the explicit CA and certificate identity are
// retained. An HTTP browser exception never disables SQL TLS verification.
export function databaseOptions(c: AuthConfig, ca = readFileSync(c.databaseTLS.caFile)): PoolConfig {
  const url = new URL(c.databaseURL);
  url.searchParams.delete('sslmode');
  return { connectionString: url.toString(), max: 1, idleTimeoutMillis: 1000,
    connectionTimeoutMillis: 120000,
    ssl: { ca, servername: c.databaseTLS.serverName, rejectUnauthorized: true,
      // pg replaces servername with the TCP host for DNS connections. Service
      // routing may differ from the operator's certificate identity; verify
      // that configured identity while retaining mandatory CA chain checks.
      checkServerIdentity: (_driverHost, certificate) =>
        checkServerIdentity(c.databaseTLS.serverName, certificate) } };
}
