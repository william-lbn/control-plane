import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { createAuthTransport, validateAuthDestination, type ApprovedAuthDestination } from "../src/transport.ts";

async function listen(server: net.Server) {
  await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  return (server.address() as net.AddressInfo).port;
}
function track(server: net.Server, sockets: Set<net.Socket>) {
  server.on("connection", socket => { sockets.add(socket); socket.on("error", () => {}); socket.on("close", () => sockets.delete(socket)); });
}
async function close(server: net.Server, sockets: Set<net.Socket>) {
  for (const socket of sockets) socket.destroy();
  await new Promise<void>(resolve => server.close(() => resolve()));
}

test("approved Auth peers reject private bypasses, redirects and changed DNS before a request", async () => {
  const publicPeer: ApprovedAuthDestination = { url: "https://auth.example.test/hook", addresses: ["93.184.216.34"] };
  assert.equal(validateAuthDestination(publicPeer).pathname, "/hook");
  for (const change of [
    { url: "not a URL" }, { egressProxyURL: "invalid proxy?secret=value" },
    { url: "http://auth.example.test/hook" }, { url: "https://user:secret@auth.example.test/hook" },
    { url: "https://auth.example.test/hook#fragment" }, { url: "https://93.184.216.34/hook" },
    ...["127.0.0.1", "169.254.169.254", "10.0.0.1", "::1", "::ffff:127.0.0.1", "::ffff:7f00:1", "64:ff9b::a00:1", "2002:7f00:1::"].map(ip => ({ addresses: [ip] })),
    { egressProxyURL: "http://user:secret@127.0.0.1:8888" }, { addresses: [] },
  ]) assert.throws(() => validateAuthDestination({ ...publicPeer, ...change }));
  const changed = createAuthTransport(async () => [{ address: "169.254.169.254", family: 4 }]);
  await assert.rejects(changed.request(publicPeer, "POST", Buffer.from("{}"), {}, 100, 1024), /Approved Auth destination unavailable/);
});

test("Auth HTTP stays on LAN, bounds replies, never follows redirects and cancels late DNS", async () => {
  let calls = 0, pathSeen = "";
  const sockets = new Set<net.Socket>();
  const server = http.createServer((req, response) => {
    calls++; pathSeen = req.url!;
    if (req.url === "/redirect") response.writeHead(302, { Location: "/never-follow" }).end();
    else if (req.url === "/large") response.end("x".repeat(2048));
    else response.end('{"ok":true}');
  }); track(server, sockets); const port = await listen(server);
  const destination: ApprovedAuthDestination = { url: `http://localhost:${port}/hook`, addresses: ["127.0.0.1"], allowLabHTTP: true, egressProxyURL: "http://127.0.0.1:1" };
  const transport = createAuthTransport(async () => [{ address: "127.0.0.1", family: 4 }]);
  try {
    let result = await transport.request(destination, "POST", Buffer.from("{}"), { "Content-Type": "application/json" }, 1000, 1024);
    assert.equal(result.status, 200); assert.deepEqual(JSON.parse(result.body.toString()), { ok: true });
    result = await transport.request({ ...destination, url: `http://localhost:${port}/redirect` }, "GET", undefined, {}, 1000, 1024);
    assert.equal(result.status, 302); assert.equal(calls, 2); assert.equal(pathSeen, "/redirect");
    await assert.rejects(transport.request({ ...destination, url: `http://localhost:${port}/large` }, "GET", undefined, {}, 1000, 1024));
    const before = calls;
    const slow = createAuthTransport(async () => { await new Promise(resolve => setTimeout(resolve, 70)); return [{ address: "127.0.0.1", family: 4 }]; });
    await assert.rejects(slow.request(destination, "GET", undefined, {}, 10, 1024));
    await new Promise(resolve => setTimeout(resolve, 100));
    assert.equal(calls, before, "An expired DNS lookup started a later HTTP request");
  } finally { await close(server, sockets); }
});

test("Auth HTTPS verifies CA and hostname and only public peers use pinned CONNECT", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "neon-auth-egress-")); fs.chmodSync(dir, 0o700);
  const sockets = new Set<net.Socket>(), proxySockets = new Set<net.Socket>();
  let secure: https.Server | undefined, proxy: http.Server | undefined, delivered = 0;
  const authorities: string[] = [];
  try {
    const key = path.join(dir, "key.pem"), cert = path.join(dir, "ca.pem");
    const generated = spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=auth.example.test", "-addext", "subjectAltName=DNS:auth.example.test", "-keyout", key, "-out", cert], { timeout: 15000, stdio: "pipe" });
    assert.equal(generated.status, 0); const caPEM = fs.readFileSync(cert, "utf8");
    secure = https.createServer({ key: fs.readFileSync(key), cert: caPEM }, (_req, response) => { delivered++; response.end('{"accepted":true}'); });
    secure.on("tlsClientError", () => {}); track(secure, sockets); const port = await listen(secure);
    proxy = http.createServer(); track(proxy, proxySockets);
    proxy.on("connect", (request, client, head) => {
      authorities.push(request.url!);
      const upstream = net.connect({ host: "127.0.0.1", port }, () => {
        client.write("HTTP/1.1 200 Connection Established\r\n\r\n");
        if (head.length) upstream.write(head);
        upstream.pipe(client); client.pipe(upstream);
      }); proxySockets.add(upstream); upstream.on("error", () => client.destroy()); upstream.on("close", () => proxySockets.delete(upstream));
    }); const proxyPort = await listen(proxy);
    const lan: ApprovedAuthDestination = { url: `https://auth.example.test:${port}/hook`, addresses: ["127.0.0.1"], allowLabHTTP: true, caPEM, egressProxyURL: `http://127.0.0.1:${proxyPort}` };
    const localTransport = createAuthTransport(async () => [{ address: "127.0.0.1", family: 4 }]);
    assert.equal((await localTransport.request(lan, "POST", Buffer.from("{}"), {}, 2000, 1024)).status, 200);
    assert.equal(authorities.length, 0, "Internal HTTPS went through the external proxy");
    await assert.rejects(localTransport.request({ ...lan, url: `https://wrong.example.test:${port}/hook` }, "GET", undefined, {}, 2000, 1024));
    await assert.rejects(localTransport.request({ ...lan, caPEM: undefined }, "GET", undefined, {}, 2000, 1024));
    assert.equal(delivered, 1);
    const wan: ApprovedAuthDestination = { ...lan, allowLabHTTP: false, addresses: ["93.184.216.34"] };
    const remoteTransport = createAuthTransport(async () => [{ address: "93.184.216.34", family: 4 }]);
    assert.equal((await remoteTransport.request(wan, "POST", Buffer.from("{}"), {}, 2000, 1024)).status, 200);
    assert.deepEqual(authorities, ["93.184.216.34:" + port]); assert.equal(delivered, 2);
  } finally {
    if (proxy) await close(proxy, proxySockets);
    if (secure) await close(secure, sockets);
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
