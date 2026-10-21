import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type ObjectIdentity = {
  kind: 'role' | 'database';
  name: string;
  oid: number;
  managed: boolean;
  protected: boolean;
  management_excluded?: boolean;
  intent_state?: string;
  intent_version?: string;
};
type Baseline = { binding_id: string; kind: 'role' | 'database'; expected_oid: number };
type Pending = { url: string; key: string; version: string; body: string };

export function CatalogCurrentObservation({
  path,
  items,
  objects,
  current,
  disabled,
  onBusy,
  onComplete,
}: {
  path: string;
  items: Baseline[];
  objects: ObjectIdentity[];
  current: Baseline[];
  disabled: boolean;
  onBusy: (busy: boolean) => void;
  onComplete: () => Promise<void>;
}) {
  const [selected, setSelected] = useState('');
  const [ack, setAck] = useState(false);
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState<Pending | null>(null);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const epoch = useRef(0);
  useEffect(
    () => () => {
      epoch.current++;
    },
    [path],
  );
  const choices = items.flatMap((baseline) => {
    const object = objects.find(
      (v) =>
        v.kind === baseline.kind &&
        v.oid === baseline.expected_oid &&
        v.managed &&
        !v.protected &&
        !v.management_excluded &&
        v.intent_state === 'ready' &&
        v.intent_version,
    );
    return object &&
      !current.some((v) => v.kind === baseline.kind && v.expected_oid === baseline.expected_oid)
      ? [{ baseline, object }]
      : [];
  });
  const choice = choices.find((v) => v.baseline.binding_id === selected);
  async function observe() {
    if (!pending && (!choice || !ack)) return;
    const request = pending || {
      url: `${path}/catalog-identities/${choice!.baseline.binding_id}/observe-current`,
      key: newRequestKey(),
      version: choice!.object.intent_version!,
      body: JSON.stringify({ name: choice!.object.name, acknowledge_current_observation: true }),
    };
    const generation = epoch.current;
    setPending(request);
    setBusy(true);
    onBusy(true);
    setError('');
    setMessage('');
    try {
      const receipt = await api<{ binding_id: string }>(request.url, {
        method: 'POST',
        headers: { 'Idempotency-Key': request.key, 'If-Match': `"${request.version}"` },
        body: request.body,
      });
      if (generation !== epoch.current) return;
      setPending(null);
      setSelected('');
      setAck(false);
      setMessage(`当前身份已衔接 · ${receipt.binding_id}`);
      await onComplete();
    } catch (e) {
      if (generation === epoch.current)
        setError(e instanceof Error ? e.message : '当前身份观察失败，请保留原请求重试。');
    } finally {
      if (generation === epoch.current) {
        setBusy(false);
        onBusy(false);
      }
    }
  }
  return (
    <form
      aria-label="衔接当前身份"
      onSubmit={(e) => {
        e.preventDefault();
        void observe();
      }}
    >
      <h3>衔接当前身份</h3>
      <p>
        核对首次观察的原 OID 与当前管理代际。先衔接 Owner 角色，再衔接数据库；此次观察不修改 SQL
        目录，也不补写此前的密码或选项历史。
      </p>
      {error && <p role="alert">{error}</p>}
      {message && <p role="status">{message}</p>}
      <label>
        衔接当前身份对象
        <select
          aria-label="衔接当前身份对象"
          value={selected}
          disabled={disabled || busy || !!pending}
          onChange={(e) => {
            setSelected(e.target.value);
            setAck(false);
          }}
        >
          <option value="">选择已有首次观察且尚无持续身份的对象</option>
          {choices.map(({ baseline, object }) => (
            <option key={baseline.binding_id} value={baseline.binding_id}>
              {object.kind === 'role' ? '角色' : '数据库'} · {object.name} · OID {object.oid} · 代际{' '}
              {object.intent_version}
            </option>
          ))}
        </select>
      </label>
      <label className="checkbox-label">
        <input
          type="checkbox"
          checked={ack}
          disabled={disabled || busy || !!pending}
          onChange={(e) => setAck(e.target.checked)}
        />
        我确认按原 OID 观察当前身份；首次观察历史保持原样，历史密码和选项不作推断。
      </label>
      <button className="button" disabled={disabled || busy || (!pending && (!choice || !ack))}>
        {pending ? '按原请求重试衔接' : '确认衔接当前身份'}
      </button>
      {pending && (
        <button
          type="button"
          className="button"
          disabled={disabled || busy}
          onClick={async () => {
            const generation = epoch.current;
            setBusy(true);
            onBusy(true);
            setError('');
            try {
              await onComplete();
              if (generation === epoch.current) {
                setPending(null);
                setSelected('');
                setAck(false);
              }
            } catch (e) {
              if (generation === epoch.current)
                setError(e instanceof Error ? e.message : '读取失败');
            } finally {
              if (generation === epoch.current) {
                setBusy(false);
                onBusy(false);
              }
            }
          }}
        >
          读取状态并重新选择衔接
        </button>
      )}
    </form>
  );
}
