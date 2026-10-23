package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var errDSGACredentialSource = errors.New("DSGA original immutable credential source is unavailable or differs")

// 只保存Renderer所需的复制值和摘要，不保留Secret指针/seed；调用后原API对象
// 被复用不会改变输出。摘要不含ResourceVersion，正常读回元数据版本不算换凭据。
// TLS证书不放入本immutable来源：其挂载与轮转必须由独立受控身份链处理。
type dsgaCredentialSnapshot struct {
	uid         string
	digest      [32]byte
	credentials dsgaRenderCredentials
}

func (snapshot dsgaCredentialSnapshot) reference() dsgaCredentialReference {
	return dsgaCredentialReference{1, snapshot.uid, hex.EncodeToString(snapshot.digest[:])}
}

func snapshotDSGACredentials(secret *core.Secret, namespace, name, project, endpoint string) (dsgaCredentialSnapshot, error) {
	var empty dsgaCredentialSnapshot
	if !dsgaOwnedSecret(secret, namespace, name, project, endpoint) || dsgapi.ValidateID(string(secret.UID), "credentials Secret") != nil || secret.Immutable == nil || !*secret.Immutable || secret.Type != core.SecretTypeOpaque ||
		len(secret.Data) != 3 || len(secret.Data["controlSigningSeed"]) != ed25519.SeedSize {
		return empty, errDSGACredentialSource
	}
	application, probe := string(secret.Data["applicationVerifier"]), string(secret.Data["probeVerifier"])
	if !functionsql.ValidVerifier(application) || !functionsql.ValidVerifier(probe) {
		return empty, errDSGACredentialSource
	}
	// 精确三键且长度有界，避免额外私有字段被误认为已登记，或对任意大Secret
	// 做序列化/分配。encoding/json稳定排序并编码[]byte，不存在拼接分隔歧义。
	raw, err := json.Marshal(secret.Data)
	if err != nil || len(raw) > 2048 {
		return empty, errDSGACredentialSource
	}
	digest := sha256.Sum256(append([]byte("dsga-creation-credentials-v1\x00"), raw...))
	key := ed25519.NewKeyFromSeed(secret.Data["controlSigningSeed"])
	return dsgaCredentialSnapshot{string(secret.UID), digest, dsgaRenderCredentials{
		ApplicationVerifier: application, ProbeVerifier: probe, ControlPublicKey: key.Public().(ed25519.PublicKey),
	}}, nil
}

// renderDSGABootstrap为真实typed创建执行器的Render回调：读取原计划和当前owned
// immutable凭据，先生成无副作用的确定性文档，再在Operation锁内固定/比对来源。
// Kube/计划读取/渲染均不持SQL锁。输入缺失不创建Secret、不轮转seed、不回落到旧
// adminVerifier/cloud_admin路径。返回仅config.json，签名seed永不进入Guest。
// 调用方仍须用同一binding.bindConfig及UID/RV释放；这里没有预算/SQL/VM许可。
func (binding *dsgaCreationBinding) renderDSGABootstrap(ctx context.Context, vm *vmv1.VirtualMachine) (map[string][]byte, error) {
	if err := binding.validateVM(vm); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return nil, err
	}
	// 原Operation必须已经记录接受时的UID/摘要；不能在第一次worker读取时
	// 收养碰巧同名的Secret。历史无ref的计划不自动升级为生产启动来源。
	if plan.Credentials.validate() != nil {
		return nil, errDSGACredentialSource
	}
	clients, _, err := newDSGAKubeClients(binding.server.kube)
	if err != nil {
		return nil, err
	}
	secret, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, vm.Name+"-credentials", metav1.GetOptions{})
	if err != nil {
		return nil, errDSGACredentialSource
	}
	snapshot, err := snapshotDSGACredentials(secret, plan.Namespace, vm.Name+"-credentials", binding.intent.ProjectID, binding.intent.EndpointID)
	if err != nil {
		return nil, err
	}
	if snapshot.reference() != plan.Credentials || !dsgaCredentialProvenance(secret, binding.operationID, plan) {
		return nil, errDSGACredentialSource
	}
	config, err := renderDSGAComputeBootstrap(binding.intent.createPayload, plan, string(vm.UID), snapshot.credentials)
	if err != nil {
		return nil, err
	}
	if err := binding.confirmCredentialSource(ctx, vm, snapshot); err != nil {
		return nil, err
	}
	return map[string][]byte{"config.json": config}, nil
}

// 锁序固定Operation→绑定→来源；不跨Kube/Secret/渲染持锁。首次固定只限原VM
// held且无执行历史、尚未Config绑定的阶段；ready恢复必须已有同一来源，不补认。
// 不使用binding缓存VM UID或ON CONFLICT覆盖；同源恢复完全只读，xmin/时间不变。
// commit前复核当前worker lease；未知COMMIT交同一原意图重读，不能生成新来源。
func (binding *dsgaCreationBinding) confirmCredentialSource(ctx context.Context, vm *vmv1.VirtualMachine, snapshot dsgaCredentialSnapshot) error {
	tx, err := binding.lockOperation(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	storedVM, storedConfig, err := binding.read(ctx, tx)
	if err != nil || storedVM != string(vm.UID) {
		return errDSGACredentialSource
	}
	var uid string
	var version int
	var digest []byte
	err = tx.QueryRow(ctx, `SELECT source_version,credentials_uid,content_digest FROM dsga_creation_credential_sources WHERE operation_id=$1`, binding.operationID).Scan(&version, &uid, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		held, heldErr := vm.DSGABootstrapHeld()
		if heldErr != nil || !held || !vm.DSGAUndispatched() || storedConfig != "" {
			return errDSGACredentialSource
		}
		_, err = tx.Exec(ctx, `INSERT INTO dsga_creation_credential_sources(operation_id,source_version,credentials_uid,content_digest) VALUES($1,1,$2,$3)`, binding.operationID, snapshot.uid, snapshot.digest[:])
	} else if err == nil && (version != 1 || uid != snapshot.uid || !bytes.Equal(digest, snapshot.digest[:])) {
		return errDSGACredentialSource
	}
	if err != nil {
		return err
	}
	if err := binding.checkLease(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
