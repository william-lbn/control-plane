export const authEvents = [
  'send.otp',
  'send.magic_link',
  'user.before_create',
  'user.created',
  'phone_number.verified',
  'organization.invitation.created',
  'organization.invitation.accepted',
];
export type AuthIntegrationSpec = {
  oauth_providers?: string[];
  webhook_id?: string;
  webhook_events?: string[];
  webhook_timeout_seconds?: number;
  phone_number_enabled?: boolean;
  phone_number_expires_in?: number;
};
export type AuthIntegrations = {
  available: boolean;
  webhooks: { id: string }[];
  oauth_providers: { id: string; kind: string; callback_url: string }[];
};
export function AuthIntegrationPolicy({
  value,
  onChange,
  available,
  disabled,
}: {
  value: AuthIntegrationSpec;
  onChange: (value: AuthIntegrationSpec) => void;
  available?: AuthIntegrations;
  disabled: boolean;
}) {
  const providers = [
    ...new Set([
      ...(available?.oauth_providers || []).map((p) => p.id),
      ...(value.oauth_providers || []),
    ]),
  ];
  const hooks = [
    ...new Set([
      ...(available?.webhooks || []).map((p) => p.id),
      ...(value.webhook_id ? [value.webhook_id] : []),
    ]),
  ];
  return (
    <fieldset className="backend-wide" disabled={disabled} data-testid="auth-integration-policy">
      <legend>第三方登录、手机与事件</legend>
      {!available?.available && (
        <p className="muted">运维尚未配置此项目可用的外部身份与通知服务。</p>
      )}
      {providers.map((id) => (
        <label className="auth-mail-policy" key={id}>
          <input
            type="checkbox"
            aria-label={'启用 OAuth ' + id}
            checked={value.oauth_providers?.includes(id) || false}
            onChange={(e) =>
              onChange({
                ...value,
                oauth_providers: e.target.checked
                  ? [...(value.oauth_providers || []), id]
                  : (value.oauth_providers || []).filter((p) => p !== id),
              })
            }
          />
          {id} 登录{' '}
          <span className="subtle-note">
            {available?.oauth_providers.find((p) => p.id === id)?.callback_url ||
              '当前提供商已不可用'}
          </span>
        </label>
      ))}
      <label>
        通知服务
        <select
          aria-label="Auth 通知服务"
          value={value.webhook_id || ''}
          onChange={(e) =>
            onChange({
              ...value,
              webhook_id: e.target.value,
              webhook_events: e.target.value ? ['user.created'] : [],
              phone_number_enabled: false,
            })
          }
        >
          <option value="">不启用</option>
          {hooks.map((id) => (
            <option key={id} value={id}>
              {id}
            </option>
          ))}
        </select>
      </label>
      {value.webhook_id && (
        <>
          <div className="backend-form">
            {authEvents.map((event) => (
              <label className="auth-mail-policy" key={event}>
                <input
                  aria-label={'订阅 Auth 事件 ' + event}
                  type="checkbox"
                  checked={value.webhook_events?.includes(event) || false}
                  onChange={(e) =>
                    onChange({
                      ...value,
                      webhook_events: e.target.checked
                        ? [...(value.webhook_events || []), event]
                        : (value.webhook_events || []).filter((p) => p !== event),
                      ...(!e.target.checked && event === 'send.otp'
                        ? { phone_number_enabled: false }
                        : {}),
                    })
                  }
                />
                {event}
              </label>
            ))}
          </div>
          <label>
            通知超时秒数
            <input
              aria-label="Auth 通知超时秒数"
              type="number"
              min={1}
              max={10}
              required
              value={value.webhook_timeout_seconds || 5}
              onChange={(e) =>
                onChange({ ...value, webhook_timeout_seconds: Number(e.target.value) })
              }
            />
          </label>
          <p className="subtle-note">
            验证码、登录链接和注册校验需要接收端成功响应。其他事件在业务提交后发送；应用管理员可查看投递状态。
          </p>
          <label className="auth-mail-policy">
            <input
              aria-label="启用应用手机号登录"
              type="checkbox"
              checked={value.phone_number_enabled || false}
              disabled={!value.webhook_events?.includes('send.otp')}
              onChange={(e) => onChange({ ...value, phone_number_enabled: e.target.checked })}
            />
            允许已绑定手机的用户通过短信验证码登录
          </label>
          {value.phone_number_enabled && (
            <label>
              短信验证码有效秒数
              <input
                aria-label="短信验证码有效秒数"
                type="number"
                min={60}
                max={600}
                required
                value={value.phone_number_expires_in || 300}
                onChange={(e) =>
                  onChange({ ...value, phone_number_expires_in: Number(e.target.value) })
                }
              />
            </label>
          )}
        </>
      )}
    </fieldset>
  );
}
