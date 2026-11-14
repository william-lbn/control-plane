package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return err
	}
	if plan.RetainCatalog {
		return errDSGACreationIdentity
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
	return s.reconcileDSGACompute(ctx, binding, 2)
}

func (s *server) reconcileDSGAStorageBranch(ctx context.Context, binding *dsgaCreationBinding) error {
	p := binding.intent.createPayload
	if binding.action != "create_branch_dsga_v1" || p.EndpointID != "" || p.ParentBranchID == "" {
		return errDSGACreationIdentity
	}
	if err := s.createStep(ctx, binding.operationID, binding.workerID, 0, "create_timeline", binding.prepareTimeline); err != nil {
		return err
	}
	return s.createStep(ctx, binding.operationID, binding.workerID, 1, "persist_ready_state", binding.persistStorageBranchReady)
}

func (b *dsgaCreationBinding) persistStorageBranchReady(ctx context.Context) error {
	tx, err := b.server.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	p := b.intent.createPayload
	if err = lockDSGAProjectParents(ctx, tx, p); err != nil {
		return err
	}
	if err = b.lockOperationTx(ctx, tx); err != nil {
		return err
	}
	var confirmed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dsga_creation_timelines WHERE operation_id=$1 AND confirmed_at IS NOT NULL)
 AND EXISTS(SELECT 1 FROM operation_steps WHERE operation_id=$1 AND ordinal=0 AND state='succeeded')`, b.operationID).Scan(&confirmed); err != nil {
		return err
	}
	if !confirmed || b.action != "create_branch_dsga_v1" || p.EndpointID != "" {
		return errDSGACreationIdentity
	}
	tag, err := tx.Exec(ctx, `UPDATE branches SET state='ready',version=version+CASE WHEN state='ready' THEN 0 ELSE 1 END
 WHERE id=$1 AND project_id=$2 AND timeline_id=$3 AND deleted_at IS NULL`, p.BranchID, p.ProjectID, p.TimelineID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errDSGACreationIdentity
	}
	tag, err = tx.Exec(ctx, `UPDATE branch_service_instances SET desired_state='suspended',observed_state='suspended',
 version=version+CASE WHEN observed_state='suspended' THEN 0 ELSE 1 END,last_observed_at=clock_timestamp()
 WHERE branch_id=$1 AND service_kind='postgres' AND driver_version='dsga-v1'`, p.BranchID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errDSGACreationIdentity
	}
	if err = b.checkLease(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *server) reconcileDSGACompute(ctx context.Context, binding *dsgaCreationBinding, first int) error {
	step := func(offset int, name string, fn func(context.Context) error) error {
		return s.createStep(ctx, binding.operationID, binding.workerID, first+offset, name, fn)
	}
	if err := step(0, "prepare_neonvm_dsga", func(ctx context.Context) error {
		if _, err := binding.prepareVM(ctx); err != nil {
			return err
		}
		return binding.prepareServices(ctx)
	}); err != nil {
		return err
	}
	if err := step(1, "wait_neonvm_running", binding.waitOriginalCompute); err != nil {
		return err
	}
	var initialization dsgaInitializationResponse
	if err := step(2, "confirm_native_initialization", func(ctx context.Context) error {
		if err := binding.configureGenericCompute(ctx); err != nil {
			return err
		}
		var err error
		initialization, err = binding.waitOriginalInitialization(ctx)
		if err != nil {
			return err
		}
		if err := binding.recordInitialization(ctx, initialization); err != nil {
			return err
		}
		if err := binding.provisionDSGARuntime(ctx, initialization); err != nil {
			return err
		}
		originalInstance := initialization.Observed.Instance
		initialization, err = binding.waitOriginalInitialization(ctx)
		if err != nil {
			return err
		}
		if initialization.Observed.Instance != originalInstance {
			return errDSGACreationIdentity
		}
		return binding.recordInitialization(ctx, initialization)
	}); err != nil {
		return err
	}
	if err := step(3, "publish_proxy_route", func(ctx context.Context) error {
		// 使用本次刚读回的原生事实；不以历史观察行代替当前初始化。
		return binding.publishInitializedSQLRoute(ctx, initialization)
	}); err != nil {
		return err
	}
	if err := step(4, "sql_probe_via_proxy", binding.probeOriginalSQL); err != nil {
		return err
	}
	return step(5, "persist_ready_state", func(ctx context.Context) error {
		fresh, err := binding.observeInitialization(ctx)
		if err != nil {
			return err
		}
		if fresh.Observed.Instance != initialization.Observed.Instance {
			return errDSGACreationIdentity
		}
		return binding.persistComputeReady(ctx, fresh, first+5)
	})
}

func (s *server) reconcileDSGAEndpoint(ctx context.Context, binding *dsgaCreationBinding) error {
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return err
	}
	if binding.action != "create_endpoint_dsga_v1" || !plan.RetainCatalog {
		return errDSGACreationIdentity
	}
	err = s.createStep(ctx, binding.operationID, binding.workerID, 0, "verify_retained_timeline", func(ctx context.Context) error {
		p := binding.intent.createPayload
		var fork dsgaRetainedFork
		if p.EndpointType == "read_write" {
			// Confirm the accepted provenance before allowing any VM to start.
			var raw []byte
			var frozen dsgaEndpointCredentialSource
			if err := s.db.QueryRow(ctx, `SELECT draft->'credential_source' FROM dsga_creation_reservations
 WHERE operation_id=$1 AND accepted_id=$1 AND base_action='create_endpoint'`, binding.operationID).Scan(&raw); err != nil {
				return err
			}
			if decodeDSGAObject(raw, &frozen) != nil || !frozen.valid(p.EndpointID) {
				return errDSGACreationIdentity
			}
			original, err := readDSGARetainedWriter(ctx, s.db, p)
			if err != nil || original != frozen {
				return errDSGACreationIdentity
			}
			fork = frozen.Fork
			if fork == (dsgaRetainedFork{}) {
				// Same-Timeline successors must wait for the old VM and Runner to exit.
				// Only the Worker can read the original Runner Pods.
				if err := s.kube.verifyDSGAPriorWriterAbsent(ctx, plan.Namespace, frozen.Endpoint); err != nil {
					return err
				}
			}
		}
		storage, err := newDSGAStorageClientAccess(ctx, s.kube, plan.Storage, false)
		if err != nil {
			return err
		}
		defer storage.CloseIdleConnections()
		fact, err := storage.request(ctx, http.MethodGet, "control/v1/tenant/"+p.TenantID+"/timeline/"+p.TimelineID, nil)
		if err != nil {
			return err
		}
		return validateDSGATimelineAggregate(fact, p.TenantID, p.TimelineID, plan.PostgresVersion,
			fork.ParentTimeline, fork.LSN, fork != (dsgaRetainedFork{}))
	})
	if err != nil {
		return err
	}
	return s.reconcileDSGACompute(ctx, binding, 1)
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

func dsgaComputeWaitError(err error, lastPhase string) error {
	// Classify the returned cause, not ctx.Err(): a concurrent deadline must
	// not hide an identity, lease or authorization failure. Cancellation is
	// also distinct from exhausting the readiness deadline.
	if errors.Is(err, context.DeadlineExceeded) {
		return vmWaitError{phase: lastPhase, cause: err}
	}
	return err
}

func (binding *dsgaCreationBinding) waitOriginalCompute(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	lastPhase := "" // No VM phase has been observed until originalCompute succeeds.
	for {
		_, vm, err := binding.originalCompute(ctx)
		if err != nil {
			return dsgaComputeWaitError(err, lastPhase)
		}
		lastPhase = string(vm.Status.Phase)
		switch lastPhase {
		case "Running":
			return nil
		case "Failed", "Succeeded":
			return vmWaitError{phase: lastPhase, cause: errors.New("original VM terminated before readiness")}
		}
		if err := waitDSGAStartupPoll(ctx); err != nil {
			return dsgaComputeWaitError(err, lastPhase)
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
		if err != nil || source.reference() != plan.Credentials || !current.verifyCredentialProvenance(ctx, secret, plan) {
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
			if !validDSGAComputeSQL(result, plan.PostgresVersion, p.EndpointType == "read_only") {
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
	return validDSGAComputeSQL(result, version, false)
}

func validDSGAComputeSQL(result sqlResult, version uint32, replica bool) bool {
	if !dsgaSupportedPostgresVersion(version) || result.Truncated || len(result.Rows) != 1 || len(result.Rows[0]) != 4 {
		return false
	}
	row := result.Rows[0]
	actual, ok := row[0].(int32)
	return ok && actual/10000 == int32(version) && row[1] == replica &&
		row[2] == dsgaApplicationDatabase && row[3] == "control_probe"
}

// 原Operation lease与初始化来源均在事务内重核。失败/COMMIT丢回复可按原意图
// 恢复；不会因再次确认Ready递增版本，也不会改写初始化首次观察、凭据或计划。
func (binding *dsgaCreationBinding) persistProjectReady(ctx context.Context, fact dsgaInitializationResponse) error {
	return binding.persistComputeReady(ctx, fact, 7)
}

func (binding *dsgaCreationBinding) persistComputeReady(ctx context.Context, fact dsgaInitializationResponse, previousSteps int) error {
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
 AND (SELECT count(*) FROM operation_steps WHERE operation_id=$1 AND ordinal>=0 AND ordinal<$6 AND state='succeeded')=$6`,
		binding.operationID, fmt.Sprint(uint64(fact.Observed.Instance.PGBootID)), vm, config, plan.Credentials.UID, previousSteps).Scan(&original); err != nil {
		return err
	}
	project := binding.action == "create_project_dsga_v1" && previousSteps == 7 && !plan.RetainCatalog
	endpoint := binding.action == "create_endpoint_dsga_v1" && previousSteps == 6 && plan.RetainCatalog
	resume := binding.action == dsgaResumeAction && previousSteps == 6 && plan.ResumeGeneration
	if !original || (!project && !endpoint && !resume) {
		return errDSGACreationIdentity
	}
	if resume {
		// Promote the active runtime only in the terminal Worker transaction.
		return binding.checkLease(ctx, tx)
	}
	p := binding.intent.createPayload
	type update struct {
		sql  string
		args []any
	}
	updates := []update{}
	if project {
		updates = append(updates,
			update{`UPDATE projects SET state='ready',updated_at=clock_timestamp(),version=version+CASE WHEN state='ready' THEN 0 ELSE 1 END WHERE id=$1 AND deleted_at IS NULL`, []any{p.ProjectID}},
			update{`UPDATE branches SET state='ready',version=version+CASE WHEN state='ready' THEN 0 ELSE 1 END WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, []any{p.BranchID, p.ProjectID}})
	}
	updates = append(updates,
		update{`UPDATE endpoints SET state='active',updated_at=clock_timestamp(),version=version+CASE WHEN state='active' THEN 0 ELSE 1 END WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL`, []any{p.EndpointID, p.ProjectID, p.BranchID}},
		update{`UPDATE branch_service_instances SET desired_state='active',observed_state='active',last_observed_at=clock_timestamp(),version=version+CASE WHEN observed_state='active' THEN 0 ELSE 1 END WHERE branch_id=$1 AND service_kind='postgres' AND driver_version='dsga-v1'`, []any{p.BranchID}},
	)
	for _, query := range updates {
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
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, p.ProjectID).Scan(&id); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, p.BranchID, p.ProjectID).Scan(&id); err != nil {
		return err
	}
	if p.EndpointID == "" {
		return nil
	}
	return tx.QueryRow(ctx, `SELECT id FROM endpoints WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL FOR UPDATE`, p.EndpointID, p.ProjectID, p.BranchID).Scan(&id)
}

func dsgaProjectCreateSteps() []string {
	return append([]string{"create_managed_tenant", "create_timeline"}, dsgaComputeCreateSteps()...)
}

func dsgaComputeCreateSteps() []string {
	return []string{"prepare_neonvm_dsga", "wait_neonvm_running", "confirm_native_initialization", "publish_proxy_route", "sql_probe_via_proxy", "persist_ready_state"}
}

func dsgaEndpointCreateSteps() []string {
	return append([]string{"verify_retained_timeline"}, dsgaComputeCreateSteps()...)
}
