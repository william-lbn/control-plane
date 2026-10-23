import { betterAuth } from "better-auth";
import { admin, jwt, emailOTP, magicLink, twoFactor } from "better-auth/plugins";
import type { Pool } from "pg";
import type { AuthConfig } from "./config.ts";
import { createMailer } from "./mail.ts";

export function authOptions(c: AuthConfig, database: Pool) {
  if (
    (c.requireEmailVerification || c.emailOTPEnabled || c.magicLinkEnabled) &&
    !c.mail
  )
    throw new Error("Email verification requires a mail transport");
  const mail = c.mail ? createMailer(c.mail, c.branchID, c.baseURL) : undefined;
  return {
    database,
    secret: c.secret,
    baseURL: c.baseURL,
    basePath: `/auth/v1/${c.branchID}`,
    trustedOrigins: [
      ...new Set([new URL(c.baseURL).origin, ...c.trustedOrigins]),
    ],
    emailAndPassword: {
      enabled: true,
      minPasswordLength: 12,
      maxPasswordLength: 128,
      requireEmailVerification: c.requireEmailVerification ?? false,
      ...(mail
        ? {
            revokeSessionsOnPasswordReset: true,
            resetPasswordTokenExpiresIn: 900,
            sendResetPassword: async ({
              user,
              url,
            }: {
              user: { email: string };
              url: string;
            }) => {
              await mail.send("password-reset", user, url);
            },
          }
        : {}),
    },
    ...(mail
      ? {
          emailVerification: {
            expiresIn: 3600,
            sendOnSignUp: c.requireEmailVerification ?? false,
            autoSignInAfterVerification: false,
            sendVerificationEmail: async ({
              user,
              url,
            }: {
              user: { email: string };
              url: string;
            }) => {
              await mail.send("email-verification", user, url);
            },
          },
        }
      : {}),
    session: {
      expiresIn: 86400,
      updateAge: 3600,
      cookieCache: { enabled: false },
    },
    advanced: {
      cookiePrefix: `neon_app_${c.branchID}`,
      // The explicit laboratory HTTP exception must never weaken HTTPS cookies.
      useSecureCookies: new URL(c.baseURL).protocol === "https:",
      // HTTPS applications may use an exact trusted origin on another site.
      // HTTP lab mode remains first-party; SameSite=None requires Secure.
      defaultCookieAttributes: {
        httpOnly: true,
        sameSite:
          new URL(c.baseURL).protocol === "https:"
            ? ("none" as const)
            : ("lax" as const),
        path: `/auth/v1/${c.branchID}`,
      },
      crossSubDomainCookies: { enabled: false },
      // Only the authenticated Go relay supplies this header, never the caller.
      ipAddress: { ipAddressHeaders: ["x-neon-client-ip"] },
    },
    rateLimit: {
      enabled: true,
      window: 60,
      max: 30,
      storage: "database" as const,
    },
    plugins: [
      // Keep persisted bans enforced even when management endpoints are disabled.
      // Only the explicitly enabled branch exposes the administrative protocol.
      admin({defaultRole:"user",adminRoles:["admin"],impersonationSessionDuration:3600,allowImpersonatingAdmins:false}),
      ...(c.twoFactorEnabled ? [twoFactor({issuer:"Neon Self Hosted",skipVerificationOnEnable:false,twoFactorCookieMaxAge:300,backupCodeOptions:{storeBackupCodes:"encrypted"}})] : []),
      ...(mail && c.magicLinkEnabled
        ? [
            magicLink({
              expiresIn: c.magicLinkExpiresIn || 300,
              disableSignUp: !c.magicLinkAllowRegistration,
              storeToken: "hashed",
              sendMagicLink: async ({ email, url }) => {
                await mail.send("magic-link", { email }, url);
              },
            }),
          ]
        : []),
      ...(mail && c.emailOTPEnabled
        ? [
            emailOTP({
              otpLength: 6,
              expiresIn: 300,
              allowedAttempts: 3,
              storeOTP: "hashed",
              disableSignUp: false,
              sendVerificationOnSignUp: false,
              sendVerificationOTP: async ({ email, otp, type }) => {
                await mail.sendOTP(type, email, otp);
              },
            }),
          ]
        : []),
      jwt({
        jwt: {
          issuer: c.baseURL,
          audience: c.branchID,
          expirationTime: "5m",
          definePayload: ({ user }: { user: { id: string } }) => ({
            branch_id: c.branchID,
            sub: user.id,
          }),
        },
        jwks: {
          keyPairConfig: { alg: "EdDSA" as const, crv: "Ed25519" as const },
        },
      }),
    ],
    // Library errors must not emit passwords, DSNs or private signing material.
    logger: { disabled: true },
  };
}
export function createAuth(c: AuthConfig, database: Pool) {
  const auth = betterAuth(authOptions(c, database));
  const handler = auth.handler;
  auth.handler = async (request: Request) => {
    if (!c.adminEnabled && new URL(request.url).pathname.startsWith(`/auth/v1/${c.branchID}/admin/`))
      return new Response(null,{status:404,headers:{"Cache-Control":"no-store"}});
    return handler(request);
  };
  return auth;
}
