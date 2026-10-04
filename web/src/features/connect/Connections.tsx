import { useEffect, useState } from 'react';
import { api, endpointPath } from '../../api';
import type { ConnectionInfo, Endpoint } from '../../api';
import { PageHeading } from '../../shared/ui';
import { useEndpointSelection } from '../../shared/useEndpointSelection';

export function Connections({
  projectId,
  endpoints,
  branchName,
  showError,
}: {
  projectId: string;
  endpoints: Endpoint[];
  branchName: (id: string) => string;
  showError: (error: unknown) => void;
}) {
  const [selected, setSelected] = useEndpointSelection(projectId, endpoints);
  const [info, setInfo] = useState<ConnectionInfo | null>(null);
  const endpoint = endpoints.find((e) => e.id === selected) || endpoints[0];
  useEffect(() => {
    const controller = new AbortController();
    setInfo(null);
    if (endpoint)
      api<ConnectionInfo>(endpointPath(projectId, endpoint.id) + '/connection-info', {
        signal: controller.signal,
      })
        .then((data) => {
          if (!controller.signal.aborted) setInfo(data);
        })
        .catch((error) => {
          if (!controller.signal.aborted) showError(error);
        });
    return () => controller.abort();
  }, [endpoint?.id]);
  return (
    <>
      <PageHeading
        kicker="PROJECT / CONNECT"
        title="连接数据库"
        description="所有应用连接都经过 Neon Proxy，不直接访问 Compute。"
      />
      <div className="selector-row">
        <label>
          选择 Endpoint
          <select value={endpoint?.id || ''} onChange={(e) => setSelected(e.target.value)}>
            {endpoints.map((e) => (
              <option key={e.id} value={e.id}>
                {branchName(e.branch_id)} · {e.endpoint_type === 'read_only' ? '只读' : '读写'} ·{' '}
                {e.selector}
              </option>
            ))}
          </select>
        </label>
      </div>
      {info && (
        <>
          <div className="panel pad connection-panel">
            <div className="panel-heading">
              <h2>连接字符串模板</h2>
              <span className="tag">不包含密码</span>
            </div>
            <p>使用创建该数据库时保存的角色密码替换占位符。</p>
            <div className="code-box">
              <code>{info.connection_uri_template}</code>
              <button
                className="button"
                onClick={() => navigator.clipboard.writeText(info.connection_uri_template)}
              >
                复制
              </button>
            </div>
            <div className="source-grid">
              <div>
                <small>Host</small>
                <strong>
                  {info.host}:{info.port}
                </strong>
              </div>
              <div>
                <small>Role / Database</small>
                <strong>
                  {info.role} / {info.database}
                </strong>
              </div>
              <div>
                <small>Endpoint</small>
                <strong>{info.endpoint_selector}</strong>
              </div>
              <div>
                <small>TLS</small>
                <strong>{info.ssl_mode}</strong>
              </div>
            </div>
          </div>
          <div className="notice">
            <span>◇</span>
            <div>
              <strong>当前为实验室证书</strong>
              <p>生产域名上线前必须配置受信任证书并使用 verify-full；连接池入口尚未部署和验收。</p>
            </div>
          </div>
        </>
      )}
    </>
  );
}
