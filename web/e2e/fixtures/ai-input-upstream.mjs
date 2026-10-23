// Synthetic native input service. It verifies exact fixture hashes and native
// content shapes; it is not a commercial model or a document-quality test.
import http from 'node:http';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
if (process.platform !== 'linux') throw Error('Linux isolated input fixture required');
const assets = JSON.parse(readFileSync('/run/fixture/assets.json', 'utf8'));
const expected = new Map(assets.map((v) => [v.sha256, v.content_type]));
const stats = {
  calls: { responses: 0, messages: 0, gemini: 0, chat_completions: 0 },
  rejected: 0,
  requests: [],
};
const reply = (res, status, value) => {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(value));
};
function inputs(body) {
  const found = [];
  function add(mime, base64) {
    if (typeof base64 !== 'string') throw Error('missing data');
    const bytes = Buffer.from(base64, 'base64');
    if (bytes.toString('base64') !== base64) throw Error('noncanonical data');
    const sha = createHash('sha256').update(bytes).digest('hex');
    if (expected.get(sha) !== mime) throw Error('input differs');
    found.push({ sha256: sha, content_type: mime, bytes: bytes.length });
  }
  function uri(value) {
    if (typeof value !== 'string') throw Error('remote input');
    const match = /^data:([^;,]+);base64,(.+)$/.exec(value);
    if (!match) throw Error('remote input');
    add(match[1], match[2]);
  }
  function walk(value) {
    if (Array.isArray(value)) {
      for (const v of value) walk(v);
      return;
    }
    if (!value || typeof value !== 'object') return;
    if (value.source?.type === 'base64') add(value.source.media_type, value.source.data);
    for (const [key, v] of Object.entries(value)) {
      if (key === 'file_data') uri(v);
      if (key === 'image_url') uri(typeof v === 'string' ? v : v.url);
      if (key === 'inlineData') add(v.mimeType, v.data);
      if (key === 'input_audio') add(v.format === 'mp3' ? 'audio/mpeg' : 'audio/wav', v.data);
      walk(v);
    }
  }
  walk(body);
  return found;
}
http
  .createServer(async (req, res) => {
    if (req.method === 'GET' && req.url === '/healthz')
      return reply(res, 200, { ok: true, synthetic: true });
    if (req.method === 'GET' && req.url === '/stats') return reply(res, 200, stats);
    const protocol = {
      '/v1/responses': 'responses',
      '/v1/messages': 'messages',
      '/v1beta/models/input-probe:generateContent': 'gemini',
      '/v1/chat/completions': 'chat_completions',
    }[req.url];
    if (req.method !== 'POST' || !protocol) return reply(res, 404, { error: 'fixture path' });
    let size = 0;
    const chunks = [];
    for await (const chunk of req) {
      size += chunk.length;
      if (size > 2 * 1024 * 1024) return reply(res, 413, { error: 'fixture size' });
      chunks.push(chunk);
    }
    try {
      const raw = Buffer.concat(chunks);
      if (
        raw.includes(Buffer.from('nif_')) ||
        req.headers.cookie ||
        req.headers['x-neon-console-ticket'] ||
        req.headers.authorization
      )
        throw Error('private caller material escaped');
      const id = req.headers['x-client-request-id'];
      if (!/^air_[a-f0-9]{24}$/.test(id || '')) throw Error('request identity missing');
      const body = JSON.parse(raw);
      if (protocol !== 'gemini' && body.model !== 'input-probe') throw Error('model identity');
      const found = inputs(body);
      const kinds = found.map((v) => v.content_type).sort();
      const wanted = (
        protocol === 'chat_completions'
          ? ['audio/wav', 'image/png']
          : ['application/pdf', 'image/png']
      ).sort();
      if (JSON.stringify(kinds) !== JSON.stringify(wanted)) throw Error('native file set');
      if (protocol === 'responses' && (!Array.isArray(body.input) || body.input[0].role !== 'user'))
        throw Error('Responses messages');
      if (protocol === 'messages' && !Array.isArray(body.messages)) throw Error('Messages body');
      if (protocol === 'gemini' && !Array.isArray(body.contents)) throw Error('Gemini body');
      stats.calls[protocol]++;
      stats.requests.push({ id, protocol, inputs: found });
      if (protocol === 'responses')
        return reply(res, 200, {
          object: 'response',
          status: 'completed',
          output: [
            {
              type: 'message',
              content: [{ type: 'output_text', text: 'Synthetic input files accepted' }],
            },
          ],
          usage: { input_tokens: 3, output_tokens: 2, total_tokens: 5 },
        });
      if (protocol === 'messages')
        return reply(res, 200, {
          type: 'message',
          content: [{ type: 'text', text: 'Synthetic input files accepted' }],
          usage: { input_tokens: 3, output_tokens: 2 },
        });
      if (protocol === 'gemini')
        return reply(res, 200, {
          candidates: [
            {
              content: { parts: [{ text: 'Synthetic input files accepted' }] },
              finishReason: 'STOP',
            },
          ],
          usageMetadata: { promptTokenCount: 3, candidatesTokenCount: 2, totalTokenCount: 5 },
        });
      return reply(res, 200, {
        object: 'chat.completion',
        choices: [{ message: { role: 'assistant', content: 'Synthetic input files accepted' } }],
        usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
      });
    } catch {
      stats.rejected++;
      return reply(res, 422, { error: { message: 'Synthetic fixture refused the native input' } });
    }
  })
  .listen(9081, '0.0.0.0');
