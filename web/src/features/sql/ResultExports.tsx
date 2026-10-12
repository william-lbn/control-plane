import { downloadResult, type ExportableResult } from './resultExport';

export function ResultExports({ result }: { result: ExportableResult }) {
  if (!result.columns.length) return null;
  return (
    <div className="query-actions" aria-label="结果导出">
      <span>
        {result.truncated ? '仅导出已显示的截断结果' : '导出当前结果'} · CSV 会转义公式，JSON
        保留原值
      </span>
      <button className="button" onClick={() => downloadResult(result, 'csv')}>
        下载 CSV
      </button>
      <button className="button" onClick={() => downloadResult(result, 'json')}>
        下载 JSON
      </button>
    </div>
  );
}
