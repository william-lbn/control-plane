package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// 此入口必须每次先读真实原生状态，绝不拿历史表充当当前初始化/准入证明。
// 历史仅保存每个boot首份观察；记录成功也不改变Agent日志、预算或普通SQL准入。
func (binding *dsgaCreationBinding) observeAndRecordInitialization(ctx context.Context) (dsgaInitializationResponse, error) {
	client := newDSGAProxy(nil).observer
	client.Timeout = 35 * time.Second
	defer client.CloseIdleConnections()
	return binding.observeAndRecordInitializationWithHTTP(ctx, client)
}

func (binding *dsgaCreationBinding) observeAndRecordInitializationWithHTTP(ctx context.Context, client *http.Client) (dsgaInitializationResponse, error) {
	fact, err := binding.observeInitializationWithHTTP(ctx, client)
	if err != nil {
		return dsgaInitializationResponse{}, err
	}
	if err := binding.recordInitialization(ctx, fact); err != nil {
		return dsgaInitializationResponse{}, err
	}
	return fact, nil
}

// 私有落库器只接受已由真实观察链核验的typed事实；这里再与完整原计划/持久
// UID逐项核对，防止调用链混入另一次创建。锁序Operation→原绑定→凭据/Service
// →本boot记录；无网络调用。首次COMMIT未知时按同一原Operation/boot恢复，
// 不覆盖旧事实；后续观察H/准入可变化，返回的当前事实仍来自本次HTTP而非旧行。
func (binding *dsgaCreationBinding) recordInitialization(ctx context.Context, fact dsgaInitializationResponse) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return err
	}
	tx, err := binding.lockOperation(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	vm, config, err := binding.read(ctx, tx)
	if err != nil || vm == "" || config == "" || fact.validate(binding.intent.createPayload, plan, vm) != nil {
		return errDSGACreationIdentity
	}
	var credentials, service string
	err = tx.QueryRow(ctx, `SELECT c.credentials_uid,s.service_uid FROM dsga_creation_credential_sources c
 JOIN dsga_creation_services s ON s.operation_id=c.operation_id AND s.service_kind='management'
 WHERE c.operation_id=$1 AND s.vm_uid=$2 FOR SHARE OF c,s`, binding.operationID, vm).Scan(&credentials, &service)
	if err != nil || credentials != plan.Credentials.UID {
		return errDSGACreationIdentity
	}
	boot := strconv.FormatUint(uint64(fact.Observed.Instance.PGBootID), 10)
	var oldVM, oldConfig, oldCredentials, oldService string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT vm_uid,config_uid,credentials_uid,management_service_uid,receipt
 FROM dsga_creation_initialization_observations WHERE operation_id=$1 AND pg_boot_id=$2`, binding.operationID, boot).
		Scan(&oldVM, &oldConfig, &oldCredentials, &oldService, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		raw, err = json.Marshal(fact)
		if err != nil || len(raw) > 64<<10 {
			return errDSGACreationIdentity
		}
		_, err = tx.Exec(ctx, `INSERT INTO dsga_creation_initialization_observations
 (operation_id,pg_boot_id,vm_uid,config_uid,credentials_uid,management_service_uid,receipt)
 VALUES($1,$2,$3,$4,$5,$6,$7)`, binding.operationID, boot, vm, config, credentials, service, raw)
	} else if err == nil {
		// 首份历史与本次当前observed不同是合法的；只验证它仍属同一原启动与
		// 固定UID，绝不UPDATE成较新状态来抹掉首次关闭准入/原boot的审计事实。
		var previous dsgaInitializationResponse
		err = json.Unmarshal(raw, &previous)
		if err != nil || oldVM != vm || oldConfig != config || oldCredentials != credentials || oldService != service ||
			previous.Observed.Instance.PGBootID != fact.Observed.Instance.PGBootID || previous.validate(binding.intent.createPayload, plan, vm) != nil {
			return errDSGACreationIdentity
		}
	}
	if err != nil {
		return err
	}
	if err := binding.checkLease(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
