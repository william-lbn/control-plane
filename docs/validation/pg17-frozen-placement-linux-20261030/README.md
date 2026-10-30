# PG17冻结放置与Sysfs最小控制面修复：Linux验收

本轮只处理PG17创建时的运行渲染、原计划恢复兼容和所需前向数据库格式；不扩展独立控制面业务、CPU策略、RO/HA或迁移。

新VM profile v3把完整NodeSelector和3个hostname放在指针子结构内冻结，深拷贝map/slice，保留profile/plan的零值比较能力。v3固定Sysfs，完整NodeSelector与唯一required hostname In term按AND组合；输出排序只作用于副本。读取原计划和恢复不从当前环境/节点对象补授权，完整spec比较拒绝selector/hostname/额外OR term/CPU模式漂移。

历史v1保持只读解码，v2 ResourceLimits合同与原QMP/无placement投影保留。v2未绑定VM UID的prepareVM在任何Kube请求前拒绝；已绑定只能核对/恢复同UID原对象，不因新环境配置升级、重造缺失VM或放宽节点。创建Envelope/native ABI不改版：新增的是嵌套VM profile v3，原创建Envelope v1、native Bootstrap v2、resource Source v1不变。

067只替换必要的reservation格式守卫：新INSERT必须带v3/资源对象/显式placement及3个不重复字符串hostname；旧v1/v2仅沿原不可变draft绑定来源/结案。未编辑已发布066、不UPDATE历史payload/UID，前向安装后原row及xmin不变有真实PG证据。完整label/内存数值及来源仍由Go生产者验证，SQL格式成功不发放节点或预算许可。所有旧严格读者必须先升级，不能删placement或降version迁就旧程序。

## 采用测试与范围

最终采用17个不同root、57个subcase，全部`-race -count=1`通过，0失败/跳过；12个PG17-U-CP-PLACEMENT单元根，5个PG17-S-CP-PLACEMENT根使用真实隔离metadata PG17和TLS Kube peer。PG17-E实际集群/KVM/Guest/物理cap本批未执行。

首轮17根15通过、2个新增测试夹具失败：一个沿用短ep_test被生产endpoint格式拒绝；另一个造两条pending reservation复用ResourceName，被原唯一键拒绝。保留失败，只修夹具；另补public policy完整v2（合法Limits但没有placement）不能新接受的直接子case。第二轮仅跑这2根及改动的policy根，共3根/11子case通过。最终采用首轮14根与第二轮3根，不能描述为同一个纯HEAD快照全部执行。

`adopted-tests.json`保存逐根/子case采用来源，`contexts/`保存实际COPY后1026个输入文件的SHA256锁、精确命令、起止源码状态和BuildKit元数据。两轮只差3个已重跑测试文件及一条生产注释；反向还原注释后的hash与首轮完全相同，生产逻辑没有在通过后变更（context-difference.json）。

## 准确工具与环境

Go1.27.1 linux/amd64、CGO_ENABLED=1实际执行。go.mod的go1.26.0/toolchain go1.27.1以及go.sum逐字节等于冻结HEAD，未降级或换依赖。工具由显式代理下载验证，GOMODCACHE与Go build缓存保留；gofmt来自同一准确Go1.27.1，hash见toolchain.json。首次导出格式工具因未载入Docker CLI配置失败，后续环境重试/只读缓存工具均有独立记录，没有改业务检查来回避失败。

元数据库专用pg17_control_test，118回环55439，NEON_V2_TEST_SCHEMA=v2_migration_pg17_placement只作安全标记，既有夹具为每根建立独占随机schema。该DB的fsync=off仅加快隔离迁移语义测试，不能称持久/崩溃恢复认证。宿主内核6.8.0-147-generic；不是Guest6.12或KVM认证。

## 本次部署配置

placement-fragment.json仅列新增字段，不是完整可部署策略，也没有编造image/UID/内存资格。完整原可信policy还须提供原有镜像digest、资源上界、TLS/存储等来源。

本次明确限定worker-117/118/119；selector包括linux、amd64和neon.tech/pg17-memory-pool=worker-117-worker-118-worker-119，hostname允许集合完全相同。源代码结构接受受信显式3节点配置，没有默默默认节点；实际本次配置与部署必须保持上述集合。Node pool标签及全四节点快照仍由Helm preflight核验，不能凭Renderer成功自动收养节点或把ubuntu/172.24.79.116纳入。

主代理另行修复Autoscaling controller的required affinity OR合并绕过；本修复不取代该最终写者检查。没有执行集群操作、推送或合并main；最终同名功能分支提交与准确依赖绑定由主代理统一。

## 精确命令

首轮完整精确选择：

```sh
go test -mod=readonly -json -race -count=1 -p=2 -timeout=20m ./internal/control -run '^(TestDSGAVMTemplateFreezesOfficialRuntimeAndTLS|TestDSGAVMTemplateRejectsUnfrozenAndCRDIncompatibleInputs|TestDSGAVMRecoveryRejectsStartupDriftAndAllowsOnlyRuntimeUse|TestDSGAVMProductionBindingResumesUnknownConfigAndNeverRecreatesBoundUID|TestDSGAResourceRendererOriginalPairAndRunnerContract|TestDSGAResourceRendererRejectsPartialAndHistoricalBudget|TestDSGAResourcePolicySnapshotOwnsLimitsAndPools|TestDSGAResourcePublicPolicyRequiresV3PlacementBeforeReservation|TestDSGAFiniteTaskLimitSurvivesFrozenPlanAndRunner|TestDSGASystemChargedDomainSurvivesOriginalPlanAndRunner|TestDSGAVM3FreezesSysfsPlacementAndRejectsRuntimeDrift|TestDSGAVM3SnapshotOwnsPlacementAndHistoricalVersionsStayFrozen|TestDSGAVM3RejectsIncompletePlacementBeforeProducingTemplate|TestDSGAVM2UnboundRecoveryNeverPostsAndBoundRecoveryNeverAdoptsPlacement|TestDSGAReservationVM3ForwardMigrationPreservesV1V2AndRequiresPlacement|TestDSGAReservationVM2ForwardMigrationPreservesOriginalAndRejectsPartial|TestDSGAProjectPublicAcceptanceAndImmutableReplay)$'
```

原始日志与退出码：`/data/neon-build/validation/pg17-control-placement-20261030t132340z`。

第二轮仅失败/改动根：

```sh
go test -mod=readonly -json -race -count=1 -p=2 -timeout=20m ./internal/control -run '^(TestDSGAResourcePublicPolicyRequiresV3PlacementBeforeReservation|TestDSGAVM3SnapshotOwnsPlacementAndHistoricalVersionsStayFrozen|TestDSGAReservationVM3ForwardMigrationPreservesV1V2AndRequiresPlacement)$'
```

原始日志与退出码：`/data/neon-build/validation/pg17-control-placement-20261030t132907z`。
