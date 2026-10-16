import assert from 'node:assert/strict';
import { test } from 'node:test';
import { buildAssistantRequest, type AssistantInput } from '../src/features/sql/sqlAssistant.ts';

const model = {
  id: 'local-small',
  name: 'Local',
  max_output_tokens: 256,
  protocols: ['chat_completions'],
};
const input: AssistantInput = {
  purpose: 'sql_fix',
  prompt: 'Correct the SELECT keyword.',
  schema: '',
  includeSQL: true,
  includeError: true,
  currentSQL: 'SELEC 43 AS ai_fix_value',
  plan: null,
  failure: { code: 'sql_failed', message: 'syntax error at or near "SELEC"' },
};

test('repair requires deliberate selection of both exact SQL and bounded failure', () => {
  for (const value of [
    { ...input, includeSQL: false },
    { ...input, includeError: false },
    { ...input, failure: null },
  ])
    assert.throws(() => buildAssistantRequest(value, model));
  const body = buildAssistantRequest(input, model),
    context = JSON.parse(body.messages[1].content);
  assert.equal(context.purpose, 'sql_fix');
  assert.equal(context.user_selected_sql, input.currentSQL);
  assert.deepEqual(context.user_selected_error, input.failure);
  assert.equal(body.messages[2].content, input.prompt);
  assert(body.messages[0].content.includes('Never execute SQL'));
  assert.equal(body.stream, false);
});

test('repair refuses oversized/control-bearing failures rather than silently truncating them', () => {
  for (const failure of [
    { code: 'sql_failed', message: '库'.repeat(342) },
    { code: 'raw\ncode', message: 'error' },
    { code: 'sql_failed', message: 'bad\x00error' },
    { code: 'sql_failed', message: ' ' },
  ])
    assert.throws(() => buildAssistantRequest({ ...input, failure }, model));
});

test('repair projects only code/message, excluding extra transport and credential fields', () => {
  const failure = {
    ...input.failure!,
    password: 'not-for-model',
    request_id: 'request-private',
    database: 'db-private',
  };
  const sent = JSON.stringify(buildAssistantRequest({ ...input, failure }, model));
  for (const value of ['not-for-model', 'request-private', 'db-private'])
    assert(!sent.includes(value));
  const ordinary = JSON.parse(
    buildAssistantRequest({ ...input, purpose: 'sql_draft', includeSQL: false }, model).messages[1]
      .content,
  );
  assert.equal(ordinary.user_selected_error, undefined);
  assert.equal(ordinary.user_selected_sql, undefined);
});
