import { readFileSync } from "node:fs";
import nodemailer from "nodemailer";

export interface MailConfig {
  version: 1;
  host: string;
  port: number;
  from: string;
  secure: boolean;
  labCleartext: boolean;
  username?: string;
  password?: string;
  caFile?: "/run/auth-mail/ca.crt";
}

// Only operator Secret input selects a transport. An application request never
// chooses the SMTP peer, credentials, sender, attachment or template source.
export function parseMailConfig(value: unknown): MailConfig {
  if (!value || typeof value !== "object")
    throw new Error("Invalid mail configuration");
  const c = value as MailConfig;
  if (
    Object.keys(c).some(
      (k) =>
        ![
          "version",
          "host",
          "port",
          "from",
          "secure",
          "labCleartext",
          "username",
          "password",
          "caFile",
        ].includes(k),
    ) ||
    c.version !== 1 ||
    typeof c.host !== "string" ||
    !/^[a-z0-9.-]{1,253}$/i.test(c.host) ||
    !Number.isInteger(c.port) ||
    c.port < 1 ||
    c.port > 65535 ||
    typeof c.secure !== "boolean" ||
    typeof c.labCleartext !== "boolean" ||
    typeof c.from !== "string" ||
    !/^[a-z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-z0-9.-]+$/i.test(c.from) ||
    c.from.length > 254 ||
    (c.username === undefined) !== (c.password === undefined) ||
    (c.username !== undefined &&
      (typeof c.username !== "string" ||
        c.username.length < 1 ||
        c.username.length > 256 ||
        /[\r\n\0]/.test(c.username))) ||
    (c.password !== undefined &&
      (typeof c.password !== "string" ||
        c.password.length < 1 ||
        c.password.length > 1024)) ||
    (c.caFile !== undefined && c.caFile !== "/run/auth-mail/ca.crt")
  )
    throw new Error("Invalid mail configuration");
  if (
    c.labCleartext &&
    (c.secure ||
      c.username !== undefined ||
      c.caFile !== undefined ||
      (!["127.0.0.1", "localhost"].includes(c.host) &&
        !/^neon-auth-mailbox\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.svc\.cluster\.local$/.test(
          c.host,
        )))
  )
    throw new Error("Lab mail requires the explicit isolated receiver");
  return c;
}

export function loadMailConfig(): MailConfig | undefined {
  const file = process.env.NEON_AUTH_MAIL_CONFIG_FILE;
  if (!file) return undefined;
  if (file !== "/run/auth-mail/config.json")
    throw new Error("Fixed operator mail Secret path required");
  return parseMailConfig(JSON.parse(readFileSync(file, "utf8")));
}

export function createMailer(c: MailConfig, branch: string, baseURL: string) {
  parseMailConfig(c);
  if (!/^br_[a-f0-9]{16}$/.test(branch)) throw new Error("Invalid mail branch");
  const base = new URL(baseURL);
  const transport = nodemailer.createTransport({
    host: c.host,
    port: c.port,
    secure: c.secure,
    // TLS is mandatory for real SMTP. Cleartext is only a named local fixture;
    // never send SMTP credentials over it. No pool keeps idle resources alive.
    requireTLS: !c.labCleartext,
    ignoreTLS: c.labCleartext,
    pool: false,
    ...(c.username !== undefined
      ? { auth: { user: c.username, pass: c.password! } }
      : {}),
    tls: {
      rejectUnauthorized: true,
      servername: c.host,
      ...(c.caFile ? { ca: readFileSync(c.caFile) } : {}),
    },
    connectionTimeout: 10000,
    greetingTimeout: 10000,
    socketTimeout: 15000,
    logger: false,
    debug: false,
    disableFileAccess: true,
    disableUrlAccess: true,
  });
  async function send(
    kind: "password-reset" | "email-verification",
    user: { email: string },
    url: string,
  ) {
    const target = new URL(url);
    if (
      url.length > 4096 ||
      target.origin !== base.origin ||
      !target.pathname.startsWith(base.pathname + "/") ||
      target.username ||
      target.password ||
      typeof user.email !== "string" ||
      user.email.length > 254 ||
      /[\r\n\0<>,;]/.test(user.email)
    )
      throw new Error("Invalid Auth mail destination");
    try {
      await transport.sendMail({
        from: c.from,
        to: { name: "", address: user.email },
        subject:
          kind === "password-reset"
            ? "Reset your application password"
            : "Verify your application email",
        text:
          (kind === "password-reset"
            ? "Use this one-time link to reset your application password."
            : "Use this link to verify your application email.") +
          "\n\n" +
          url +
          "\n\nIf you did not request this, ignore this message.",
        headers: { "X-Neon-Auth-Branch": branch, "X-Neon-Auth-Event": kind },
        disableFileAccess: true,
        disableUrlAccess: true,
      });
    } catch {
      // Provider errors may contain addresses, URLs or credentials. Neither
      // the caller nor stdout receives the original exception/transcript.
      throw new Error("Auth mail transport unavailable");
    }
  }
  async function sendOTP(kind: string, email: string, otp: string) {
    const subjects: Record<string, string> = {
      "sign-in": "Your application sign-in code",
      "email-verification": "Your application email verification code",
      "forget-password": "Your application password recovery code",
    };
    if (
      !Object.hasOwn(subjects, kind) ||
      !/^\d{6}$/.test(otp) ||
      typeof email !== "string" ||
      email.length > 254 ||
      /[\r\n\0<>,;]/.test(email)
    )
      throw new Error("Invalid Auth OTP destination");
    try {
      await transport.sendMail({
        from: c.from,
        to: { name: "", address: email },
        subject: subjects[kind],
        text:
          "Your one-time code is:\n\n" +
          otp +
          "\n\nIt expires in five minutes. If you did not request this, ignore it.",
        headers: {
          "X-Neon-Auth-Branch": branch,
          "X-Neon-Auth-Event": "email-otp:" + kind,
        },
        disableFileAccess: true,
        disableUrlAccess: true,
      });
    } catch {
      throw new Error("Auth mail transport unavailable");
    }
  }
  return { send, sendOTP, close: () => transport.close() };
}
