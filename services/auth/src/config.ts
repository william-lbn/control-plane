import { readFileSync } from "node:fs";
import { loadMailConfig, type MailConfig } from "./mail.ts";
import { validateAuthWebhook, type AuthWebhookConfig } from "./webhooks.ts";
import { validateAuthOAuthProviders, type AuthOAuthProvider } from "./oauth.ts";

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
  twoFactorEnabled?: boolean;
  adminEnabled?: boolean;
  organizationEnabled?: boolean;
  organizationLimit?: number;
  organizationMembershipLimit?: number;
  organizationCreatorRole?: "owner" | "admin";
  organizationInvitationEmail?: boolean;
  organizationInvitationURL?: string;
  projectID?: string;
  endpointID?: string;
  webhook?: AuthWebhookConfig;
  phoneNumberEnabled?: boolean;
  phoneNumberExpiresIn?: number;
  phoneNumberSecret?: string;
  oauthProviders?: AuthOAuthProvider[];
}

export const authLinkDeliveryEnabled = (config: AuthConfig) => !!config.mail || !!config.webhook?.events.includes("send.magic_link");
export const authOTPDeliveryEnabled = (config: AuthConfig) => !!config.mail || !!config.webhook?.events.includes("send.otp");

// Both the URL and SQL credential come from an immutable, owned Secret file.
// Request headers can never choose an issuer, audience, SQL route or database.
export function parseConfig(value: unknown): AuthConfig {
  if (!value || typeof value !== "object")
    throw new Error("Invalid Auth configuration");
  const c = value as AuthConfig;
  if(c.oauthProviders!==undefined)validateAuthOAuthProviders(c.oauthProviders,c.allowLabHTTP);
  if (c.webhook !== undefined) validateAuthWebhook(c.webhook);
  if ((c.projectID !== undefined && !/^prj_[a-f0-9]{16}$/.test(c.projectID)) ||
    (c.endpointID !== undefined && !/^ep_[a-f0-9]{16}$/.test(c.endpointID)) ||
    (c.phoneNumberEnabled !== undefined && typeof c.phoneNumberEnabled !== "boolean") ||
    (!c.phoneNumberEnabled && (c.phoneNumberExpiresIn !== undefined || c.phoneNumberSecret !== undefined)) ||
    (c.webhook?.destination.allowLabHTTP && !c.allowLabHTTP)) throw new Error("Invalid branch integration policy");
  if (c.phoneNumberEnabled && (!c.webhook?.events.includes("send.otp") || !Number.isInteger(c.phoneNumberExpiresIn) ||
    c.phoneNumberExpiresIn! < 60 || c.phoneNumberExpiresIn! > 600 || typeof c.phoneNumberSecret !== "string" ||
    !/^[A-Za-z0-9_-]{43}$/.test(c.phoneNumberSecret) || Buffer.from(c.phoneNumberSecret,"base64url").toString("base64url") !== c.phoneNumberSecret))
    throw new Error("Phone login requires approved SMS delivery and a branch phone key");
  if (c.adminEnabled !== undefined && typeof c.adminEnabled !== "boolean")
    throw new Error("Invalid application admin policy");
  if (c.organizationEnabled !== undefined && typeof c.organizationEnabled !== "boolean")
    throw new Error("Invalid application organization policy");
  for (const [value,max] of [[c.organizationLimit,100],[c.organizationMembershipLimit,1000]] as const)
    if (value!==undefined && (!Number.isInteger(value)||value<1||value>max))
      throw new Error("Invalid application organization limit");
  if (c.organizationCreatorRole!==undefined && !["owner","admin"].includes(c.organizationCreatorRole))
    throw new Error("Invalid application organization creator role");
  if (c.organizationInvitationEmail!==undefined && typeof c.organizationInvitationEmail!=="boolean")
    throw new Error("Invalid organization invitation mail policy");
  if (!c.organizationEnabled && [c.organizationLimit,c.organizationMembershipLimit,c.organizationCreatorRole,c.organizationInvitationEmail,c.organizationInvitationURL].some(v=>v!==undefined))
    throw new Error("Organization options require the plugin to be enabled");
  if(c.twoFactorEnabled!==undefined && typeof c.twoFactorEnabled!=="boolean")throw new Error("Invalid two-factor policy");
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
  if (c.organizationInvitationEmail) {
    if (!c.requireEmailVerification || typeof c.organizationInvitationURL!=="string" || c.organizationInvitationURL.length>2048)
      throw new Error("Organization invitations require verified email and a fixed application URL");
    const target=new URL(c.organizationInvitationURL);
    if (target.username||target.password||target.search||target.hash||
      (target.protocol!=="https:"&&!(c.allowLabHTTP&&target.protocol==="http:"))||
      ![base.origin,...c.trustedOrigins].includes(target.origin))
      throw new Error("Organization invitation URL must belong to a trusted application origin");
  } else if (c.organizationInvitationURL!==undefined)
    throw new Error("Organization invitation URL requires invitation email");
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
    ((config.requireEmailVerification || config.magicLinkEnabled) && !authLinkDeliveryEnabled(config)) ||
    (config.emailOTPEnabled && !authOTPDeliveryEnabled(config)) ||
    (config.organizationInvitationEmail && !config.mail)
  )
    throw new Error("Email verification requires a mail transport");
  return config;
}
