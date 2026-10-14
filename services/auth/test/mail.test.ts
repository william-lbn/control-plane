import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:net";
import { once } from "node:events";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import pg from "pg";
import { createAuth } from "../src/auth.ts";
import type { AuthConfig } from "../src/config.ts";
import { createMailer, parseMailConfig, type MailConfig } from "../src/mail.ts";

// Disposable SMTP receiver. The sender is the actual pinned Nodemailer stack;
// only the remote mailbox is a fixture. No message/token appears in stdout.
async function mailbox() {
  const messages: string[] = [];
  const server = createServer((socket) => {
    socket.setTimeout(20000, () => socket.destroy());
    socket.write("220 localhost ESMTP\r\n");
    let buffer = "",
      data = false,
      body = "";
    socket.on("data", (chunk) => {
      buffer += chunk.toString();
      if (buffer.length + body.length > 65536) {
        socket.destroy();
        return;
      }
      while (buffer.includes("\r\n")) {
        const end = buffer.indexOf("\r\n"),
          line = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);
        if (data) {
          if (line === ".") {
            messages.push(body);
            body = "";
            data = false;
            socket.write("250 queued\r\n");
          } else
            body += (line.startsWith("..") ? line.slice(1) : line) + "\r\n";
          continue;
        }
        if (/^EHLO /i.test(line))
          socket.write("250-localhost\r\n250-8BITMIME\r\n250 SMTPUTF8\r\n");
        else if (/^(MAIL FROM:|RCPT TO:|RSET)/i.test(line))
          socket.write("250 ok\r\n");
        else if (line === "DATA") {
          data = true;
          socket.write("354 end with dot\r\n");
        } else if (line === "QUIT") {
          socket.end("221 bye\r\n");
        } else socket.write("502 unsupported\r\n");
      }
    });
    socket.on("error", () => {});
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  if (!address || typeof address === "string")
    throw new Error("SMTP fixture unavailable");
  return {
    messages,
    port: address.port,
    close: () =>
      new Promise<void>((resolve, reject) =>
        server.close((e) => (e ? reject(e) : resolve())),
      ),
  };
}
function mailURL(raw: string): URL {
  const end = raw.indexOf("\r\n\r\n"),
    headers = raw.slice(0, end),
    encoded = raw.slice(end + 4);
  const text = /Content-Transfer-Encoding: base64/i.test(headers)
    ? Buffer.from(encoded.replace(/\s/g, ""), "base64").toString()
    : encoded
        .replace(/=\r\n/g, "")
        .replace(/=([0-9A-F]{2})/gi, (_, v) =>
          String.fromCharCode(parseInt(v, 16)),
        );
  const link = text.match(
    /https:\/\/auth\.example\.test\/auth\/v1\/[^\s]+/,
  )?.[0];
  if (!link) throw new Error("Expected private Auth link absent");
  return new URL(link);
}

test("SMTP operator schema rejects unsafe transport and credentials", () => {
  const base: MailConfig = {
    version: 1,
    host: "127.0.0.1",
    port: 1025,
    from: "noreply@example.test",
    secure: false,
    labCleartext: true,
  };
  assert.equal(parseMailConfig(base).host, "127.0.0.1");
  for (const bad of [
    { host: "unrelated.example" },
    { username: "user", password: "private-synthetic" },
    { from: "x@example.test\r\nBcc:other@example.test" },
    { caFile: "/tmp/unapproved" },
    { unknown: "value" },
  ])
    assert.throws(() => parseMailConfig({ ...base, ...bad }));
});

test("actual PostgreSQL and SMTP verify email and one-time password recovery", async () => {
  const dsn = process.env.AUTH_TEST_DATABASE_URL;
  if (!dsn) throw new Error("Dedicated auth_ci required");
  const input = new URL(dsn);
  if (
    input.pathname !== "/auth_ci" ||
    input.username !== "auth_ci" ||
    !["127.0.0.1", "localhost", "[::1]"].includes(input.hostname)
  )
    throw new Error("Disposable local auth_ci only");
  const smtp = await mailbox(),
    schema = "auth_mail_" + randomBytes(8).toString("hex");
  const admin = new pg.Pool({ connectionString: dsn, max: 1 }),
    db = new pg.Pool({
      connectionString: dsn,
      options: `-c search_path=${schema},pg_catalog`,
      max: 2,
    });
  const branch = "br_0123456789abcdef";
  const config: AuthConfig = {
    version: 1,
    branchID: branch,
    baseURL: `https://auth.example.test/auth/v1/${branch}`,
    secret: randomBytes(48).toString("base64url"),
    trustedOrigins: [],
    allowLabHTTP: false,
    databaseURL:
      "postgresql://synthetic:synthetic@proxy.example.test/postgres?sslmode=verify-full&options=endpoint%3Dep-0123456789abcdef",
    databaseTLS: {
      caFile: "/run/auth-ca/ca.crt",
      serverName: "proxy.example.test",
    },
    mail: {
      version: 1,
      host: "127.0.0.1",
      port: smtp.port,
      from: "noreply@example.test",
      secure: false,
      labCleartext: true,
    },
  };
  try {
    const ddl = readFileSync(
      new URL(
        "../../../api/internal/control/assets/auth-schema.sql",
        import.meta.url,
      ),
      "utf8",
    );
    await admin.query(
      `CREATE SCHEMA ${schema}; SET search_path=${schema},pg_catalog; ${ddl}`,
    );
    const auth = createAuth(config, db);
    const call = (action: string, body?: unknown, cookie?: string) =>
      auth.handler(
        new Request(config.baseURL + "/" + action, {
          method: body ? "POST" : "GET",
          headers: {
            Origin: new URL(config.baseURL).origin,
            ...(body ? { "Content-Type": "application/json" } : {}),
            ...(cookie ? { Cookie: cookie } : {}),
          },
          body: body ? JSON.stringify(body) : undefined,
        }),
      );
    const cookie = (r: Response) =>
      r.headers
        .getSetCookie()
        .map((v) => v.split(";")[0])
        .join("; ");
    const email = "mail-user@example.test",
      password = randomBytes(24).toString("base64url"),
      nextPassword = randomBytes(24).toString("base64url");
    const signup = await call("sign-up/email", {
      email,
      name: "Mail user",
      password,
    });
    assert.equal(signup.status, 200);
    const originalCookie = cookie(signup);
    assert.equal(
      (await call("send-verification-email", { email }, originalCookie)).status,
      200,
    );
    assert.equal(smtp.messages.length, 1);
    const verify = mailURL(smtp.messages.pop()!);
    assert.equal(verify.pathname, `/auth/v1/${branch}/verify-email`);
    const verified = await auth.handler(new Request(verify));
    assert.equal(verified.status, 302);
    assert.equal(
      new URL(verified.headers.get("location")!, config.baseURL).origin,
      new URL(config.baseURL).origin,
    );
    assert.equal(
      (await (await call("get-session", undefined, originalCookie)).json()).user
        .emailVerified,
      true,
    );
    assert.equal(
      (
        await call("request-password-reset", {
          email,
          redirectTo: new URL(config.baseURL).origin,
        })
      ).status,
      200,
    );
    assert.equal(smtp.messages.length, 1);
    const reset = mailURL(smtp.messages.pop()!),
      token = reset.pathname.split("/").at(-1)!;
    assert.equal(
      reset.pathname.startsWith(`/auth/v1/${branch}/reset-password/`),
      true,
    );
    const invalid = await call("reset-password", {
      token: "invalid-token",
      newPassword: nextPassword,
    });
    assert.ok(invalid.status >= 400);
    const complete = await call("reset-password", {
      token,
      newPassword: nextPassword,
    });
    assert.equal(complete.status, 200);
    assert.equal(
      (await (await call("get-session", undefined, originalCookie)).json()) ===
        null,
      true,
    );
    assert.equal((await call("token", undefined, originalCookie)).status, 401);
    assert.ok(
      (await call("reset-password", { token, newPassword: password })).status >=
        400,
    );
    assert.equal(
      (await call("sign-in/email", { email, password })).status,
      401,
    );
    assert.equal(
      (await call("sign-in/email", { email, password: nextPassword })).status,
      200,
    );
    const before = smtp.messages.length;
    assert.equal(
      (await call("request-password-reset", { email: "unknown@example.test" }))
        .status,
      200,
    );
    assert.equal(smtp.messages.length, before);
    assert.equal((await call("request-password-reset", { email })).status, 200);
    assert.equal(smtp.messages.length, 1);
    const expiredToken = mailURL(smtp.messages.pop()!)
      .pathname.split("/")
      .at(-1)!;
    const lifetime = await db.query(
      'SELECT EXTRACT(EPOCH FROM ("expiresAt"-now()))::int AS seconds FROM verification WHERE identifier=$1',
      ["reset-password:" + expiredToken],
    );
    assert.equal(lifetime.rows.length, 1);
    assert.equal(
      lifetime.rows[0].seconds <= 900 && lifetime.rows[0].seconds >= 890,
      true,
    );
    await db.query(
      "UPDATE verification SET \"expiresAt\"=now()-interval '1 minute' WHERE identifier=$1",
      ["reset-password:" + expiredToken],
    );
    assert.ok(
      (
        await call("reset-password", {
          token: expiredToken,
          newPassword: password,
        })
      ).status >= 400,
    );
    // Exercise the required-email product policy using the real library and
    // durable rows, not a fabricated session or a transport-only unit test.
    const required = createAuth(
      { ...config, requireEmailVerification: true },
      db,
    );
    const requiredCall = (action: string, body?: unknown) =>
      required.handler(
        new Request(config.baseURL + "/" + action, {
          method: body ? "POST" : "GET",
          headers: {
            Origin: new URL(config.baseURL).origin,
            ...(body ? { "Content-Type": "application/json" } : {}),
          },
          body: body ? JSON.stringify(body) : undefined,
        }),
      );
    const secondEmail = "required@example.test";
    const requiredSignup = await requiredCall("sign-up/email", {
      email: secondEmail,
      name: "Required mail",
      password,
    });
    assert.equal(requiredSignup.status, 200);
    assert.equal((await requiredSignup.json()).token, null);
    assert.equal(smtp.messages.length, 1);
    assert.equal(
      (await requiredCall("sign-in/email", { email: secondEmail, password }))
        .status,
      403,
    );
    // A denied login may resend verification mail. Verify the original actual
    // message; never infer validation merely from accepting the HTTP request.
    const requiredLink = mailURL(smtp.messages.shift()!);
    assert.equal(
      (await required.handler(new Request(requiredLink))).status,
      302,
    );
    await new Promise((resolve) => setTimeout(resolve, 11000));
    assert.equal(
      (await requiredCall("sign-in/email", { email: secondEmail, password }))
        .status,
      200,
    );
  } finally {
    await db.end();
    await admin.query(`DROP SCHEMA IF EXISTS ${schema} CASCADE`);
    await admin.end();
    await smtp.close();
  }
});

test("real SMTP mode refuses a peer without STARTTLS before sending a message", async () => {
  const smtp = await mailbox(),
    branch = "br_0123456789abcdef";
  const base = "https://auth.example.test/auth/v1/" + branch;
  const sender = createMailer(
    {
      version: 1,
      host: "127.0.0.1",
      port: smtp.port,
      from: "noreply@example.test",
      secure: false,
      labCleartext: false,
    },
    branch,
    base,
  );
  try {
    await assert.rejects(
      sender.send(
        "email-verification",
        { email: "tls-user@example.test" },
        base + "/verify-email?token=synthetic-fixture",
      ),
      { message: "Auth mail transport unavailable" },
    );
    assert.equal(smtp.messages.length, 0);
  } finally {
    sender.close();
    await smtp.close();
  }
});
