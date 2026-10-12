import assert from 'node:assert/strict';
import { test } from 'node:test';
import { resultCSV, resultJSON } from '../src/features/sql/resultExport.ts';

test('CSV preserves delimiters/newlines and prevents spreadsheet formulas in data and headers', () => {
  assert.equal(
    resultCSV({
      columns: ['=header', 'text'],
      rows: [
        [' \t=1+2', 'a,"b"\n中'],
        [-5, null],
      ],
      command: 'SELECT 2',
      truncated: false,
    }),
    '"\'=header","text"\r\n"\' \t=1+2","a,""b""\n中"\r\n"-5",""\r\n',
  );
});

test('JSON preserves duplicate columns, nulls, nested values, exact integers and truncation', () => {
  const result = {
    columns: ['same', 'same', ''],
    rows: [[null, { id: '9223372036854775807' }, '=1+2']],
    command: 'SELECT 205',
    truncated: true,
  };
  assert.deepEqual(JSON.parse(resultJSON(result)), { format: 'neon-console-result-v1', ...result });
});
