import { createServer } from "node:http";
import { createSecureContext } from "node:tls";
import { toNodeHandler } from "better-auth/node";
import pg from "pg";
import { createAuth } from "./auth.ts";
import { loadConfig } from "./config.ts";
import { authLinkDeliveryEnabled, authOTPDeliveryEnabled } from "./config.ts";
import { databaseOptions } from "./database.ts";
import { adminActions } from "./admin.ts";
import { organizationActions } from "./organization.ts";
import { phoneActions } from "./phone.ts";

const config = loadConfig();
// max=1 and a one-second idle lifetime leave no permanent database session.
// Health probes inspect process configuration, never wake a suspended Compute.
const options = databaseOptions(config);
// Invalid/missing CA material must fail startup before any endpoint is served.
createSecureContext(options.ssl as Parameters<typeof createSecureContext>[0]);
const pool = new pg.Pool(options);
pool.on("error", () => {
  console.error('{"event":"auth_database_idle_error"}');
});
const auth = createAuth(config, pool);
const handler = toNodeHandler(auth);
const allowed = new Set([
  "sign-up/email",
  "sign-in/email",
  "sign-out",
  "get-session",
  "token",
  "jwks",
  "list-sessions",
  "revoke-session",
  "revoke-sessions",
  "revoke-other-sessions",
  "change-password",
  "update-user",
  ...(config.twoFactorEnabled ? ["two-factor/enable","two-factor/disable","two-factor/get-totp-uri","two-factor/verify-totp","two-factor/verify-backup-code","two-factor/generate-backup-codes"] : []),
  ...(authLinkDeliveryEnabled(config) && config.magicLinkEnabled
    ? ["sign-in/magic-link", "magic-link/verify"]
    : []),
  ...(authOTPDeliveryEnabled(config) && config.emailOTPEnabled
    ? [
        "email-otp/send-verification-otp",
        "email-otp/check-verification-otp",
        "email-otp/verify-email",
        "email-otp/request-password-reset",
        "email-otp/reset-password",
        "sign-in/email-otp",
      ]
    : []),
  ...(authLinkDeliveryEnabled(config)
    ? [
        "request-password-reset",
        "reset-password",
        "send-verification-email",
        "verify-email",
      ]
    : []),
]);
const server = createServer(async (req, res) => {
  res.setHeader("Cache-Control", "no-store");
  const path = (req.url || "").split("?")[0];
  if (path === "/readyz" || path === "/livez") {
    res.end("ok");
    return;
  }
  const action = path.slice(`/auth/v1/${config.branchID}/`.length);
  const adminAction = config.adminEnabled && Object.hasOwn(adminActions,action) &&
    (req.method===adminActions[action] || req.method==="OPTIONS");
  const organizationAction = config.organizationEnabled && Object.hasOwn(organizationActions,action) &&
    (req.method===organizationActions[action] || req.method==="OPTIONS");
  const phoneAction = config.phoneNumberEnabled && Object.hasOwn(phoneActions,action) && (req.method==='POST'||req.method==='OPTIONS');
  const hookKeys = !!config.webhook && action==='.well-known/jwks.json' && req.method==='GET';
  const hookManagement = !!config.webhook && config.adminEnabled && ((action==='webhooks/events'&&req.method==='GET')||(action==='webhooks/drain'&&req.method==='POST'));
  const oauthAction=!!config.oauthProviders?.length&&((['sign-in/social','link-social','unlink-account'].includes(action)&&['POST','OPTIONS'].includes(req.method||''))||(action==='list-accounts'&&req.method==='GET')||(config.oauthProviders.some(p=>action==='callback/'+p.id)&&req.method==='GET'));
  const resetCallback =
    authLinkDeliveryEnabled(config) &&
    /^reset-password\/[a-zA-Z0-9_-]{20,128}$/.test(action) &&
    req.method === "GET";
  if (
    !path.startsWith(`/auth/v1/${config.branchID}/`) ||
    (!allowed.has(action) && !resetCallback && !adminAction && !organizationAction && !phoneAction && !hookKeys && !hookManagement && !oauthAction) ||
    !["GET", "POST", "OPTIONS"].includes(req.method || "")
  ) {
    res.writeHead(404).end();
    return;
  }
  try {
    await handler(req, res);
  } catch {
    if (!res.headersSent)
      res.writeHead(503).end('{"error":"auth_runtime_unavailable"}');
    else res.destroy();
  }
});
server.requestTimeout = 130000;
server.headersTimeout = 10000;
server.listen(9082, "0.0.0.0");
const stop = () => {
  server.close(() => {
    void pool.end().then(() => process.exit(0));
  });
  setTimeout(() => process.exit(1), 15000).unref();
};
process.on("SIGTERM", stop);
process.on("SIGINT", stop);
