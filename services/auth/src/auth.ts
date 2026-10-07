import { betterAuth } from 'better-auth';
import { jwt } from 'better-auth/plugins';
import type { Pool } from 'pg';
import type { AuthConfig } from './config.ts';

export function authOptions(c: AuthConfig, database: Pool) {
  return {
    database,
    secret: c.secret,
    baseURL: c.baseURL,
    basePath: `/auth/v1/${c.branchID}`,
    trustedOrigins: [...new Set([new URL(c.baseURL).origin, ...c.trustedOrigins])],
    emailAndPassword: { enabled: true, minPasswordLength: 12, maxPasswordLength: 128 },
    session: { expiresIn: 86400, updateAge: 3600, cookieCache: { enabled: false } },
    advanced: {
      cookiePrefix: `neon_app_${c.branchID}`,
      // The explicit laboratory HTTP exception must never weaken HTTPS cookies.
      useSecureCookies: new URL(c.baseURL).protocol === 'https:',
      // HTTPS applications may use an exact trusted origin on another site.
      // HTTP lab mode remains first-party; SameSite=None requires Secure.
      defaultCookieAttributes: { httpOnly: true, sameSite: new URL(c.baseURL).protocol === 'https:' ? 'none' as const : 'lax' as const, path: `/auth/v1/${c.branchID}` },
      crossSubDomainCookies: { enabled: false },
      // Only the authenticated Go relay supplies this header, never the caller.
      ipAddress: { ipAddressHeaders: ['x-neon-client-ip'] },
    },
    rateLimit: { enabled: true, window: 60, max: 30, storage: 'database' as const },
    plugins: [jwt({
      jwt: { issuer: c.baseURL, audience: c.branchID, expirationTime: '5m',
        definePayload: ({ user }: { user: { id: string } }) => ({ branch_id: c.branchID, sub: user.id }) },
      jwks: { keyPairConfig: { alg: 'EdDSA' as const, crv: 'Ed25519' as const } },
    })],
    // Library errors must not emit passwords, DSNs or private signing material.
    logger: { disabled: true },
  };
}
export function createAuth(c: AuthConfig, database: Pool) {
  return betterAuth(authOptions(c, database));
}
