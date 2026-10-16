import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  ASSISTANT_SCHEMA_SQL,
  assistantSchemaSnapshot,
  assistantPlanSummary,
  assistantResponseText,
  buildAssistantRequest,
  parseSQLDraft,
  type AssistantInput,
} from '../src/features/sql/sqlAssistant.ts';

test('schema snapshot contains only bounded public role-filtered column metadata', () => {
  assert(ASSISTANT_SCHEMA_SQL.includes('information_schema.columns'));
  assert(ASSISTANT_SCHEMA_SQL.includes("table_schema='public'"));
  assert(!ASSISTANT_SCHEMA_SQL.includes('column_default'));
  const columns = [
    'table_schema',
    'table_name',
    'column_name',
    'data_type',
    'is_nullable',
    'udt_name',
  ];
  const rows = Array.from({ length: 61 }, (_, i) => [
    'public',
    't',
    `c${i}`,
    'integer',
    'YES',
    'int4',
  ]);
  const value = JSON.parse(assistantSchemaSnapshot(columns, rows, false));
  assert.equal(value.source, 'role_filtered_information_schema');
  assert.equal(value.schema, 'public');
  assert.equal(value.truncated, true);
  assert(value.columns.length > 0 && value.columns.length <= 60);
  assert.equal(value.columns[0].nullable, true);
  assert(Buffer.byteLength(JSON.stringify(value)) <= 4096);
  assert.throws(() => assistantSchemaSnapshot([...columns, 'column_default'], rows, false));
  assert.throws(() =>
    assistantSchemaSnapshot(columns, [['private', 't', 'c', 'text', 'YES', 'text']], false),
  );
  assert.throws(() =>
    assistantSchemaSnapshot(columns, [['public', 't', null, 'text', 'YES', 'text']], false),
  );
});

const model = {
  id: 'local-fixture',
  name: 'Fixture',
  max_output_tokens: 128,
  protocols: ['chat_completions'],
};
const input: AssistantInput = {
  purpose: 'sql_draft',
  prompt: 'Write SELECT 42',
  schema: '',
  includeSQL: false,
  currentSQL: "SELECT 'secret literal'",
  plan: null,
};
test('assistant sends only explicit context, never opaque password/result properties', () => {
  const extra = {
    ...input,
    password: 'connection-password',
    database: 'private-db',
    role: 'private-role',
    result: [['private-row']],
  };
  const body = buildAssistantRequest(extra, model);
  const sent = JSON.stringify(body);
  for (const denied of [
    'connection-password',
    'private-db',
    'private-role',
    'private-row',
    'secret literal',
  ])
    assert(!sent.includes(denied));
  assert.equal(body.max_tokens, 128);
  assert.equal(body.stream, false);
  assert.equal(body.temperature, 0);
  assert.equal(body.messages[2].content, input.prompt);
  assert.equal(JSON.parse(body.messages[1].content).request, undefined);
  const selected = buildAssistantRequest(
    { ...input, includeSQL: true, schema: 'CREATE TABLE permitted(id integer)' },
    model,
  );
  assert(selected.messages[1].content.includes('secret literal'));
  assert(selected.messages[1].content.includes('permitted'));
  assert.throws(() => buildAssistantRequest({ ...input, prompt: '库'.repeat(700) }, model));
  assert.throws(() => buildAssistantRequest({ ...input, schema: '库'.repeat(1400) }, model));
  assert.throws(() => buildAssistantRequest({ ...input, prompt: 'bad\x00prompt' }, model));
});

test('plan context removes relation/index names, expressions, SQL and unknown fields', () => {
  const rows = [
    [
      [
        {
          Plan: {
            'Node Type': 'Seq Scan',
            'Relation Name': 'private_table',
            'Index Name': 'private_index',
            Filter: "password='secret'",
            Output: ['private_column'],
            'Total Cost': 8,
            'Plan Rows': 4,
            'Actual Rows': 3,
            'Actual Loops': 2,
            'Actual Total Time': 1,
          },
          'Query Text': 'private SQL',
          'Planning Time': 0.1,
          'Execution Time': 5,
        },
      ],
    ],
  ];
  const explain = assistantPlanSummary(rows, false);
  const text = JSON.stringify(explain);
  for (const denied of [
    'private_table',
    'private_index',
    'password',
    'secret',
    'private_column',
    'private SQL',
    'Actual Rows',
  ])
    assert(!text.includes(denied));
  assert.equal(explain.execution_ms, null);
  assert.equal(explain.nodes[0].metrics['Total Cost'], 8);
  const actual = assistantPlanSummary(rows, true);
  assert.equal(actual.execution_ms, 5);
  assert.equal(actual.nodes[0].metrics['Actual Loops'], 2);
  assert.throws(() => buildAssistantRequest({ ...input, purpose: 'plan_advice' }, model));
});

test('AI plan metadata stays bounded below the larger interactive tree limits', () => {
  const children = Array.from({ length: 199 }, (_, i) => ({
    'Node Type': 'Result',
    'Total Cost': i,
    'Plan Rows': i + 1,
  }));
  const value = assistantPlanSummary(
    [[[{ Plan: { 'Node Type': 'Append', Plans: children } }]]],
    true,
  );
  assert(value.nodes.length <= 20);
  assert(new TextEncoder().encode(JSON.stringify(value.nodes)).length <= 4096);
  assert.equal(value.truncated, true);
});

test('draft parsing accepts bounded JSON or one SQL block without claiming syntax safety', () => {
  const value = parseSQLDraft(
    JSON.stringify({
      sql: 'SELECT 42 AS value;',
      explanation: 'Returns a literal.',
      warnings: ['Review before execution.'],
    }),
  );
  assert.equal(value.sql, 'SELECT 42 AS value;');
  assert.equal(value.format, 'json');
  assert.equal(parseSQLDraft('```sql\nSELECT 1;\n```').sql, 'SELECT 1;');
  assert.equal(parseSQLDraft('SELECT 1;').format, 'sql');
  // A draft is explicitly not a parser/execution certificate. It is never run.
  assert.equal(parseSQLDraft('SELECT invalid_unverified_identifier;').format, 'sql');
  for (const bad of [
    '<script>alert(1)</script>',
    'Here is SQL: SELECT 1;',
    '```sql\nSELECT 1;\n```\nextra text',
    '{"sql":"SELECT 1"}',
    JSON.stringify({ sql: 'SELECT 1', explanation: '', warnings: [], execute: true }),
    'SELECT ' + 'x'.repeat(16384),
    '{"sql":"SELECT \\u0000","explanation":"x","warnings":[]}',
  ])
    assert.throws(() => parseSQLDraft(bad));
});

test('tool calls, extra choices, truncation and controls never become a loaded draft', () => {
  const good = {
    choices: [{ finish_reason: 'stop', message: { role: 'assistant', content: 'SELECT 1;' } }],
  };
  assert.equal(assistantResponseText(good), 'SELECT 1;');
  for (const value of [
    { choices: [{ finish_reason: 'length', message: { role: 'assistant', content: 'SELECT' } }] },
    {
      choices: [
        {
          finish_reason: 'stop',
          message: { role: 'assistant', content: 'SELECT 1', tool_calls: [{}] },
        },
      ],
    },
    { choices: [...good.choices, ...good.choices] },
    {
      choices: [{ finish_reason: 'stop', message: { role: 'assistant', content: 'bad\x00data' } }],
    },
  ])
    assert.throws(() => assistantResponseText(value));
});
