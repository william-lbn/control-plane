import { readFileSync } from 'node:fs';

export interface AuthConfig {
  version: 1;
  branchID: string;
  baseURL: string;
  databaseURL: string;
  secret: string;
  trustedOrigins: string[];
  allowLabHTTP: boolean;
}

// Both the URL and SQL credential come from an immutable, owned Secret file.
// Request headers can never choose an issuer, audience, SQL route or database.
export function parseConfig(value: unknown): AuthConfig {
  if (!value || typeof value !== 'object') throw new Error('Invalid Auth configuration');
  const c = value as AuthConfig;
  if (c.version !== 1 || !/^br_[a-f0-9]{16}$/.test(c.branchID) ||
      typeof c.secret !== 'string' || c.secret.length < 43 ||
      !Array.isArray(c.trustedOrigins) || c.trustedOrigins.length > 8 ||
      typeof c.allowLabHTTP !== 'boolean') throw new Error('Invalid Auth configuration');
  const base = new URL(c.baseURL);
  if (base.username || base.password || base.search || base.hash || base.hostname.includes('*') ||
      base.pathname !== `/auth/v1/${c.branchID}` ||
      (base.protocol !== 'https:' && !(c.allowLabHTTP && base.protocol === 'http:')))
    throw new Error('Invalid Auth origin');
  for (const origin of c.trustedOrigins) {
    const u = new URL(origin);
    if (u.origin !== origin || u.username || u.password || u.hostname.includes('*') ||
        (u.protocol !== 'https:' && !(c.allowLabHTTP && u.protocol === 'http:')))
      throw new Error('Invalid trusted origin');
  }
  const sql = new URL(c.databaseURL);
  if (sql.protocol !== 'postgresql:' || !sql.username || !sql.password ||
      !sql.searchParams.get('options')?.match(/^endpoint=ep-[a-f0-9]{16}$/) ||
      sql.searchParams.get('sslmode') !== 'require') throw new Error('Invalid SQL route');
  return c;
}
export function loadConfig(): AuthConfig {
  const file = process.env.NEON_AUTH_CONFIG_FILE;
  if (!file) throw new Error('NEON_AUTH_CONFIG_FILE is required');
  return parseConfig(JSON.parse(readFileSync(file, 'utf8')));
}
