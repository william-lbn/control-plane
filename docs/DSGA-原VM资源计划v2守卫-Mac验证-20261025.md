# 原VM资源计划v2与reservation守卫

新增前向迁移900000066，只替换原reservation守卫函数的VM格式约定，保留
全部锁序、键互斥、原计划不可变、凭据首次绑定、最终原资源图/步骤约束。
不修改已发布061、不UPDATE任何旧行、Operation、UID或来源。新INSERT要求VM2
及显式resource_limits对象；历史VM1保留原文和旧守卫语义，实际Go生产/恢复仍
要求完整v2，不能从当前env补历史预算或重建原意图。

数据库只核验已有结构约定；limits数值、总量、来源和真实硬约束仍由原Go/
Runner/Guest验证。SQL结构接受不是预算落实或VM执行许可。旧原迁移重放夹具
回退基础表时也须撤销依赖066标记，否则人为构造“旧守卫+新标记”的不可能
半回退快照；实际DDL/标记同事务提交。

Mac真实PG17 d426和Go race：13顶层/27含子项通过，0skip。覆盖全部受影响公开
项目入口、凭据/权限/配额/并发/原键恢复、新旧格式及直接SQL拒绝、旧行整行/
xmin不变。初轮仅旧回退夹具失败；修正依赖标记后只替换该1项，不重复其他12项。
两次产品源码相同，577输入摘要仅dsga_project_test.go夹具改变。readonly vet通过。
日志build/dsga-reservation-vm2-native/、dsga-reservation-vm2-forward-replacement/、
dsga-reservation-vm2-vet.log；原维护中两项失败亦保留，机器收据记录实际范围。

复验：使用独占临时PG、NEON_V2_TEST_DATABASE_URL及唯一schema；Go1.26.5
执行go test -race -count=1 ./internal/control -run
'^(TestDSGAProject.*|TestDSGAReservationVM2ForwardMigrationPreservesOriginalAndRejectsPartial|TestMigrationRegistryRejectsUnreviewedNumericCollisions)$'。
仅合成本机API夹具，不接用户数据库或K8s。Linux/物理/性能另验；完整伸缩缺失
代码仍需继续，默认关闭、不构建镜像、不部署、不合主分支。
