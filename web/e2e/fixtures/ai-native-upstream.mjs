// Deterministic protocol supplier for functional acceptance, never a model.
// It retains request IDs and synthetic usage only, without prompts or secrets.
import http from 'node:http';
if (process.platform !== 'linux')
  throw Error('This isolated fixture must run in its Linux test Pod');

const statements = new Map();
const counts = { calls: 0, protocols: {}, custom_tools: 0, inline_images: 0 };
const reply = (res, status, value) => {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(value));
};
const chunk = (res, event, value) =>
  res.write((event ? 'event: ' + event + '\n' : '') + 'data: ' + JSON.stringify(value) + '\n\n');
const goodLimit = (value) => Number.isInteger(value) && value >= 1 && value <= 64;

http
  .createServer(async (req, res) => {
    const target = new URL(req.url, 'http://fixture');
    if (req.method === 'GET' && target.pathname === '/healthz')
      return reply(res, 200, { ready: true, synthetic: true });
    if (req.method === 'GET' && target.pathname === '/stats') return reply(res, 200, counts);
    if (req.method === 'GET' && target.pathname === '/statement') {
      const value = statements.get(target.searchParams.get('request_id'));
      return reply(res, value ? 200 : 404, value || { error: 'not_found' });
    }
    if (req.method !== 'POST') return reply(res, 404, { error: 'fixture_endpoint' });
    // A route with no supplier key must never receive a caller's branch token.
    if (req.headers.authorization || req.headers['x-api-key'] || req.headers['x-goog-api-key'])
      return reply(res, 401, { error: 'caller_credential_forwarded' });
    const id = req.headers['x-client-request-id'];
    if (typeof id !== 'string' || !/^air_[a-f0-9]{24}$/.test(id))
      return reply(res, 422, { error: 'platform_request_id_required' });
    let raw = '';
    for await (const block of req) {
      raw += block;
      if (Buffer.byteLength(raw) > 32768) return reply(res, 413, { error: 'fixture_body_limit' });
    }
    let q;
    try {
      q = JSON.parse(raw);
    } catch {
      return reply(res, 400, { error: 'fixture_json' });
    }
    let protocol;
    if (target.pathname === '/v1/responses') protocol = 'responses';
    else if (target.pathname === '/v1/chat/completions') protocol = 'chat_completions';
    else if (target.pathname === '/v1/embeddings') protocol = 'embeddings';
    else if (target.pathname === '/v1/messages') protocol = 'messages';
    else if (
      target.pathname === '/v1beta/models/probe:generateContent' ||
      target.pathname === '/v1beta/models/probe:streamGenerateContent'
    )
      protocol = 'gemini';
    else return reply(res, 404, { error: 'fixture_path' });
    if (protocol === 'gemini') {
      if (
        q.model !== undefined ||
        !Array.isArray(q.contents) ||
        !goodLimit(q.generationConfig?.maxOutputTokens)
      )
        return reply(res, 422, { error: 'gemini_wire' });
    } else if (q.model !== 'probe') return reply(res, 422, { error: 'model_alias_not_resolved' });
    if (
      protocol === 'responses' &&
      (!goodLimit(q.max_output_tokens) || q.store !== false || q.background !== false)
    )
      return reply(res, 422, { error: 'responses_wire' });
    if (
      protocol === 'messages' &&
      (!goodLimit(q.max_tokens) || req.headers['anthropic-version'] !== '2023-06-01')
    )
      return reply(res, 422, { error: 'messages_wire' });
    if (protocol === 'chat_completions' && !goodLimit(q.max_tokens ?? q.max_completion_tokens))
      return reply(res, 422, { error: 'chat_wire' });
    counts.calls++;
    counts.protocols[protocol] = (counts.protocols[protocol] || 0) + 1;
    const tools = Array.isArray(q.tools) && q.tools.length > 0;
    if (tools) counts.custom_tools++;
    if (raw.includes('data:image/') || raw.includes('inlineData')) counts.inline_images++;
    const batch =
      protocol === 'embeddings' && Array.isArray(q.input) && typeof q.input[0] === 'string'
        ? q.input.length
        : 1;
    const usage = {
      prompt_tokens: protocol === 'embeddings' ? 3 * batch : 3,
      completion_tokens: protocol === 'embeddings' ? 0 : 2,
      total_tokens: protocol === 'embeddings' ? 3 * batch : 5,
    };
    statements.set(id, { request_id: id, synthetic: true, accepted: true, protocol, usage });
    const text = 'synthetic-native-reply';
    const uncertain = raw.includes('__uncertain__');
    let value;
    if (protocol === 'responses')
      value = {
        id: 'resp_' + id,
        object: 'response',
        model: 'probe',
        status: 'completed',
        error: null,
        output: tools
          ? [{ type: 'function_call', call_id: 'call_' + id, name: 'weather', arguments: '{}' }]
          : [
              {
                type: 'message',
                role: 'assistant',
                content: [{ type: 'output_text', text, annotations: [] }],
              },
            ],
        ...(uncertain ? {} : { usage: { input_tokens: 3, output_tokens: 2, total_tokens: 5 } }),
      };
    if (protocol === 'messages')
      value = {
        id: 'msg_' + id,
        type: 'message',
        role: 'assistant',
        model: 'probe',
        content: [{ type: 'text', text }],
        stop_reason: 'end_turn',
        usage: { input_tokens: 3, output_tokens: 2 },
      };
    if (protocol === 'gemini')
      value = {
        candidates: [
          { index: 0, content: { role: 'model', parts: [{ text }] }, finishReason: 'STOP' },
        ],
        usageMetadata: { promptTokenCount: 3, candidatesTokenCount: 2, totalTokenCount: 5 },
        modelVersion: 'probe',
      };
    if (protocol === 'embeddings')
      value = {
        object: 'list',
        model: 'probe',
        data: Array.from({ length: batch }, (_, index) => ({
          object: 'embedding',
          index,
          embedding: [0.25, 0.5],
        })),
        usage: { prompt_tokens: usage.prompt_tokens, total_tokens: usage.total_tokens },
      };
    if (protocol === 'chat_completions')
      value = {
        id: 'chat_' + id,
        object: 'chat.completion',
        model: 'probe',
        choices: [
          {
            index: 0,
            message: {
              role: 'assistant',
              content: tools ? null : text,
              ...(tools
                ? {
                    tool_calls: [
                      {
                        id: 'call_' + id,
                        type: 'function',
                        function: { name: 'weather', arguments: '{}' },
                      },
                    ],
                  }
                : {}),
            },
            finish_reason: tools ? 'tool_calls' : 'stop',
          },
        ],
        usage,
      };
    const streaming = Boolean(q.stream) || target.pathname.endsWith(':streamGenerateContent');
    if (!streaming) return reply(res, 200, value);
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' });
    if (protocol === 'responses') {
      chunk(res, 'response.output_text.delta', {
        type: 'response.output_text.delta',
        sequence_number: 1,
        delta: text,
      });
      chunk(res, 'response.completed', {
        type: 'response.completed',
        sequence_number: 2,
        response: value,
      });
    } else if (protocol === 'messages') {
      chunk(res, 'message_start', {
        type: 'message_start',
        message: { ...value, content: [], usage: { input_tokens: 3, output_tokens: 0 } },
      });
      chunk(res, 'content_block_delta', {
        type: 'content_block_delta',
        index: 0,
        delta: { type: 'text_delta', text },
      });
      chunk(res, 'message_delta', {
        type: 'message_delta',
        delta: { stop_reason: 'end_turn' },
        usage: { output_tokens: 2 },
      });
      chunk(res, 'message_stop', { type: 'message_stop' });
    } else if (protocol === 'gemini') chunk(res, '', value);
    else if (protocol === 'chat_completions') {
      chunk(res, '', {
        object: 'chat.completion.chunk',
        model: 'probe',
        choices: [
          {
            index: 0,
            delta: tools
              ? { tool_calls: [{ index: 0, function: { name: 'weather', arguments: '{}' } }] }
              : { content: text },
          },
        ],
      });
      chunk(res, '', { object: 'chat.completion.chunk', model: 'probe', choices: [], usage });
      res.write('data: [DONE]\n\n');
    }
    res.end();
  })
  .listen(Number(process.env.PORT || 9081), '0.0.0.0');
