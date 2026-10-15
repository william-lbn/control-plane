import assert from 'node:assert/strict';
import { test } from 'node:test';
import { unzipSync, strFromU8 } from 'fflate';
import { resultXLSX } from '../src/features/sql/resultXlsx.ts';

test('XLSX preserves SQL text, positional headers, nulls and explicit truncation without executable cells', () => {
  const files = unzipSync(
    resultXLSX({
      columns: ['same', 'same', ''],
      rows: [
        ['=HYPERLINK("http://invalid")', '9223372036854775807', null],
        ['_x0041_\0中😀\r\n<&>', true, { n: '0.12345678901234567890' }],
        ['', -5, '@SUM(1,2)'],
      ],
      command: 'SELECT 201',
      truncated: true,
    }),
  );
  assert.deepEqual(
    Object.keys(files).sort(),
    [
      '[Content_Types].xml',
      '_rels/.rels',
      'xl/workbook.xml',
      'xl/_rels/workbook.xml.rels',
      'xl/worksheets/sheet1.xml',
      'xl/worksheets/sheet2.xml',
    ].sort(),
  );
  const xml = strFromU8(files['xl/worksheets/sheet1.xml']);
  assert.equal((xml.match(/>same<\/t>/g) || []).length, 2);
  assert(xml.includes('<c r="C2"/>'));
  assert(xml.includes('=HYPERLINK("http://invalid")'));
  assert(xml.includes('9223372036854775807'));
  assert(xml.includes('_x005F_x0041__x0000_中😀&#13;\n&lt;&amp;&gt;'));
  assert(xml.includes('<c r="A4" t="inlineStr"><is><t xml:space="preserve"></t>'));
  assert(!xml.includes('<f>'));
  const meta = strFromU8(files['xl/worksheets/sheet2.xml']);
  assert(meta.includes('Result truncated'));
  assert(meta.includes('>true</t>'));
  for (const [name, data] of Object.entries(files)) {
    assert(!name.includes('vba'));
    assert(!strFromU8(data).includes('TargetMode="External"'));
  }
});

test('XLSX handles column names past Z and refuses silent value loss or unbounded export', () => {
  const columns = Array.from({ length: 27 }, (_, n) => String(n));
  const xml = strFromU8(
    unzipSync(resultXLSX({ columns, rows: [], command: 'SELECT 0', truncated: false }))[
      'xl/worksheets/sheet1.xml'
    ],
  );
  assert(xml.includes('r="AA1"'));
  const base = { columns: ['x'], rows: [], command: 'SELECT 0', truncated: false };
  assert.throws(() => resultXLSX({ ...base, rows: [['x'.repeat(32768)]] }), /32767/);
  assert.throws(() => resultXLSX({ ...base, rows: [[1, 2]] }), /列数/);
  assert.throws(
    () => resultXLSX({ ...base, rows: Array.from({ length: 100000 }, () => [1]) }),
    /100000/,
  );
});
