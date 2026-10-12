export type ExportableResult = {
  columns: string[];
  rows: unknown[][];
  command: string;
  truncated: boolean;
};

function csvCell(value: unknown): string {
  let text =
    value === null ? '' : typeof value === 'object' ? JSON.stringify(value) : String(value);
  // CSV is often opened in a spreadsheet. Quoting alone does not prevent
  // formula evaluation; prefix dangerous text, including hidden whitespace.
  // Numeric values stay numeric. JSON is available for exact original values.
  if (typeof value === 'string' && /^[\s\u0000-\u001f]*[=+@-]/u.test(text)) text = "'" + text;
  return '"' + text.replaceAll('"', '""') + '"';
}

export function resultCSV(result: ExportableResult): string {
  return (
    [result.columns, ...result.rows].map((row) => row.map(csvCell).join(',')).join('\r\n') + '\r\n'
  );
}

export function resultJSON(result: ExportableResult): string {
  // Positional rows preserve duplicate/empty column names, nulls and objects.
  // Do not convert rows into keyed objects or omit truncation metadata.
  return JSON.stringify({ format: 'neon-console-result-v1', ...result }, null, 2) + '\n';
}

export function downloadResult(result: ExportableResult, format: 'csv' | 'json'): void {
  const blob = new Blob([format === 'csv' ? '\ufeff' + resultCSV(result) : resultJSON(result)], {
    type: format === 'csv' ? 'text/csv;charset=utf-8' : 'application/json;charset=utf-8',
  });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = `query-result.${format}`;
  document.body.appendChild(link);
  link.click();
  link.remove();
  // Download navigation must acquire the blob before its URL is revoked.
  window.setTimeout(() => URL.revokeObjectURL(url), 30_000);
}
