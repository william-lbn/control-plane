import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  validHistoryName,
  parseHistoryName,
  buildHistoryNameRequest,
} from '../src/features/sql/historyName.ts';
import { responseJSON } from '../src/features/sql/aiResponse.ts';
const model = {
  id: 'model',
  name: 'Local',
  max_output_tokens: 128,
  protocols: ['chat_completions'],
};
test('personal names obey the Go single-line character and UTF-8 limits', () => {
  for (const value of ['', 'Literal result', '中文名称', '中'.repeat(80)])
    assert(validHistoryName(value));
  for (const value of [
    ' ',
    ' name',
    'name ',
    'bad\nname',
    'bad\x00name',
    'bad\u0085name',
    'bad\u2028name',
    'bad\u2029name',
    'x'.repeat(81),
    '😀'.repeat(61),
  ])
    assert(!validHistoryName(value));
  assert.equal(parseHistoryName('  Query purpose  '), 'Query purpose');
  assert.throws(() => parseHistoryName('two\nlines'));
  assert.throws(() => parseHistoryName(''));
});
test('AI naming uses complete selected historical SQL with an explicit bounded request', () => {
  const body = buildHistoryNameRequest('SELECT 43', false, model);
  assert.equal(body.max_tokens, 64);
  assert.equal(body.stream, false);
  assert.equal(JSON.parse(body.messages[1].content).reference_sql, 'SELECT 43');
  assert.throws(() => buildHistoryNameRequest('SELECT 43', true, model));
  assert.throws(() => buildHistoryNameRequest('x'.repeat(9217), false, model));
  assert.throws(() => buildHistoryNameRequest('SELECT \x00', false, model));
  assert.throws(() => buildHistoryNameRequest('\\'.repeat(9000), false, model));
});
test('shared AI response decoding rejects oversize, malformed and invalid UTF-8', async () => {
  assert.deepEqual(await responseJSON(new Response('{"ok":true}')), { ok: true });
  for (const response of [
    new Response('x'.repeat(65537)),
    new Response('{"a":'),
    new Response(new Uint8Array([0xff])),
    new Response(null),
  ])
    await assert.rejects(responseJSON(response));
});
