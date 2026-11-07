package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const dsgaStartupPollInterval = 2 * time.Second

// 共同创建链只编排已有领域原语，不自行签发平台资格、调整VM或打开PG准入。
// 资格与自动策略仍由原Agent/Scheduler负责；它们未就绪时SQL探针不能通过。
// 每次恢复重新观察原来源，operation_steps不是跳过物理核验的缓存。
func (s *server) reconcileDSGAProject(ctx context.Context, binding *dsgaCreationBinding) error {
	p := binding.intent.createPayload
	if binding.action != "create_project_dsga_v1" || p.EndpointType != "read_write" ||
		p.ParentTimelineID != "" || p.RestoreSource != "" || p.CatalogPolicy != "" {
		return errDSGACreationIdentity
	}
	if _, err := binding.creationPlan(ctx); err != nil {
		return err
	}
	step := func(ordinal int, name string, fn func(context.Context) error) error {
		return s.createStep(ctx, binding.operationID, binding.workerID, ordinal, name, fn)
	}
	if err := step(0, "create_managed_tenant", binding.prepareTenant); err != nil {
		return err
	}
	if err := step(1, "create_timeline", binding.prepareTimeline); err != nil {
		return err
	}
	if err := step(2, "prepare_neonvm_dsga", func(ctx context.Context) error {
		if _, err := binding.prepareVM(ctx); err != nil {
			return err
		}
		return binding.prepareServices(ctx)
	}); err != nil {
		return err
	}
	if err := step(3, "wait_neonvm_running", binding.waitOriginalCompute); err != nil {
		return err
	}
	var initialization dsgaInitializationResponse
	if err := step(4, "confirm_native_initialization", func(ctx context.Context) error {
		var err error
		initialization, err = binding.waitOriginalInitialization(ctx)
		if err != nil {
			return err
		}
		return binding.recordInitialization(ctx, initialization)
	}); err != nil {
		return err
	}
	if err := step(5, "publish_proxy_route", func(ctx context.Context) error {
		// 使用本次刚读回的原生事实；不以历史观察行代替当前初始化。
		return binding.publishInitializedSQLRoute(ctx, initialization)
	}); err != nil {
		return err
	}
	if err := step(6, "sql_probe_via_proxy", binding.probeOriginalSQL); err != nil {
		return err
	}
	return step(7, "persist_ready_state", func(ctx context.Context) error {
		fresh, err := binding.observeInitialization(ctx)
		if err != nil {
			return err
		}
		if fresh.Observed.Instance != initialization.Observed.Instance {
			return errDSGACreationIdentity
		}
		return binding.persistProjectReady(ctx, fresh)
	})
}

// 不用legacy waitCompute：该入口明确拒绝DSGA。这里每轮重读lease和固定VM UID，
// 保留完整模板检查；只等待非终态启动，不接管同名替换VM或自动重建Failed实例。
func (binding *dsgaCreationBinding) originalCompute(ctx context.Context) (*dsgaCreationBinding, *vmv1.VirtualMachine, error) {
	current, err := binding.server.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
	if err != nil {
		return nil, nil, err
	}
	if current.vmUID == "" || current.configUID == "" {
		return nil, nil, errDSGACreationIdentity
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		return nil, nil, err
	}
	_, vms, err := newDSGAKubeClients(current.server.kube)
	if err != nil {
		return nil, nil, err
	}
	vm, err := vms.NeonvmV1().VirtualMachines(plan.Namespace).Get(ctx, kubeName(current.intent.EndpointID), metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	wanted, err := renderDSGAVM(current.intent.createPayload, plan)
	if err != nil || current.validateVM(vm) != nil || !sameDSGAVM(vm, wanted) ||
		string(vm.UID) != current.vmUID || !vm.Spec.DSGABootstrap.Ready ||
		vm.Spec.DSGABootstrap.VMUID != current.vmUID || vm.Spec.DSGABootstrap.ConfigSecretUID != current.configUID {
		return nil, nil, errDSGACreationIdentity
	}
	return current, vm, nil
}

func waitDSGAStartupPoll(ctx context.Context) error {
	timer := time.NewTimer(dsgaStartupPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (binding *dsgaCreationBinding) waitOriginalCompute(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		_, vm, err := binding.originalCompute(ctx)
		if err != nil {
			return err
		}
		switch string(vm.Status.Phase) {
		case "Running":
			return nil
		case "Failed", "Succeeded":
			return vmWaitError{phase: string(vm.Status.Phase), cause: errors.New("original VM terminated before readiness")}
		}
		if err := waitDSGAStartupPoll(ctx); err != nil {
			return vmWaitError{phase: string(vm.Status.Phase), cause: err}
		}
	}
}

func (binding *dsgaCreationBinding) waitOriginalInitialization(ctx context.Context) (dsgaInitializationResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		fact, err := binding.observeInitialization(ctx)
		if err == nil || !errors.Is(err, errDSGAInitializationPending) {
			return fact, err
		}
		if err := waitDSGAStartupPoll(ctx); err != nil {
			return dsgaInitializationResponse{}, err
		}
	}
}

// 普通SQL探针只读应用数据库，核真实PG大版本与读写角色；不拿HTTP Running、
// VM大小或capabilities当产品Ready。仅暂时连接失败可有界重试，配置/TLS/认证
// 和版本不匹配立即拒绝。新随机probe种子必须匹配接受时的原UID/完整Data摘要。
func (binding *dsgaCreationBinding) probeOriginalSQL(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		current, vm, err := binding.originalCompute(ctx)
		if err != nil {
			return err
		}
		if vm.Status.Phase != vmv1.VmRunning {
			return errDSGACreationIdentity
		}
		plan, err := current.creationPlan(ctx)
		if err != nil {
			return err
		}
		clients, _, err := newDSGAKubeClients(current.server.kube)
		if err != nil {
			return err
		}
		secret, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, vm.Name+"-credentials", metav1.GetOptions{})
		if err != nil {
			return errDSGACredentialSource
		}
		p := current.intent.createPayload
		source, err := snapshotDSGACredentials(secret, plan.Namespace, vm.Name+"-credentials", p.ProjectID, p.EndpointID)
		if err != nil || source.reference() != plan.Credentials || !dsgaCredentialProvenance(secret, current.operationID, plan) {
			return errDSGACredentialSource
		}
		password, err := source.probePassword()
		if err != nil {
			return err
		}
		if err := current.confirmCredentialSource(ctx, vm, source); err != nil {
			return err
		}
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, 10*time.Second)
		result, err := runSQL(attemptCtx, current.server.proxyHost, current.server.proxyPort, "control_probe", password,
			dsgaApplicationDatabase, selector(p.EndpointID),
			"SELECT current_setting('server_version_num')::integer, pg_is_in_recovery(), current_database(), current_user")
		cancelAttempt()
		if err == nil {
			if !validDSGAProjectSQL(result, plan.PostgresVersion) {
				return errDSGACreationIdentity
			}
			latest, latestVM, err := current.originalCompute(ctx)
			if err != nil {
				return err
			}
			return latest.confirmCredentialSource(ctx, latestVM, source)
		}
		failure := classifySQLFailure(err)
		if !failure.retryable || (failure.class != "connection" && failure.class != "timeout") {
			return err
		}
		if waitErr := waitDSGAStartupPoll(ctx); waitErr != nil {
			return waitErr
		}
	}
}

func validDSGAProjectSQL(result sqlResult, version uint32) bool {
	if !dsgaSupportedPostgresVersion(version) || result.Truncated || len(result.Rows) != 1 || len(result.Rows[0]) != 4 {
		return false
	}
	row := result.Rows[0]
	actual, ok := row[0].(int32)
	return ok && actual/10000 == int32(version) && row[1] == false &&
		row[2] == dsgaApplicationDatabase && row[3] == "control_probe"
}

// 原Operation lease与初始化来源均在事务内重核。失败/COMMIT丢回复可按原意图
// 恢复；不会因再次确认Ready递增版本，也不会改写初始化首次观察、凭据或计划。
func (binding *dsgaCreationBinding) persistProjectReady(ctx context.Context, fact dsgaInitializationResponse) error {
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return err
	}
	tx, err := binding.server.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockDSGAProjectParents(ctx, tx, binding.intent.createPayload); err != nil {
		return err
	}
	if err := binding.lockOperationTx(ctx, tx); err != nil {
		return err
	}
	vm, config, err := binding.read(ctx, tx)
	if err != nil || vm == "" || config == "" || !fact.Observed.AdmissionOpen || fact.validate(binding.intent.createPayload, plan, vm) != nil {
		return errDSGACreationIdentity
	}
	var original bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dsga_creation_initialization_observations
 WHERE operation_id=$1 AND pg_boot_id=$2 AND vm_uid=$3 AND config_uid=$4 AND credentials_uid=$5)
 AND (SELECT count(*) FROM operation_steps WHERE operation_id=$1 AND ordinal BETWEEN 0 AND 6 AND state='succeeded')=7`,
		binding.operationID, fmt.Sprint(uint64(fact.Observed.Instance.PGBootID)), vm, config, plan.Credentials.UID).Scan(&original); err != nil {
		return err
	}
	if !original || binding.action != "create_project_dsga_v1" {
		return errDSGACreationIdentity
	}
	p := binding.intent.createPayload
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE projects SET state='ready',updated_at=clock_timestamp(),version=version+CASE WHEN state='ready' THEN 0 ELSE 1 END WHERE id=$1 AND deleted_at IS NULL`, []any{p.ProjectID}},
		{`UPDATE branches SET state='ready',version=version+CASE WHEN state='ready' THEN 0 ELSE 1 END WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, []any{p.BranchID, p.ProjectID}},
		{`UPDATE endpoints SET state='active',updated_at=clock_timestamp(),version=version+CASE WHEN state='active' THEN 0 ELSE 1 END WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL`, []any{p.EndpointID, p.ProjectID, p.BranchID}},
		{`UPDATE branch_service_instances SET desired_state='active',observed_state='active',last_observed_at=clock_timestamp(),version=version+CASE WHEN observed_state='active' THEN 0 ELSE 1 END WHERE branch_id=$1 AND service_kind='postgres'`, []any{p.BranchID}},
	} {
		tag, err := tx.Exec(ctx, query.sql, query.args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errDSGACreationIdentity
		}
	}
	if err := binding.checkLease(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// 与公开父生命周期准入一致，避免Operation→Project的反向锁序。
// 只在提交资源终态时调用；外部Kube/HTTP/SQL探针永远不在此事务内执行。
func lockDSGAProjectParents(ctx context.Context, tx pgx.Tx, p createPayload) error {
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{`SELECT id FROM projects WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, []any{p.ProjectID}},
		{`SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, []any{p.BranchID, p.ProjectID}},
		{`SELECT id FROM endpoints WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL FOR UPDATE`, []any{p.EndpointID, p.ProjectID, p.BranchID}},
	} {
		var id string
		if err := tx.QueryRow(ctx, query.sql, query.args...).Scan(&id); err != nil {
			return err
		}
	}
	return nil
}

func dsgaProjectCreateSteps() []string {
	return []string{"create_managed_tenant", "create_timeline", "prepare_neonvm_dsga", "wait_neonvm_running", "confirm_native_initialization", "publish_proxy_route", "sql_probe_via_proxy", "persist_ready_state"}
}
