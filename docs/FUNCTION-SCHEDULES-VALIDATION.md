# 定时触发器验收索引

F02 本地基础闭环已通过；精确版本、原失败/纠正、真实 SQL 与 UI 收尾见
[2026-10-17 交付](DELIVERY-2026-10-17-FUNCTION-SCHEDULES.md)。
[合同与手工步骤](FUNCTION-SCHEDULES.md)供独立复测；[需求总账](REMAINING-REQUIREMENTS-2026-10-16.md)
保留其余开发/验收门槛。此结论不等于官网全部功能或生产准入。

主套件 `functions-schedules.spec.ts` 验证实际冷启动与 SQL；
`functions-runtime-recovery.spec.ts` 只证明原失败实例正常退休；
`functions-schedules-finalize.spec.ts` 凭原业务通过证据验证同一次调用的 UI 与退休，
不会重发客户请求。不能单独用恢复/收尾套件替代实际业务验证。

原终态错误不得改写，新一分钟与显式代码修正分别保留独立 ID/版本。
维护者原始证据与受保护输入保留在工作区，不随公共 Git 上传。
