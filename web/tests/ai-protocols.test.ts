import assert from 'node:assert/strict';
import test from 'node:test';
import {
  playgroundRequest,
  protocolAnswer,
  protocolDelta,
  usdToMicros,
} from '../src/features/ai/protocols.ts';

test('native Playground emits vendor payloads and caps output without converting protocols', () => {
  assert.deepEqual(playgroundRequest('responses', 'alias', 'input', 64, true), {
    model: 'alias',
    input: 'input',
    max_output_tokens: 64,
    stream: true,
    store: false,
  });
  assert.deepEqual(playgroundRequest('embeddings', 'alias', 'input', 64, true), {
    model: 'alias',
    input: 'input',
    encoding_format: 'float',
  });
  assert.deepEqual(playgroundRequest('gemini', 'alias', 'input', 256, true), {
    model: 'alias',
    contents: [{ role: 'user', parts: [{ text: 'input' }] }],
    generationConfig: { maxOutputTokens: 128 },
  });
  assert.equal(
    (playgroundRequest('messages', 'alias', 'input', 64, true) as { max_tokens: number })
      .max_tokens,
    64,
  );
});

test('USD budget input converts exactly to integer micro-units', () => {
  assert.equal(usdToMicros('0.000001'), 1);
  assert.equal(usdToMicros('0.1'), 100000);
  assert.equal(usdToMicros('1000000.000000'), 1000000000000);
  for (const bad of ['-1', '1e3', '0.0000001', '1000000.000001', 'NaN', '1.'])
    assert.throws(() => usdToMicros(bad));
});

test('native output displays function calls and embeddings without invoking tools', () => {
  assert.match(
    protocolAnswer('responses', {
      output: [{ type: 'function_call', name: 'weather', arguments: '{}' }],
    }),
    /weather/,
  );
  assert.match(
    protocolAnswer('messages', { content: [{ type: 'tool_use', name: 'weather' }] }),
    /tool_use/,
  );
  assert.equal(
    protocolAnswer('gemini', { candidates: [{ content: { parts: [{ text: 'hello' }] } }] }),
    'hello',
  );
  assert.match(protocolAnswer('embeddings', { data: [{ embedding: [0.1, 0.2] }] }), /0.2/);
});

test('native stream terminal events and incomplete outcomes remain distinct', () => {
  assert.deepEqual(
    protocolDelta('responses', { type: 'response.function_call_arguments.delta', delta: '{}' }),
    { text: '{}', done: false },
  );
  assert.equal(protocolDelta('responses', { type: 'response.completed' }).done, true);
  assert.equal(protocolDelta('messages', { type: 'message_stop' }).done, true);
  assert.deepEqual(
    protocolDelta('messages', { type: 'content_block_delta', delta: { partial_json: '{}' } }),
    { text: '{}', done: false },
  );
  assert.throws(() => protocolDelta('responses', { type: 'response.incomplete' }), /不会自动重试/);
});
