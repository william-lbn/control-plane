import { downloadResult, type ExportableResult } from './resultExport';
import { downloadXLSX } from './resultXlsx';
import { useState } from 'react';

export function ResultExports({ result }: { result: ExportableResult }) {
  const [error, setError] = useState('');
  if (!result.columns.length) return null;
  return (
    <div className="query-actions" aria-label="结果导出">
      <span>
        {result.truncated ? '仅导出已显示的截断结果' : '导出当前结果'} · CSV 会转义公式，JSON
        保留原值；XLSX 以文本保留精度，不执行公式
      </span>
      <button className="button" onClick={() => downloadResult(result, 'csv')}>
        下载 CSV
      </button>
      <button className="button" onClick={() => downloadResult(result, 'json')}>
        下载 JSON
      </button>
      <button
        className="button"
        onClick={() => {
          setError('');
          try {
            downloadXLSX(result);
          } catch (error) {
            setError(error instanceof Error ? error.message : 'XLSX 导出失败，请使用 JSON。');
          }
        }}
      >
        下载 XLSX
      </button>
      {error && <span role="alert">{error}</span>}
    </div>
  );
}
