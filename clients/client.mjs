import http from 'node:http';
import https from 'node:https';
import { isIP } from 'node:net';
import { readFileSync, statSync, createReadStream, createWriteStream } from 'node:fs';
import { link, unlink } from 'node:fs/promises';
import { pipeline } from 'node:stream/promises';
import { Transform } from 'node:stream';
import { randomBytes } from 'node:crypto';
import { describe } from './catalog.mjs';

const JSON_LIMIT = 1024 * 1024, FILE_LIMIT = 64 * 1024 * 1024;
const managed = new Set(['authorization', 'cookie', 'host', 'origin', 'x-csrf-token', 'connection', 'content-length', 'transfer-encoding', 'proxy-authorization']);
const secretNames = /^(?:password|password_hash|client_secret|secret|token|access_token|refresh_token|session_token|csrf_token|authorization|cookie|private_key|probePassword|probe_password|api_key|credential_value|connection_string|connection_uri|dsn)$/i;
const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const text = value => typeof value === 'string' && !/[\x00-\x1f\x7f]/.test(value);

export function lanHost(host) {
  host = host.replace(/^\[|\]$/g, '').toLowerCase();
  if (host === 'localhost' || host === '::1' || host.endsWith('.svc') || host.endsWith('.cluster.local')) return true;
  if (isIP(host) === 4) {
    const [a, b] = host.split('.').map(Number);
    return a === 10 || a === 127 || a === 192 && b === 168 || a === 172 && b >= 16 && b <= 31 || a === 169 && b === 254;
  }
  return isIP(host) === 6 && /^(?:fc|fd|fe[89ab])/i.test(host);
}

function privateJSON(file) {
  const stat = statSync(file);
  if (!stat.isFile() || stat.size > 65536) throw new Error('Credential profile must be a bounded regular JSON file');
  const value = JSON.parse(readFileSync(file, 'utf8'));
  if (!object(value)) throw new Error('Invalid credential profile');
  return value;
}

export function loadProfile(env = process.env) {
  if (!env.NEON_CONTROL_PROFILE) throw new Error('NEON_CONTROL_PROFILE must name a protected credential profile');
  const value = privateJSON(env.NEON_CONTROL_PROFILE);
  const allowed = new Set(['version', 'origin', 'allow_http', 'allow_mutations', 'token', 'session_cookie', 'csrf_token', 'ca_file', 'allowed_operation_ids']);
  if (Object.keys(value).some(key => !allowed.has(key)) || value.version !== 1 || typeof value.origin !== 'string' ||
      value.allow_http !== undefined && typeof value.allow_http !== 'boolean' || value.allow_mutations !== undefined && typeof value.allow_mutations !== 'boolean') throw new Error('Invalid credential profile fields');
  const url = new URL(value.origin);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.pathname !== '/' || url.search || url.hash || url.origin !== value.origin.replace(/\/$/, '')) throw new Error('Use one fixed HTTP(S) origin without credentials or paths');
  if (url.protocol === 'http:' && !(value.allow_http === true && lanHost(url.hostname))) throw new Error('Plain HTTP requires explicit approval for a literal LAN/loopback endpoint');
  const token = value.token, cookie = value.session_cookie, csrf = value.csrf_token;
  if (Boolean(token) === Boolean(cookie) || token && (!text(token) || token.length < 8 || token.length > 8192) || cookie && (!text(cookie) || cookie.length < 8 || cookie.length > 16384 || !text(csrf) || csrf.length < 8 || csrf.length > 8192)) throw new Error('Use exactly one protected bearer token or Console session with CSRF');
  if (value.allowed_operation_ids !== undefined && (!Array.isArray(value.allowed_operation_ids) || value.allowed_operation_ids.length > 1000 || new Set(value.allowed_operation_ids).size !== value.allowed_operation_ids.length)) throw new Error('Invalid operation allowlist');
  if (value.allowed_operation_ids) for (const id of value.allowed_operation_ids) describe(id);
  return { ...value, origin: url.origin, allow_mutations: value.allow_mutations === true };
}

export function scrub(value, profile) {
  const secrets = [profile.token, profile.session_cookie, profile.csrf_token].filter(Boolean);
  function visit(item, depth = 0) {
    if (depth > 32) return '[REDACTED-NESTING-LIMIT]';
    if (typeof item === 'string') {
      for (const secret of secrets) item = item.replaceAll(secret, '[REDACTED-CONFIGURED-CREDENTIAL]');
      return item;
    }
    if (Array.isArray(item)) return item.map(v => visit(v, depth + 1));
    if (object(item)) return Object.fromEntries(Object.entries(item).map(([key, data]) => [key, secretNames.test(key) && data !== null ? '[REDACTED-SECRET]' : visit(data, depth + 1)]));
    return item;
  }
  return visit(value);
}

export class ControlClient {
  constructor(profile, env = process.env) {
    const [major, minor] = process.versions.node.split('.').map(Number);
    if (major !== 24 || minor < 5) throw new Error('Node.js 24.5 or later in the 24.x line is required for the verified proxy behavior');
    this.profile = profile;
    const direct = lanHost(new URL(profile.origin).hostname);
    this.http = new http.Agent({ keepAlive: false, proxyEnv: direct ? {} : env });
    this.https = new https.Agent({ keepAlive: false, proxyEnv: direct ? {} : env, ...(profile.ca_file ? { ca: readFileSync(profile.ca_file) } : {}) });
  }
  close() { this.http.destroy(); this.https.destroy(); }

  async execute(operationId, input = {}, options = {}) {
    const op = describe(operationId), profile = this.profile;
    if (!object(input) || Object.keys(input).some(key => !['path', 'query', 'headers', 'body', 'content_type'].includes(key))) throw new Error('Unknown request input; origin and credentials come only from the operator profile');
    if (profile.allowed_operation_ids && !profile.allowed_operation_ids.includes(operationId)) throw new Error('Operation is outside the configured allowlist');
    if (!op.read_only && !(profile.allow_mutations && options.allowWrite === true)) throw new Error('Mutations require both an approved profile and explicit write invocation');
    const params = input.path || {}, query = input.query || {}, extra = input.headers || {};
    if (![params, query, extra].every(object)) throw new Error('Path, query and headers must be objects');
    let path = op.path;
    const allowedPath = op.parameters.filter(p => p.in === 'path').map(p => p.name);
    if (Object.keys(params).some(key => !allowedPath.includes(key))) throw new Error('Unknown path parameter');
    for (const name of allowedPath) {
      const value = params[name];
      if (!text(value) || value.length === 0 || value.length > 2048 || ['.', '..'].includes(value)) throw new Error('Missing or invalid path parameter: ' + name);
      path = path.replaceAll('{' + name + '}', encodeURIComponent(value));
    }
    if (/[{}]/.test(path)) throw new Error('Unresolved path parameter');
    const url = new URL(profile.origin + path);
    if (url.origin !== profile.origin || url.pathname !== path) throw new Error('Path normalization changed the fixed operation target');
    for (const [name, value] of Object.entries(query)) {
      if (!op.parameters.some(p => p.in === 'query' && p.name === name)) throw new Error('Unknown query parameter: ' + name);
      for (const item of Array.isArray(value) ? value : [value]) {
        if (!['string','number','boolean'].includes(typeof item) || !text(String(item)) || String(item).length > 8192) throw new Error('Invalid query value');
        url.searchParams.append(name, String(item));
      }
    }
    const headers = { Accept: 'application/json', Origin: profile.origin };
    if (profile.token) headers.Authorization = 'Bearer ' + profile.token;
    else { headers.Cookie = profile.session_cookie; headers['X-CSRF-Token'] = profile.csrf_token; }
    for (const [name, value] of Object.entries(extra)) {
      if (managed.has(name.toLowerCase()) || !op.parameters.some(p => p.in === 'header' && p.name.toLowerCase() === name.toLowerCase()) || !text(value) || value.length > 8192) throw new Error('Header is not an allowed operation parameter');
      headers[name] = value;
    }
    for (const parameter of op.parameters.filter(p => p.required)) {
      if (parameter.in === 'query' && query[parameter.name] === undefined) throw new Error('Missing required query parameter: ' + parameter.name);
      if (parameter.in === 'header' && !managed.has(parameter.name.toLowerCase()) && !Object.keys(headers).some(name => name.toLowerCase() === parameter.name.toLowerCase())) throw new Error('Missing required header: ' + parameter.name);
    }
    let body;
    const content = op.request_body?.content || {};
    const contentType = input.content_type || (content['application/json'] ? 'application/json' : undefined);
    if (input.body !== undefined && options.bodyFile) throw new Error('Choose JSON body or a CLI body file');
    if (input.body !== undefined || options.bodyFile) {
      if (op.read_only || !contentType || !Object.hasOwn(content, contentType)) throw new Error('Body media type is not supported by this operation');
      headers['Content-Type'] = contentType;
      if (options.bodyFile) {
        const stat = statSync(options.bodyFile);
        if (!stat.isFile() || stat.size > FILE_LIMIT) throw new Error('CLI input file exceeds the fixed 64 MiB contract');
        headers['Content-Length'] = stat.size; body = createReadStream(options.bodyFile);
      } else {
        if (contentType !== 'application/json') throw new Error('MCP/JSON invocation requires a JSON request media type; use a CLI body file for bytes');
        body = Buffer.from(JSON.stringify(input.body));
        if (body.length > JSON_LIMIT) throw new Error('JSON request exceeds 1 MiB');
        headers['Content-Length'] = body.length;
      }
    } else if (op.request_body?.required) throw new Error('Request body is required');
    const transport = url.protocol === 'https:' ? https : http;
    const response = await new Promise((resolve, reject) => {
      const request = transport.request(url, { method: op.method, headers, agent: url.protocol === 'https:' ? this.https : this.http, signal: options.signal }, resolve);
      const timer = setTimeout(() => request.destroy(new Error('Fixed-origin API deadline exceeded; inspect the original request before retrying a mutation')), options.timeoutMs || 60000);
      timer.unref(); request.once('close', () => clearTimeout(timer));
      request.once('error', () => reject(new Error('API transport failed; mutation outcome may be unknown; no automatic retry was sent')));
      if (body?.pipe) pipeline(body, request).catch(() => request.destroy()); else request.end(body);
    });
    const status = response.statusCode;
    const metadata = { status, request_id: response.headers['x-request-id'] || null, content_type: response.headers['content-type'] || null,
      etag: response.headers.etag || null, retry_after: response.headers['retry-after'] || null,
      idempotency_replayed: response.headers['idempotency-replayed'] === 'true' };
    if (status >= 300 && status < 400) { response.destroy(); throw new Error('Redirect rejected; credentials remain bound to the configured origin'); }
    if (options.outputFile && status >= 200 && status < 300) {
      const partial = options.outputFile + '.partial-' + randomBytes(8).toString('hex');
      let size = 0;
      const bound = new Transform({ transform(chunk, _encoding, callback) { size += chunk.length; callback(size > FILE_LIMIT ? new Error('CLI response exceeds 64 MiB') : null, chunk); } });
      try { await pipeline(response, bound, createWriteStream(partial, { flags: 'wx', mode: 0o600 })); await link(partial, options.outputFile); await unlink(partial); }
      catch { throw new Error('Download was not published; inspect its owned partial file and existing output path'); }
      return { ...metadata, output_file: options.outputFile, bytes: size };
    }
    const parts = []; let size = 0;
    for await (const chunk of response) {
      size += chunk.length;
      if (size > JSON_LIMIT) { response.destroy(); throw new Error('Response exceeds 1 MiB; use CLI --output-file'); }
      parts.push(chunk);
    }
    const bytes = Buffer.concat(parts);
    let value = null;
    if (bytes.length && metadata.content_type?.toLowerCase().includes('json')) {
      try { value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes)); }
      catch { throw new Error('API returned malformed JSON'); }
    } else if (bytes.length) {
      const containsCredential = [profile.token, profile.session_cookie, profile.csrf_token].filter(Boolean).some(secret => bytes.includes(Buffer.from(secret)));
      value = containsCredential ? { encoding: 'redacted', reason: 'Response contains a configured credential' } : { encoding: 'base64', data: bytes.toString('base64') };
    }
    return scrub({ ...metadata, body: value }, profile);
  }
}
