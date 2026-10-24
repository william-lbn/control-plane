import { createHmac, timingSafeEqual } from "node:crypto";
import { phoneNumber } from "better-auth/plugins";
import type { Pool } from "pg";
import type { createAuthWebhooks } from "./webhooks.ts";

export const phoneActions: Record<string, "POST"> = { "phone-number/send-otp": "POST", "phone-number/verify": "POST" };
export const validPhoneNumber = (value: unknown): value is string => typeof value === "string" && /^\+[1-9][0-9]{7,14}$/.test(value);

export function createPhonePlugin(database: Pool, options: {
  secret: string;
  branchID: string;
  expiresIn: number;
  webhooks: ReturnType<typeof createAuthWebhooks>;
  markRejectedAttempt: () => void;
}) {
  if (!/^[A-Za-z0-9_-]{43}$/.test(options.secret) || !/^br_[a-f0-9]{16}$/.test(options.branchID) ||
    !Number.isInteger(options.expiresIn) || options.expiresIn < 60 || options.expiresIn > 600 || !options.webhooks.enabled("send.otp"))
    throw new Error("Approved SMS delivery and a branch phone key are required");
  const hash = (phone: string, code: string) => createHmac("sha256", Buffer.from(options.secret, "base64url"))
    .update("neon.auth.phone.otp.v1\0" + options.branchID + "\0" + phone + "\0" + code).digest();
  const plugin = phoneNumber({
    otpLength: 6, expiresIn: options.expiresIn, allowedAttempts: 3,
    requireVerification: true, phoneNumberValidator: validPhoneNumber,
    // No signUpOnVerification: the maintained plugin only signs in existing
    // users or links a verified phone to the current application session.
    sendOTP: async ({ phoneNumber: phone, code }) => {
      if (!validPhoneNumber(phone) || !/^\d{6}$/.test(code)) throw new Error("Invalid phone challenge");
      const expires = new Date(Date.now() + options.expiresIn * 1000);
      await database.query(`INSERT INTO control_phone_challenge(phone,digest,attempts,expires_at) VALUES($1,$2,0,$3)
        ON CONFLICT(phone) DO UPDATE SET digest=EXCLUDED.digest,attempts=0,expires_at=EXCLUDED.expires_at`, [phone, hash(phone, code).toString("hex"), expires]);
      // The request transaction is still open. No plaintext native challenge
      // is committed, including when delivery fails or the process exits.
      await database.query('DELETE FROM verification WHERE identifier=$1', [phone]);
      await options.webhooks.deliver(options.webhooks.envelope("send.otp", { phoneNumber: phone }, {
        otp_code: code, otp_type: "sign-in", delivery_preference: "sms", expires_at: expires.toISOString(),
      }));
    },
    verifyOTP: async ({ phoneNumber: phone, code }, ctx) => {
      if (!validPhoneNumber(phone) || !/^\d{6}$/.test(code)) { options.markRejectedAttempt(); return false; }
      // The outer native request transaction holds the per-phone advisory
      // lock. Wrong-attempt changes are explicitly committed on its 400 result.
      const value = await database.query('SELECT digest,attempts,expires_at FROM control_phone_challenge WHERE phone=$1 FOR UPDATE', [phone]);
      const row = value.rows[0];
      if (!row) { options.markRejectedAttempt(); return false; }
      if (new Date(row.expires_at).getTime() <= Date.now()) {
        await database.query('DELETE FROM control_phone_challenge WHERE phone=$1', [phone]);
        options.markRejectedAttempt(); return false;
      }
      if (!timingSafeEqual(Buffer.from(row.digest, "hex"), hash(phone, code))) {
        if (row.attempts >= 2) await database.query('DELETE FROM control_phone_challenge WHERE phone=$1', [phone]);
        else await database.query('UPDATE control_phone_challenge SET attempts=attempts+1 WHERE phone=$1', [phone]);
        options.markRejectedAttempt(); return false;
      }
      await database.query('DELETE FROM control_phone_challenge WHERE phone=$1', [phone]);
      if ((ctx?.body as {updatePhoneNumber?:unknown}|undefined)?.updatePhoneNumber !== true) {
        const existing = await database.query('SELECT id FROM "user" WHERE "phoneNumber"=$1', [phone]);
        if (existing.rowCount !== 1) { options.markRejectedAttempt(); return false; }
      }
      return true;
    },
  });
  return { ...plugin, schema: { ...plugin.schema, user: { ...plugin.schema.user, fields: {
    ...plugin.schema.user.fields,
    // Public signup/profile input must never reserve another person's phone
    // or set its verification flag. The native verified-link endpoint writes it.
    phoneNumber: { ...plugin.schema.user.fields.phoneNumber, input: false },
  } } } };
}
