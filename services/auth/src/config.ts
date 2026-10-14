import { readFileSync } from "node:fs";
import { loadMailConfig, type MailConfig } from "./mail.ts";

export interface AuthConfig {
  version: 1;
  branchID: string;
  baseURL: string;
  databaseURL: string;
  secret: string;
  trustedOrigins: string[];
  allowLabHTTP: boolean;
  databaseTLS: { caFile: "/run/auth-ca/ca.crt"; serverName: string };
  mail?: MailConfig;
  requireEmailVerification?: boolean;
  emailOTPEnabled?: boolean;
  magicLinkEnabled?: boolean;
  magicLinkExpiresIn?: number;
  magicLinkAllowRegistration?: boolean;
}

// Both the URL and SQL credential come from an immutable, owned Secret file.
// Request headers can never choose an issuer, audience, SQL route or database.
export function parseConfig(value: unknown): AuthConfig {
  if (!value || typeof value !== "object")
    throw new Error("Invalid Auth configuration");
  const c = value as AuthConfig;
  if (
    (c.magicLinkEnabled !== undefined &&
      typeof c.magicLinkEnabled !== "boolean") ||
    (c.magicLinkAllowRegistration !== undefined &&
      typeof c.magicLinkAllowRegistration !== "boolean") ||
    (c.magicLinkExpiresIn !== undefined &&
      c.magicLinkExpiresIn !== 0 &&
      (!Number.isInteger(c.magicLinkExpiresIn) ||
        c.magicLinkExpiresIn < 300 ||
        c.magicLinkExpiresIn > 86400))
  )
    throw new Error("Invalid magic link policy");
  if (
    !c.magicLinkEnabled &&
    (c.magicLinkAllowRegistration || c.magicLinkExpiresIn)
  )
    throw new Error("Magic link policy disabled");
  if (c.emailOTPEnabled !== undefined && typeof c.emailOTPEnabled !== "boolean")
    throw new Error("Invalid email OTP policy");
  if (
    c.requireEmailVerification !== undefined &&
    typeof c.requireEmailVerification !== "boolean"
  )
    throw new Error("Invalid email verification policy");
  if (
    c.version !== 1 ||
    !/^br_[a-f0-9]{16}$/.test(c.branchID) ||
    typeof c.secret !== "string" ||
    c.secret.length < 43 ||
    !Array.isArray(c.trustedOrigins) ||
    c.trustedOrigins.length > 8 ||
    typeof c.allowLabHTTP !== "boolean"
  )
    throw new Error("Invalid Auth configuration");
  const base = new URL(c.baseURL);
  if (
    base.username ||
    base.password ||
    base.search ||
    base.hash ||
    base.hostname.includes("*") ||
    base.pathname !== `/auth/v1/${c.branchID}` ||
    (base.protocol !== "https:" &&
      !(c.allowLabHTTP && base.protocol === "http:"))
  )
    throw new Error("Invalid Auth origin");
  for (const origin of c.trustedOrigins) {
    const u = new URL(origin);
    if (
      u.origin !== origin ||
      u.username ||
      u.password ||
      u.hostname.includes("*") ||
      (u.protocol !== "https:" && !(c.allowLabHTTP && u.protocol === "http:"))
    )
      throw new Error("Invalid trusted origin");
  }
  const sql = new URL(c.databaseURL);
  if (
    sql.protocol !== "postgresql:" ||
    !sql.username ||
    !sql.password ||
    !sql.searchParams.get("options")?.match(/^endpoint=ep-[a-f0-9]{16}$/) ||
    sql.searchParams.get("sslmode") !== "verify-full" ||
    sql.hash ||
    [...sql.searchParams.keys()].some(
      (k) => !["sslmode", "options"].includes(k),
    )
  )
    throw new Error("Invalid SQL route");
  if (
    c.databaseTLS?.caFile !== "/run/auth-ca/ca.crt" ||
    typeof c.databaseTLS.serverName !== "string" ||
    c.databaseTLS.serverName.length > 253 ||
    !c.databaseTLS.serverName
      .split(".")
      .every((label) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/i.test(label))
  )
    throw new Error(
      "An explicit database CA and certificate identity are required",
    );
  return c;
}
export function loadConfig(): AuthConfig {
  const file = process.env.NEON_AUTH_CONFIG_FILE;
  if (!file) throw new Error("NEON_AUTH_CONFIG_FILE is required");
  const config = parseConfig(JSON.parse(readFileSync(file, "utf8")));
  config.mail = loadMailConfig();
  if (
    (config.requireEmailVerification ||
      config.emailOTPEnabled ||
      config.magicLinkEnabled) &&
    !config.mail
  )
    throw new Error("Email verification requires a mail transport");
  return config;
}
