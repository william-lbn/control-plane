import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import net from "node:net";
import { createPublicKey, randomBytes, verify } from "node:crypto";
import { createAuthWebhooks, AuthWebhookRejected, validateAuthWebhook, type AuthWebhookConfig } from "../src/webhooks.ts";
import { createAuthTransport } from "../src/transport.ts";

test("Auth webhook signatures bind exact bytes, timestamp and branch; retries retain one event identity", async () => {
  let mode = "allow", remainingFailures = 0;
  const received: { id: string; signature: string; attempt: string; body: string; type: string }[] = [];
  const sockets = new Set<net.Socket>();
  let check: (raw: Buffer, headers: http.IncomingHttpHeaders) => void = () => {};
  const server = http.createServer(async (request, response) => {
    const chunks: Buffer[] = []; for await (const chunk of request) chunks.push(chunk);
    const raw = Buffer.concat(chunks); check(raw, request.headers);
    received.push({ id: String(request.headers["x-neon-event-id"]), signature: String(request.headers["x-neon-signature"]), attempt: String(request.headers["x-neon-delivery-attempt"]), body: raw.toString(), type: String(request.headers["x-neon-event-type"]) });
    if (remainingFailures-- > 0) response.writeHead(503).end("temporarily unavailable");
    else if (mode === "block") response.end(JSON.stringify({ allowed: false, error_code: "DOMAIN_BLOCKED", error_message: "This domain is not allowed" }));
    else if (mode === "invalid") response.end(JSON.stringify({ allowed: "yes" }));
    else if (mode === "redirect") response.writeHead(302, { Location: "/not-followed" }).end();
    else if (mode === "reject") response.writeHead(400).end("rejected");
    else if (mode === "large") response.end("x".repeat(11000));
    else response.end(JSON.stringify({ allowed: true }));
  });
  server.on("connection", socket => { sockets.add(socket); socket.on("error", () => {}); socket.on("close", () => sockets.delete(socket)); });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as net.AddressInfo).port;
  const config: AuthWebhookConfig = { destination: { url: `http://localhost:${port}/hook`, addresses: ["127.0.0.1"], allowLabHTTP: true }, events: ["user.before_create", "user.created", "send.otp"], timeoutSeconds: 1, signingSeed: randomBytes(32).toString("base64url") };
  const context = { branch_id: "br_0123456789abcdef", endpoint_id: "ep_0123456789abcdef", project_id: "prj_0123456789abcdef" };
  const hooks = createAuthWebhooks(config, context, createAuthTransport(async () => [{ address: "127.0.0.1", family: 4 }]));
  const jwk = hooks.jwks().keys[0], key = createPublicKey({ key: jwk, format: "jwk" });
  check = (raw, headers) => {
    const [header, empty, signature] = String(headers["x-neon-signature"]).split(".");
    assert.equal(empty, ""); assert.equal(JSON.parse(Buffer.from(header, "base64url").toString()).alg, "EdDSA");
    assert.equal(headers["x-neon-signature-kid"], jwk.kid);
    const encoded = Buffer.from(String(headers["x-neon-timestamp"]) + "." + raw.toString("base64url")).toString("base64url");
    assert(verify(null, Buffer.from(header + "." + encoded), key, Buffer.from(signature, "base64url")));
    assert(!verify(null, Buffer.from(header + "." + encoded + "changed"), key, Buffer.from(signature, "base64url")));
    assert.equal(JSON.parse(raw.toString()).context.branch_id, context.branch_id);
  };
  try {
    const before = hooks.envelope("user.before_create", { id: "new-user", email: "user@example.test", password: "never-deliver-password", accessToken: "never-deliver-token", internalAdmin: true }, {});
    await hooks.deliver(before); assert.equal(received.length, 1); assert(!received[0].body.includes("never-deliver"));
    mode = "block";
    await assert.rejects(hooks.deliver(hooks.envelope("user.before_create", {}, {})), error => error instanceof AuthWebhookRejected && error.code === "DOMAIN_BLOCKED" && error.message === "This domain is not allowed");
    mode = "invalid";
    await assert.rejects(hooks.deliver(hooks.envelope("user.before_create", {}, {})), error => error instanceof AuthWebhookRejected && error.code === "SIGNUP_VALIDATION_UNAVAILABLE");
    mode = "allow"; remainingFailures = 2; const offset = received.length;
    const sent = hooks.envelope("send.otp", { phoneNumber: "+15550000001" }, { otp_code: "123456", otp_type: "sign-in", delivery_preference: "sms" });
    await hooks.deliver(sent); const retries = received.slice(offset);
    assert.equal(retries.length, 3); assert.deepEqual(retries.map(x => x.attempt), ["1", "2", "3"]);
    assert.equal(new Set(retries.map(x => x.id)).size, 1); assert.equal(new Set(retries.map(x => x.body)).size, 1); assert.equal(new Set(retries.map(x => x.signature)).size, 1);
    for (const status of ["reject", "redirect"]) {
      mode = status; const previous: number = received.length;
      await assert.rejects(hooks.deliver(hooks.envelope("user.created", {}, {})), /delivery could not be confirmed/);
      assert.equal(received.length, previous + 1);
    }
    mode = "large"; const previous = received.length;
    await assert.rejects(hooks.deliver(hooks.envelope("user.created", {}, {}))); assert.equal(received.length, previous + 3);
  } finally {
    for (const socket of sockets) socket.destroy();
    await new Promise<void>(resolve => server.close(() => resolve()));
  }
});

test("Auth webhook configuration and payloads keep signing secrets and foreign scope out of delivery", async () => {
  const config: AuthWebhookConfig = { destination: { url: "http://localhost:12345/hook", addresses: ["127.0.0.1"], allowLabHTTP: true }, events: ["user.created"], timeoutSeconds: 5, signingSeed: randomBytes(32).toString("base64url") };
  const hooks = createAuthWebhooks(config, { branch_id: "br_0123456789abcdef" });
  assert(!JSON.stringify(hooks.jwks()).includes(config.signingSeed)); assert(!("d" in hooks.jwks().keys[0]));
  assert.equal(hooks.enabled("send.otp"), false);
  for (const change of [{ events: [] }, { events: ["unknown"] }, { events: ["user.created", "user.created"] }, { timeoutSeconds: 0 }, { timeoutSeconds: 11 }, { signingSeed: "invalid" }])
    assert.throws(() => validateAuthWebhook({ ...config, ...change } as AuthWebhookConfig));
  assert.throws(() => hooks.envelope("user.created", {}, { value: "x".repeat(32768) }));
  assert.throws(() => hooks.envelope("user.created", {}, {}, "-".repeat(36)));
  const foreign = hooks.envelope("user.created", {}, {}); foreign.context.branch_id = "br_fedcba9876543210";
  await assert.rejects(hooks.deliver(foreign), /scope mismatch/);
});
