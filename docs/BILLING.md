# 付款、原订单恢复与组织额度

这是默认关闭的自托管预付额度功能。组织管理员选择运营方已批准的固定 USD
方案，先创建原订单，再请求该订单的 Stripe 托管收银台。价格、额度、方案修订、
购买者和商户身份均随原订单保存。页面返回、创建订单或生成链接均不代表到账。

只实现固定金额的一次性预付额度。订阅续费、税务发票、汇率换算、优惠券及平台
主动发起商业退款不在此合同内。退款/争议由支付方原记录核验后同步；正式商户、
收款规则、受保护凭据及外部生产认证见 [运营输入](OPERATOR-INPUTS.md)。
本地合成回执和支付方测试模式不代表真实收款。

## 授权与启用

- 普通购买和历史读取要求当前组织管理员 Console 会话。控制 API key 不可购买。
- 平台核对要求配置中独立批准的 `operator_ids` 和组织范围；AI 供应商操作员、
  Bootstrap Owner 或服务计划操作员身份不自动授予付款核对权限。
- 组织额度账户须预先启用。付款不会自动开启账户，也不改写已消费和未结预留。
- Helm `billing.enabled: false` 默认关闭。启用须提供 `billing.existingSecret`，
  其中 `config.json` 只投射进 API；Worker、AI 供应商进程及浏览器不接触商户密钥。
  Chart 同时要求独立 Worker 和保留的 Backend 版本化加密密钥。

配置结构如下。所有占位符必须通过受保护入口提供；勿将实际凭据提交到 Git：

```json
{
  "version": 1,
  "operator_ids": ["approved_billing_operator"],
  "allowed_organizations": ["approved_organization"],
  "return_origin": "https://console.example.invalid",
  "provider": {
    "type": "stripe",
    "origin": "https://api.stripe.com",
    "checkout_origin": "https://checkout.stripe.com",
    "account_id": "acct_REPLACE",
    "api_version": "2024-06-20",
    "livemode": true,
    "private_fixture": false,
    "api_key": "<protected live credential>",
    "webhook_secrets": ["<protected endpoint signing credential>"]
  },
  "offers": [{
    "id": "offer_000000000000000000000001",
    "name": "Operator approved prepaid offer",
    "amount_minor": 125,
    "credit_micros": 1000000
  }]
}
```

示例金额仅用于说明单位，不是商业定价。每单 50～99,999,999 USD 分，
额度为 1～10¹² microUSD，方案名最多 80 个 Unicode 字符。API 派生不可变方案
修订；客户端不能提交自定金额、额度或修订值。配置上限 64 个方案、16 个操作员、
128 个组织和两把轮换中的 Webhook 签名密钥，配置文件最多 64 KiB。
变更配置后显式滚动 API；旧商户原订单不能绑定到另一商户。

非 Live 模式必须同时设置 `allow_non_live_credit_issuance: true` 和非空明确组织
白名单；正式模式禁止该标志。私有合成夹具还要求 `private_fixture: true`、
明确的私有字面 IP 和 `livemode: false`。此类内网请求直连，外网生产 Stripe
请求沿现有 WAN 代理环境。生产地址固定为上述 HTTPS origins，拒绝重定向。

## 原订单与未知结果

POST `/api/v1/organizations/{org}/billing/invoices` 提交 `offer_id` 和
`offer_revision`，返回 201 原订单；POST `.../{invoice}/checkout` 提交当前
`version`，返回 202 持久请求。两步都使用 `Idempotency-Key` 与当前 CSRF。
丢失响应应携原键恢复原请求，或读取原订单列表，不能通过换键创建替代付款。

后台在提交 `checkout_dispatched_at` 后只发起一次支付方创建请求；该标记之后的
未知结果不自动重发。Stripe 幂等键保留窗口不是长期恢复机制。只有已证明没有
发出的原订单才允许显式重新排队，原订单 ID 保持不变。

正常链接仅以用途隔离的版本化 AES-GCM 密文保存在数据库，AAD 绑定订单、
组织、商户、密钥版本和过期时间。GET `.../{invoice}/checkout` 再次检查当前
权限、商户、原订单及有效期，以 `Cache-Control: no-store` 返回原链接。
链接不能进入不可变幂等回执、普通日志或公开验收附件。

操作员 POST `/api/v1/platform/billing/organizations/{billingOrg}/invoices/{invoice}/reconcile`
提交原订单版本、`object_type`（`session`、`charge` 或 `dispute`）、原支付方
`object_id`、原因和 `evidence_sha256`。这只创建有来源的持久核对请求。
后台读取并验证原记录后，才能恢复同一收银台或同步付款/退款/争议。
人工断言不能直接发放额度，也不能用别的订单的记录替换原身份。

## 签名回执与原子额度

`POST /webhooks/billing/stripe` 在 JSON 解码前验证原始请求体的 Stripe v1 HMAC，
最多 1 MiB，时间偏差上限五分钟、未来上限 30 秒；支持两把签名密钥轮换。
收到事件后先保留最小类型化 inbox 及原始字节 SHA-256，再返回 202；同事件
重投返回 200，关键身份冲突返回 409。拒绝重复 JSON 键，忽略已签名的非支持事件。
不保存客户姓名、邮箱、卡号或支付方原始完整 JSON。

支持 Checkout completed/async success/expired、Charge refunded 和 Dispute
created/updated/closed。后台通过认证的 Account、Session、PaymentIntent、
Charge、Dispute GET 核对原商户、Live 模式、USD、金额、已捕获金额、支付状态、
原 ID 和隐藏订单绑定。退款先到达时只按原 PaymentIntent 有界查询唯一 Session。
支付方网络请求期间不持有数据库事务或组织额度锁。

每次核验把组织额度账户、原信用账本、不可变付款 effect、订单累计数和最终 inbox
在同一短事务中提交。重复/乱序回执不能重复发放；退款累计数只增加，争议终态
不被旧处理中状态覆盖。数据库约束要求 effect 总和与订单应得额度一致。

累计扣回为 `floor(原额度 × 有效累计负债 / 原付款分)`，使用整数精确计算，最终
全额扣回无舍入残差。退款与处理中/败诉争议的合计最多原付款；争议原始金额另存
最小核验记录，支持超过原付款的支付方争议金额。胜诉和 `prevented` 不扣争议额度。
退款可以使可用余额为负，已有用量和未完成预留保持原值，新 AI 准入继续由原额度
账本拒绝。供应商数据库角色无权插入或修改付款表及发放付款额度。

组织和平台的 `.../{invoice}/receipts` 返回最近 100 条最小核验状态；订单列表为
每页 100 条并返回下一游标。历史在撤回临时启用后仍保留，最终 applied/ignored
回执、价格快照、来源和账本不能被覆盖或删除。

协议依据：[Webhook 签名与重复事件](https://docs.stripe.com/webhooks)、
[原创建请求幂等窗口](https://docs.stripe.com/api/idempotent_requests)、
[Checkout 创建](https://docs.stripe.com/api/checkout/sessions/create)、
[按 PaymentIntent 查询 Session](https://docs.stripe.com/api/checkout/sessions/list)、
[Charge](https://docs.stripe.com/api/charges/object)、
[Dispute](https://docs.stripe.com/api/disputes/object)。实际功能回执与正式商户认证分别记录。
