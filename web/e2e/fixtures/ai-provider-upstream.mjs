// Explicit synthetic model service for native discovery and publication checks.
// The disposable key is read only from a Linux Secret volume, never from source.
import http from 'node:http';
import { readFileSync } from 'node:fs';
import { timingSafeEqual } from 'node:crypto';
if (process.platform !== 'linux') throw Error('Linux isolated fixture required');
const secret = readFileSync(process.env.NEON_FIXTURE_KEY_FILE).toString().trim();
if (secret.length < 24) throw Error('Disposable fixture credential required');
const stats = {
  anthropic_metadata: 0,
  gemini_metadata: 0,
  anthropic_inference: 0,
  gemini_inference: 0,
  rejected: 0,
};
const reply = (res, status, value) => {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(value));
};
const valid = (value) =>
  typeof value === 'string' &&
  Buffer.byteLength(value) === Buffer.byteLength(secret) &&
  timingSafeEqual(Buffer.from(value), Buffer.from(secret));
http
  .createServer(async (req, res) => {
    const url = new URL(req.url, 'http://fixture');
    if (req.method === 'GET' && url.pathname === '/healthz')
      return reply(res, 200, { ready: true, synthetic: true });
    if (req.method === 'GET' && url.pathname === '/stats') return reply(res, 200, stats);
    const kind = req.headers['x-api-key']
      ? 'anthropic'
      : req.headers['x-goog-api-key']
        ? 'gemini'
        : '';
    if (
      !kind ||
      req.headers.authorization ||
      req.headers.cookie ||
      !valid(req.headers[kind === 'anthropic' ? 'x-api-key' : 'x-goog-api-key']) ||
      (kind === 'anthropic' && req.headers['anthropic-version'] !== '2023-06-01')
    ) {
      stats.rejected++;
      return reply(res, 401, { error: 'fixture_authentication' });
    }
    if (req.method === 'GET' && kind === 'anthropic' && url.pathname === '/v1/models') {
      stats.anthropic_metadata++;
      if (url.searchParams.get('after_id') === null)
        return reply(res, 200, {
          data: [{ id: 'claude-probe-1', type: 'model' }],
          has_more: true,
          last_id: 'claude-probe-1',
        });
      if (url.searchParams.get('after_id') === 'claude-probe-1')
        return reply(res, 200, {
          data: [{ id: 'claude-probe-2', type: 'model' }],
          has_more: false,
          last_id: 'claude-probe-2',
        });
    }
    if (req.method === 'GET' && kind === 'gemini' && url.pathname === '/v1beta/models') {
      stats.gemini_metadata++;
      if (url.searchParams.get('pageToken') === null)
        return reply(res, 200, {
          models: [{ name: 'models/gemini-probe-1' }],
          nextPageToken: 'opaque+/=&page2',
        });
      if (url.searchParams.get('pageToken') === 'opaque+/=&page2')
        return reply(res, 200, { models: [{ name: 'models/gemini-probe-2' }] });
    }
    if (
      req.method !== 'POST' ||
      !/^air_[a-f0-9]{24}$/.test(req.headers['x-client-request-id'] || '')
    )
      return reply(res, 404, { error: 'fixture_path' });
    let raw = '';
    for await (const part of req) {
      raw += part;
      if (Buffer.byteLength(raw) > 32768) return reply(res, 413, { error: 'fixture_limit' });
    }
    let q;
    try {
      q = JSON.parse(raw);
    } catch {
      return reply(res, 400, { error: 'fixture_json' });
    }
    if (
      kind === 'anthropic' &&
      url.pathname === '/v1/messages' &&
      q.model === 'claude-probe-2' &&
      !q.stream
    ) {
      stats.anthropic_inference++;
      return reply(res, 200, {
        id: 'msg_fixture',
        type: 'message',
        role: 'assistant',
        model: q.model,
        stop_reason: 'end_turn',
        content: [{ type: 'text', text: 'Synthetic Anthropic profile result' }],
        usage: { input_tokens: 3, output_tokens: 2 },
      });
    }
    if (
      kind === 'gemini' &&
      url.pathname === '/v1beta/models/gemini-probe-2:generateContent' &&
      Array.isArray(q.contents)
    ) {
      stats.gemini_inference++;
      return reply(res, 200, {
        candidates: [
          {
            content: { role: 'model', parts: [{ text: 'Synthetic Gemini profile result' }] },
            finishReason: 'STOP',
          },
        ],
        usageMetadata: { promptTokenCount: 3, candidatesTokenCount: 2, totalTokenCount: 5 },
      });
    }
    return reply(res, 422, { error: 'fixture_native_wire' });
  })
  .listen(9081, '0.0.0.0');
