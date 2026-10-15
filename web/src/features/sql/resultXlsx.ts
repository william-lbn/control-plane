import { strToU8, zipSync } from 'fflate';
import type { ExportableResult } from './resultExport';

const sheetNS = 'http://schemas.openxmlformats.org/spreadsheetml/2006/main';
const relNS = 'http://schemas.openxmlformats.org/package/2006/relationships';
const officeNS = 'http://schemas.openxmlformats.org/officeDocument/2006/relationships';
const declaration = '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>';

// SpreadsheetML escapes XML-forbidden code units. Escape literal _xNNNN_
// prefixes first so a database string cannot turn itself into an Excel escape.
function text(value: string): string {
  let output = '';
  for (const char of value.replace(/_x(?=[0-9a-f]{4}_)/gi, '_x005F_x')) {
    const code = char.codePointAt(0)!;
    output +=
      (code < 32 && ![9, 10, 13].includes(code)) ||
      (code >= 0xd800 && code <= 0xdfff) ||
      code === 0xfffe ||
      code === 0xffff
        ? `_x${code.toString(16).toUpperCase().padStart(4, '0')}_`
        : char;
  }
  return output
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('\r', '&#13;');
}

function column(index: number): string {
  let name = '';
  for (let n = index + 1; n; n = Math.floor((n - 1) / 26))
    name = String.fromCharCode(65 + ((n - 1) % 26)) + name;
  return name;
}

export function resultXLSX(result: ExportableResult): Uint8Array<ArrayBuffer> {
  if (
    !result.columns.length ||
    result.columns.length > 16384 ||
    (result.rows.length + 1) * result.columns.length > 100000
  ) {
    throw new Error('XLSX 导出最多 100000 个单元格；请缩小查询结果。');
  }
  let bytes = 0;
  const sheet = (rows: unknown[][], freeze: boolean) => {
    const data = rows
      .map(
        (row, r) =>
          '<row r="' +
          (r + 1) +
          '">' +
          row
            .map((value, c) => {
              const ref = column(c) + (r + 1);
              if (value === null) return `<c r="${ref}"/>`;
              const raw = typeof value === 'object' ? JSON.stringify(value) : String(value);
              if (raw.length > 32767)
                throw new Error('XLSX 单元格超过 32767 字符；请使用 JSON 导出完整内容。');
              bytes += strToU8(raw).length;
              if (bytes > 8 * 1024 * 1024) throw new Error('XLSX 文本超过 8 MiB；请缩小查询结果。');
              // Every non-NULL value is explicit text, including numbers and strings
              // beginning with =/+/-/@. Excel must not execute SQL result content or
              // round PostgreSQL int8/numeric values into floating-point cells.
              return `<c r="${ref}" t="inlineStr"><is><t xml:space="preserve">${text(raw)}</t></is></c>`;
            })
            .join('') +
          '</row>',
      )
      .join('');
    const views = freeze
      ? '<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>'
      : '';
    return (
      declaration +
      `<worksheet xmlns="${sheetNS}">${views}<sheetData>${data}</sheetData></worksheet>`
    );
  };
  if (result.rows.some((row) => row.length !== result.columns.length))
    throw new Error('查询列数与行不一致，无法导出 XLSX。');
  const resultSheet = sheet([result.columns, ...result.rows], true);
  const metadata = sheet(
    [
      ['Format', 'neon-console-xlsx-v1'],
      ['Command', result.command],
      ['Exported data rows', String(result.rows.length)],
      ['Exported columns', String(result.columns.length)],
      ['Result truncated', result.truncated ? 'true' : 'false'],
      [
        'Cell types',
        'All non-NULL cells are text; NULL cells are blank. JSON export retains JSON types.',
      ],
      ['Scope', 'Only the currently displayed result; no additional SQL or network request.'],
    ],
    false,
  );
  const contentTypes =
    declaration +
    '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">' +
    '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>' +
    '<Default Extension="xml" ContentType="application/xml"/>' +
    '<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>' +
    [1, 2]
      .map(
        (n) =>
          `<Override PartName="/xl/worksheets/sheet${n}.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`,
      )
      .join('') +
    '</Types>';
  const workbook =
    declaration +
    `<workbook xmlns="${sheetNS}" xmlns:r="${officeNS}"><bookViews><workbookView/></bookViews><sheets><sheet name="Result" sheetId="1" r:id="rId1"/><sheet name="Export info" sheetId="2" r:id="rId2"/></sheets></workbook>`;
  return zipSync(
    {
      '[Content_Types].xml': strToU8(contentTypes),
      '_rels/.rels': strToU8(
        declaration +
          `<Relationships xmlns="${relNS}"><Relationship Id="rId1" Type="${officeNS}/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
      ),
      'xl/workbook.xml': strToU8(workbook),
      'xl/_rels/workbook.xml.rels': strToU8(
        declaration +
          `<Relationships xmlns="${relNS}">` +
          [1, 2]
            .map(
              (n) =>
                `<Relationship Id="rId${n}" Type="${officeNS}/worksheet" Target="worksheets/sheet${n}.xml"/>`,
            )
            .join('') +
          '</Relationships>',
      ),
      'xl/worksheets/sheet1.xml': strToU8(resultSheet),
      'xl/worksheets/sheet2.xml': strToU8(metadata),
    },
    { level: 1 },
  );
}

export function downloadXLSX(result: ExportableResult): void {
  const blob = new Blob([resultXLSX(result)], {
    type: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = 'query-result.xlsx';
  document.body.appendChild(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 30000);
}
