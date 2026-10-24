import http from "node:http";
import https from "node:https";
import tls from "node:tls";
import { lookup } from "node:dns/promises";
import { BlockList, isIP } from "node:net";

export interface ApprovedAuthDestination {
  url: string;
  addresses: string[];
  allowLabHTTP?: boolean;
  caPEM?: string;
  egressProxyURL?: string;
}
const blocked = new BlockList();
for (const [address, prefix] of [
  ["0.0.0.0", 8], ["10.0.0.0", 8], ["100.64.0.0", 10],
  ["127.0.0.0", 8], ["169.254.0.0", 16], ["172.16.0.0", 12],
  ["192.0.0.0", 24], ["192.0.2.0", 24], ["192.168.0.0", 16],
  ["198.18.0.0", 15], ["198.51.100.0", 24], ["203.0.113.0", 24],
  ["224.0.0.0", 3],
] as const) blocked.addSubnet(address, prefix, "ipv4");
blocked.addAddress("::", "ipv6");
blocked.addAddress("::1", "ipv6");
blocked.addSubnet("fc00::", 7, "ipv6");
blocked.addSubnet("fe80::", 10, "ipv6");
blocked.addSubnet("ff00::", 8, "ipv6");
blocked.addSubnet("2001:db8::", 32, "ipv6");

function normalize(address: string): string {
  const lower = address.toLowerCase();
  if (!lower.startsWith("::ffff:")) return lower;
  const suffix = lower.slice(7);
  if (isIP(suffix) === 4) return suffix;
  if (/^[0-9a-f]{1,4}:[0-9a-f]{1,4}$/.test(suffix)) {
    const [left, right] = suffix.split(":").map(value => parseInt(value, 16));
    return [left >> 8, left & 255, right >> 8, right & 255].join(".");
  }
  return lower;
}
function publicAddress(address: string): boolean {
  const value = normalize(address), family = isIP(value);
  if (family === 6 && (!/^[23][0-9a-f]{3}:/.test(value) || /^2002:|^2001:(?:0:|10:|20:|2:)/.test(value))) return false;
  return family !== 0 && !blocked.check(value, family === 4 ? "ipv4" : "ipv6");
}
const unavailable = () => new Error("Approved Auth destination unavailable");

// These settings come only from an immutable operator Secret. Branch requests
// select an approved alias; they cannot supply a peer, CA, proxy or address.
export function validateAuthDestination(value: ApprovedAuthDestination): URL {
  if (!value || typeof value !== "object" || Object.keys(value).some(k =>
    !["url", "addresses", "allowLabHTTP", "caPEM", "egressProxyURL"].includes(k)) ||
    typeof value.url !== "string" || value.url.length > 2048 ||
    !Array.isArray(value.addresses) || value.addresses.length < 1 || value.addresses.length > 8 ||
    value.addresses.some(ip => typeof ip !== "string" || isIP(ip) === 0) ||
    new Set(value.addresses.map(normalize)).size !== value.addresses.length ||
    (value.allowLabHTTP !== undefined && typeof value.allowLabHTTP !== "boolean") ||
    (value.caPEM !== undefined && (typeof value.caPEM !== "string" || value.caPEM.length > 65536 || !value.caPEM.includes("-----BEGIN CERTIFICATE-----"))))
    throw unavailable();
  let url: URL;
  try { url = new URL(value.url); } catch { throw unavailable(); }
  if (url.href !== value.url || url.username || url.password || url.search || url.hash ||
    !url.hostname || /[%\\*\s]/.test(url.hostname) ||
    (url.protocol !== "https:" && !(value.allowLabHTTP && url.protocol === "http:")) ||
    (!value.allowLabHTTP && (isIP(url.hostname.replace(/^\[|\]$/g, "")) !== 0 || value.addresses.some(ip => !publicAddress(ip)))))
    throw unavailable();
  if (value.egressProxyURL !== undefined) {
    if (typeof value.egressProxyURL !== "string" || value.egressProxyURL.length > 2048) throw unavailable();
    let proxy: URL;
    try { proxy = new URL(value.egressProxyURL); } catch { throw unavailable(); }
    if (proxy.protocol !== "http:" || proxy.username || proxy.password || proxy.pathname !== "/" ||
      proxy.search || proxy.hash || !isIP(proxy.hostname.replace(/^\[|\]$/g, "")) || !proxy.port)
      throw unavailable();
  }
  return url;
}

type Resolver = (hostname: string) => Promise<{ address: string; family: number }[]>;
export type AuthTransportReply = { status: number; body: Buffer };
export function createAuthTransport(resolve: Resolver = hostname => lookup(hostname, { all: true, verbatim: true })) {
  async function request(destination: ApprovedAuthDestination, method: "GET" | "POST",
    body: Buffer | undefined, headers: Record<string, string>, timeoutMS: number, maxBytes: number): Promise<AuthTransportReply> {
    const url = validateAuthDestination(destination);
    if (!Number.isInteger(timeoutMS) || timeoutMS < 1 || timeoutMS > 15000 ||
      !Number.isInteger(maxBytes) || maxBytes < 1 || maxBytes > 1 << 20 || (body && body.length > 1 << 20)) throw unavailable();
    const hostname = url.hostname.replace(/^\[|\]$/g, ""), pins = new Set(destination.addresses.map(normalize));
    let timer: ReturnType<typeof setTimeout> | undefined;
    let abort: (() => void) | undefined;
    let stopped = false;
    const timeout = new Promise<never>((_, reject) => { timer = setTimeout(() => { stopped = true; abort?.(); reject(unavailable()); }, timeoutMS); });
    const work = async () => {
      const resolved = isIP(hostname) ? [{ address: hostname, family: isIP(hostname) }] : await resolve(hostname);
      if (stopped) throw unavailable();
      if (!resolved.length || resolved.some(row => !pins.has(normalize(row.address)))) throw unavailable();
      const target = normalize(resolved[0].address), port = Number(url.port || (url.protocol === "https:" ? 443 : 80));
      const options: https.RequestOptions = {
        protocol: url.protocol, hostname, port, path: url.pathname, method,
        headers: { ...headers, Host: url.host, "Accept-Encoding": "identity", ...(body ? { "Content-Length": String(body.length) } : {}) },
        agent: false, lookup: (_hostname, opts, callback) => {
          const address = { address: target, family: isIP(target) };
          if (opts.all) callback(null, [address]); else callback(null, target, address.family);
        },
        rejectUnauthorized: true, servername: isIP(hostname) ? undefined : hostname,
        ...(destination.caPEM ? { ca: destination.caPEM } : {}),
      };
      // Only public egress uses the operator proxy. Private laboratory/cluster
      // peers always connect directly, even when an egress proxy is configured.
      if (destination.egressProxyURL && publicAddress(target)) {
        if (url.protocol !== "https:") throw unavailable();
        const proxy = new URL(destination.egressProxyURL), authority = (isIP(target) === 6 ? "[" + target + "]" : target) + ":" + port;
        const tunnel = await new Promise<import("node:net").Socket>((resolveTunnel, reject) => {
          const connect = http.request({ hostname: proxy.hostname.replace(/^\[|\]$/g, ""), port: Number(proxy.port), method: "CONNECT", path: authority, headers: { Host: authority }, agent: false });
          abort = () => connect.destroy();
          connect.once("connect", (response, socket, head) => {
            if (response.statusCode !== 200 || head.length) { socket.destroy(); reject(unavailable()); }
            else resolveTunnel(socket);
          });
          connect.once("error", () => reject(unavailable())); connect.end();
        });
        const agent = new https.Agent({ keepAlive: false, maxSockets: 1 });
        if (stopped) { tunnel.destroy(); throw unavailable(); }
        agent.createConnection = () => tls.connect({ socket: tunnel, servername: hostname,
          rejectUnauthorized: true, ...(destination.caPEM ? { ca: destination.caPEM } : {}) });
        options.agent = agent;
        abort = () => { tunnel.destroy(); agent.destroy(); };
      }
      return new Promise<AuthTransportReply>((resolveReply, reject) => {
        const sender = url.protocol === "https:" ? https : http;
        const previousAbort = abort;
        const outgoing = sender.request(options, response => {
          const chunks: Buffer[] = []; let length = 0;
          response.on("data", chunk => {
            length += chunk.length;
            if (length > maxBytes) { response.destroy(); outgoing.destroy(); reject(unavailable()); }
            else chunks.push(chunk);
          });
          response.once("error", () => reject(unavailable()));
          response.once("end", () => resolveReply({ status: response.statusCode || 502, body: Buffer.concat(chunks) }));
        });
        abort = () => { outgoing.destroy(); previousAbort?.(); };
        outgoing.once("error", () => reject(unavailable()));
        if (body) outgoing.end(body); else outgoing.end();
      });
    };
    try { return await Promise.race([work(), timeout]); }
    catch { abort?.(); throw unavailable(); }
    finally { stopped = true; if (timer) clearTimeout(timer); }
  }
  return { request };
}
