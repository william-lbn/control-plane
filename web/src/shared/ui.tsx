import React from 'react';

export const fmt = (value: string | null | undefined) =>
  value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—';
export const short = (value: string) => (value.length > 19 ? value.slice(0, 19) + '…' : value);
export const stateLabel = (value: string) =>
  (
    ({
      ready: '就绪',
      active: '运行中',
      succeeded: '已完成',
      queued: '已排队',
      running: '执行中',
      failed: '失败',
      suspended: '已休眠',
      unknown: '未知',
      starting: '启动中',
      provisioning: '创建中',
    }) as Record<string, string>
  )[value] || value;
export const status = (value: string) => (
  <span className={`status status-${value}`}>● {stateLabel(value)}</span>
);

export function PageHeading({
  kicker,
  title,
  description,
  action,
}: {
  kicker: string;
  title: string;
  description: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="page-heading">
      <div>
        <div className="eyebrow">{kicker}</div>
        <h1>{title}</h1>
        <p>{description}</p>
      </div>
      {action}
    </div>
  );
}
export function Empty({ title, message }: { title: string; message: string }) {
  return (
    <div className="empty">
      <span>◇</span>
      <h3>{title}</h3>
      <p>{message}</p>
    </div>
  );
}
