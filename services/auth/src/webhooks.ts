import { createHash, createPrivateKey, createPublicKey, randomUUID, sign } from "node:crypto";
import { createAuthTransport, validateAuthDestination, type ApprovedAuthDestination } from "./transport.ts";

export const authWebhookEvents = ["send.otp", "send.magic_link", "user.before_create", "user.created", "phone_number.verified", "organization.invitation.created", "organization.invitation.accepted"] as const;
export type AuthWebhookEvent = typeof authWebhookEvents[number];
export interface AuthWebhookConfig {
  destination: ApprovedAuthDestination;
  events: AuthWebhookEvent[];
  timeoutSeconds: number;
  signingSeed: string;
}
export type AuthEventEnvelope = {
  event_id: string;
  event_type: AuthWebhookEvent;
  timestamp: string;
  context: { branch_id: string; endpoint_id?: string; project_id?: string };
  user: Record<string, string | boolean>;
  event_data: Record<string, unknown>;
};
const userFields: Record<string, string> = {
  id: "id", name: "name", email: "email", image: "image", role: "role", banned: "banned",
  emailVerified: "email_verified", phoneNumber: "phone_number", phoneNumberVerified: "phone_number_verified",
  createdAt: "created_at", updatedAt: "updated_at", banReason: "ban_reason", banExpires: "ban_expires",
  twoFactorEnabled: "two_factor_enabled", isAnonymous: "is_anonymous",
};
export function authEventUser(user: Record<string, unknown>): Record<string, string | boolean> {
  const safe: Record<string, string | boolean> = {};
  for (const [key, target] of Object.entries(userFields)) {
    const value = user[key];
    if (typeof value === "boolean") safe[target] = value;
    else if (value instanceof Date && Number.isFinite(value.getTime())) safe[target] = value.toISOString();
    else if (typeof value === "string" && value.length <= (key === "image" ? 2048 : 500)) safe[target] = value;
  }
  return safe;
}
export class AuthWebhookRejected extends Error {
  readonly code: string;
  constructor(code: string, message: string) { super(message); this.code = code; }
}
export function validateAuthWebhook(config: AuthWebhookConfig): void {
  if (!config || typeof config !== "object" || Object.keys(config).some(key => !["destination", "events", "timeoutSeconds", "signingSeed"].includes(key)) ||
    !Array.isArray(config.events) || config.events.length < 1 || config.events.length > authWebhookEvents.length ||
    new Set(config.events).size !== config.events.length || config.events.some(type => !authWebhookEvents.includes(type)) ||
    !Number.isInteger(config.timeoutSeconds) || config.timeoutSeconds < 1 || config.timeoutSeconds > 10 ||
    typeof config.signingSeed !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(config.signingSeed) ||
    Buffer.from(config.signingSeed, "base64url").length !== 32 || Buffer.from(config.signingSeed, "base64url").toString("base64url") !== config.signingSeed)
    throw new Error("Invalid Auth webhook configuration");
  validateAuthDestination(config.destination);
}

export function createAuthWebhooks(config: AuthWebhookConfig, context: AuthEventEnvelope["context"], transport = createAuthTransport()) {
  validateAuthWebhook(config);
  if (!/^br_[a-f0-9]{16}$/.test(context.branch_id) ||
    (context.endpoint_id !== undefined && !/^ep_[a-f0-9]{16}$/.test(context.endpoint_id)) ||
    (context.project_id !== undefined && !/^prj_[a-f0-9]{16}$/.test(context.project_id))) throw new Error("Invalid Auth webhook context");
  const privateKey = createPrivateKey({ key: Buffer.concat([Buffer.from("302e020100300506032b657004220420", "hex"), Buffer.from(config.signingSeed, "base64url")]), format: "der", type: "pkcs8" });
  const publicKey = createPublicKey(privateKey).export({ format: "jwk" });
  const kid = "awh_" + createHash("sha256").update(publicKey.x!).digest("hex").slice(0, 24);
  const jwk = { ...publicKey, kid, alg: "EdDSA", use: "sig" };
  const enabled = (type: AuthWebhookEvent) => config.events.includes(type);
  function envelope(type: AuthWebhookEvent, user: Record<string, unknown>, data: Record<string, unknown>, id: string = randomUUID(), occurredAt = new Date()): AuthEventEnvelope {
    if (!authWebhookEvents.includes(type) || !/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(id) || !Number.isFinite(occurredAt.getTime())) throw new Error("Invalid Auth event");
    const value: AuthEventEnvelope = { event_id: id, event_type: type, timestamp: occurredAt.toISOString(), context: { ...context }, user: authEventUser(user), event_data: data };
    if (Buffer.byteLength(JSON.stringify(value)) > 32768) throw new Error("Auth event exceeds delivery limit");
    return value;
  }
  async function deliver(event: AuthEventEnvelope): Promise<void> {
    if (!enabled(event.event_type) || event.context.branch_id !== context.branch_id || event.context.project_id !== context.project_id || event.context.endpoint_id !== context.endpoint_id)
      throw new Error("Auth event destination scope mismatch");
    const payload = Buffer.from(JSON.stringify(event));
    if (payload.length > 32768) throw new Error("Auth event exceeds delivery limit");
    const timestamp = String(Date.now()), payloadB64 = payload.toString("base64url");
    const header = Buffer.from(JSON.stringify({ alg: "EdDSA", typ: "JWS", kid })).toString("base64url");
    const signaturePayload = Buffer.from(timestamp + "." + payloadB64).toString("base64url");
    const signature = header + ".." + sign(null, Buffer.from(header + "." + signaturePayload), privateKey).toString("base64url");
    const deadline = Date.now() + 15000;
    for (let attempt = 1; attempt <= 3; attempt++) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) break;
      let result;
      try {
        result = await transport.request(config.destination, "POST", payload, {
          "Content-Type": "application/json", "X-Neon-Signature": signature, "X-Neon-Signature-Kid": kid,
          "X-Neon-Timestamp": timestamp, "X-Neon-Event-Type": event.event_type,
          "X-Neon-Event-Id": event.event_id, "X-Neon-Delivery-Attempt": String(attempt),
        }, Math.min(config.timeoutSeconds * 1000, remaining), 10240);
      } catch { continue; }
      if (result.status >= 200 && result.status < 300) {
        if (event.event_type === "user.before_create") {
          let decision: { allowed?: unknown; error_code?: unknown; error_message?: unknown };
          try { decision = JSON.parse(result.body.toString()); } catch { throw new AuthWebhookRejected("SIGNUP_VALIDATION_UNAVAILABLE", "Signup validation could not be confirmed"); }
          if (!decision || typeof decision !== "object" || typeof decision.allowed !== "boolean") throw new AuthWebhookRejected("SIGNUP_VALIDATION_UNAVAILABLE", "Signup validation could not be confirmed");
          if (!decision.allowed) {
            const code = typeof decision.error_code === "string" && /^[A-Z][A-Z0-9_]{0,63}$/.test(decision.error_code) ? decision.error_code : "SIGNUP_NOT_ALLOWED";
            const message = typeof decision.error_message === "string" && decision.error_message.length <= 500 && !/[\u0000-\u001f]/.test(decision.error_message) ? decision.error_message : "Signup is not allowed";
            throw new AuthWebhookRejected(code, message);
          }
        }
        return;
      }
      if (result.status < 500 && result.status !== 408 && result.status !== 429) break;
    }
    throw new AuthWebhookRejected("AUTH_DELIVERY_UNAVAILABLE", "Authentication event delivery could not be confirmed");
  }
  return { enabled, envelope, deliver, jwks: () => ({ keys: [{ ...jwk }] }) };
}
